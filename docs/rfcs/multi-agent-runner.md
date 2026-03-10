# RFC: 多 Agent Runner

> **状态**: 草案
> **对应**: ../REQUIREMENTS.md §11.3 "多 agent runner" / Cycle 5 扩展池
> **前置**: Cycle 1-4 首版交付完成

---

## 1. 目标

为 symphony-go 的 `agent.Runner` 接口新增 Claude Code 和 OpenCode 适配器，使编排器可以使用不同的 coding agent 后端执行任务。这是首版 Codex-only agent runner 的第一次多后端扩展。

完成后，用户只需把 `agent.kind` 改为 `claude-code` 或 `opencode` 并提供对应的命令配置，即可切换 coding agent 后端驱动派发。现有 Codex 配置完全兼容，`agent.kind` 默认值为 `codex`。

## 2. 范围

### In Scope

- `agent.kind` 配置字段与 Runner 工厂路由
- `ClaudeCodeRunner` 实现 `agent.Runner`（双向 streaming JSON 子进程协议）
- `OpenCodeRunner` 实现 `agent.Runner`（CLI 子进程 + `--format json`）
- `ArgsProcessFactory`：新增参数数组式进程启动（避免 shell 注入）
- 配置解析与校验（`agent.kind` + 各 agent 专属配置段）
- 事件映射统一契约（各 agent 原生事件 → `AgentEvent`）
- 热重载拒绝策略（`agent.kind` 变更 → 日志告警 + 跳过应用）
- 单元测试（fake process mock）
- 可跳过的集成测试策略

### Out of Scope

- 单 issue 多 agent 并行（同一 issue 同时由多个 agent 处理；需新的冲突解决机制）
- Per-issue / per-state 动态 agent 路由（所有 issue 使用同一 `agent.kind`）
- Aider 适配器（Python 运行时依赖重，首批只做原生可执行 agent）
- 修改 `Runner` 接口签名（现有接口已满足需求）
- 修改 `RunParams` 结构体（prompt 渲染复用现有 `buildTurnPrompt` 模式）

## 3. 配置字段

目录模式下，配置位于 `automation/project.yaml`（或 active profile 覆盖）中的 `runtime.*`：

```yaml
runtime:
  agent:
    kind: codex
    max_concurrent_agents: 2
    max_turns: 10

  codex:
    command: codex app-server
    approval_policy: never
    thread_sandbox: workspace-write
    read_timeout_ms: 15000
    turn_timeout_ms: 600000

  claude_code:
    command: claude
    model: ""
    permission: dangerously-skip-permissions
    max_turns: 0
    read_timeout_ms: 15000

  opencode:
    command: opencode
    model: ""
    agent: ""
    max_turns: 0
```

### 字段说明

| 字段 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `agent.kind` | 否 | `codex` | Agent 后端类型 |
| `claude_code.command` | `kind=claude-code` 时是 | `claude` | Claude Code CLI |
| `claude_code.model` | 否 | — | `--model` 参数值 |
| `claude_code.permission` | 否 | `dangerously-skip-permissions` | 权限策略 |
| `claude_code.max_turns` | 否 | 继承 `agent.max_turns` | Claude Code `--max-turns` |
| `claude_code.read_timeout_ms` | 否 | `15000` | stdin 初始化响应超时 |
| `opencode.command` | `kind=opencode` 时是 | `opencode` | OpenCode CLI |
| `opencode.model` | 否 | — | `--model provider/model` |
| `opencode.agent` | 否 | — | `--agent` 命名 agent |
| `opencode.max_turns` | 否 | 继承 `agent.max_turns` | 最大对话轮次 |

### 默认值策略

- `agent.kind` 未指定时默认为 `codex`，所有现有行为不变
- 各 agent 专属配置段仅在对应 `kind` 生效时被解析和校验
- `codex:` 段默认值保持不变
- 各适配器的 `max_turns` 若为 `0` 或未指定，回退到 `agent.max_turns`

### `claude_code.permission` 字段说明

单字段统一控制 Claude Code 的权限策略，消除布尔值与字符串枚举的组合歧义：

| 值 | CLI 标志 | 说明 |
|---|---|---|
| `dangerously-skip-permissions` | `--dangerously-skip-permissions` | headless 自动化场景的推荐值 |
| `default` | `--permission-mode default` | 每次操作需确认，不适合 headless |
| `acceptEdits` | `--permission-mode acceptEdits` | 自动接受编辑，其他操作仍询问 |
| `plan` | `--permission-mode plan` | 仅规划不执行 |
| `bypassPermissions` | `--permission-mode bypassPermissions` | SDK 模式全部放行 |

当值为 `dangerously-skip-permissions` 时，适配器生成独立标志；其他值统一生成 `--permission-mode <value>` 参数。

### `ServiceConfig` 新增字段

```go
// internal/model/model.go — ServiceConfig 新增
AgentKind string // "codex" | "claude-code" | "opencode"

// Claude Code 专属
ClaudeCodeCommand       string
ClaudeCodeModel         string
ClaudeCodePermission    string // "dangerously-skip-permissions" | "default" | "acceptEdits" | "plan" | "bypassPermissions"
ClaudeCodeMaxTurns      int
ClaudeCodeReadTimeoutMS int

// OpenCode 专属
OpenCodeCommand  string
OpenCodeModel    string
OpenCodeAgent    string
OpenCodeMaxTurns int
```

### Typed Errors 新增

```go
// internal/model/model.go — AgentError 新增
ErrUnsupportedAgentKind = &AgentError{Code: "unsupported_agent_kind"}
ErrMissingAgentCommand  = &AgentError{Code: "missing_agent_command"}
ErrAgentConfigConflict  = &AgentError{Code: "agent_config_conflict"}
```

## 4. 安全进程启动：ArgsProcessFactory

### 问题

当前 `execProcessFactory.StartProcess(ctx, cwd, command)` 将整条命令字符串交给 `bash -lc` 执行（`shell.BashCommand`）。这对 `codex app-server` 是安全的，因为命令固定，不含用户输入。

但 CLI 适配器需要传递 prompt 内容。Prompt 来自 active flow 选中的 Liquid 模板渲染结果，可能包含引号、换行、反引号和 shell 元字符。通过 `bash -lc` 传递这些内容存在 shell 注入风险。

### 解决方案

新增 `ArgsProcessFactory` 接口和默认实现，使用 `exec.Command(name, args...)` 直接启动进程，绕过 shell：

```go
// internal/agent/exec_args.go
type ArgsProcessFactory interface {
    StartProcess(ctx context.Context, cwd string, name string, args []string) (Process, error)
}

type execArgsProcessFactory struct{}

func (execArgsProcessFactory) StartProcess(ctx context.Context, cwd string, name string, args []string) (Process, error) {
    cmd := exec.CommandContext(ctx, name, args...)
    cmd.Dir = cwd
    // ... StdinPipe, StdoutPipe, StderrPipe, Start
}
```

### 关键设计决策

1. `ArgsProcessFactory` 是独立接口，不复用现有 `ProcessFactory`
2. CLI 适配器通过构造器接收 `ArgsProcessFactory`，与现有 `ProcessFactory` 注入模式一致
3. 现有 `ProcessFactory` 和 `execProcessFactory` 不做修改，Codex 继续通过 `bash -lc` 启动
4. 测试时注入 `fakeArgsProcessFactory`

### CLI 适配器的 prompt 传递方式

| Agent | 方式 | 说明 |
|---|---|---|
| Claude Code | stdin JSON 流 | 使用 `--input-format stream-json`，prompt 作为 JSON 消息写入 stdin |
| OpenCode | 外部临时文件 + `--file` | 渲染后的 prompt 写入 repo 外部临时目录，通过 `--file` 引用 |

## 5. Claude Code 适配器

### 5.1 协议概述

Claude Code CLI 支持双向 streaming JSON 子进程协议：

- `--input-format stream-json`：stdin 接收 newline-delimited JSON 消息
- `--output-format stream-json`：stdout 输出 newline-delimited JSON 事件流
- 双向通信保持进程存活，支持同一进程内多轮对话

这与 Codex app-server 的 JSON-RPC 协议在概念上类似：都是通过 stdin/stdout 双向 JSON 通信。关键区别是 Claude Code 不使用 JSON-RPC 信封，而是使用 `type` 字段区分消息类型。

### 5.2 进程启动

适配器使用 `ArgsProcessFactory` 启动 Claude Code 进程，命令行参数为固定标志，不含 prompt：

```go
func (r *ClaudeCodeRunner) startAgent(ctx context.Context, cwd string) (Process, error) {
    cfg := r.configProvider()
    args := []string{
        "-p",
        "--input-format", "stream-json",
        "--output-format", "stream-json",
        "--verbose",
    }

    perm := cfg.ClaudeCodePermission
    if perm == "" {
        perm = "dangerously-skip-permissions"
    }
    if perm == "dangerously-skip-permissions" {
        args = append(args, "--dangerously-skip-permissions")
    } else {
        args = append(args, "--permission-mode", perm)
    }

    maxTurns := r.effectiveMaxTurns(cfg)
    if maxTurns > 0 {
        args = append(args, "--max-turns", strconv.Itoa(maxTurns))
    }

    if cfg.ClaudeCodeModel != "" {
        args = append(args, "--model", cfg.ClaudeCodeModel)
    }

    return r.processFactory.StartProcess(ctx, cwd, cfg.ClaudeCodeCommand, args)
}
```

prompt 通过 stdin 发送。

Claude Code 的 streaming JSON input 格式遵循 Anthropic SDK 的 `type: "user"` message envelope。实现阶段必须通过真实 CLI 交互校正精确 schema。当前初始假设如下：

```go
func buildStdinMessage(prompt string) map[string]any {
    return map[string]any{
        "type": "user",
        "message": map[string]any{
            "role": "user",
            "content": []map[string]any{
                {"type": "text", "text": prompt},
            },
        },
    }
}
```

首次 turn 和 continuation turn 都复用该消息组装逻辑，仅 prompt 文本不同。

### 5.3 事件映射

Claude Code streaming JSON 输出示例：

```json
{"type":"system","subtype":"init","session_id":"sess_abc","tools":[...]}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"..."}]},"session_id":"sess_abc"}
{"type":"result","subtype":"success","duration_ms":1234,"duration_api_ms":800,"num_turns":2,"result":"...","session_id":"sess_abc","total_cost_usd":0.003,"is_error":false}
```

映射表：

| Claude Code 事件 | `AgentEvent.Event` | 说明 |
|---|---|---|
| `type=system, subtype=init` | `session_started` | 提取 `session_id`，设置 PID |
| `type=assistant` | `notification` | 中间输出 |
| `type=result, subtype=success` | `turn_completed` | 成功完成；若存在未文档化 usage 字段则一并提取 |
| `type=result, subtype=error` 或 `is_error=true` | `turn_failed` | 提取错误信息 |
| 进程非零退出（无 `result` 事件） | `turn_failed` | 进程级失败 |
| stdout 非 JSON 行 | 静默忽略 | 诊断输出混入 |

### 5.4 Token 计量

Claude Code 官方文档当前未把 token usage 列为稳定的 `result` schema 字段，因此适配器采用 best-effort 策略：

1. 尝试从 `result.usage.input_tokens` / `result.usage.output_tokens` 提取
2. 若不存在，则 `TokenUsage` 全部置零
3. `applyUsageLocked` 对零 delta 无操作，不影响全局聚合

`usage` 不是本 RFC 认定的稳定契约字段，若实际 CLI 输出不包含该字段，实现应以零值继续运行。`§5.3` 中的 `result` 示例和映射不得把 `usage` 视为稳定主路径。

### 5.5 Multi-turn 语义

| 维度 | Codex app-server | Claude Code |
|---|---|---|
| 进程生命周期 | 单进程跨 turn | 单进程跨 turn |
| Turn 触发 | 发送 `turn/start` JSON-RPC | 通过 stdin 写入 `type=user` 消息 |
| Turn 边界 | `turn/completed` / `turn/failed` | `type=result` 事件 |
| Session 持续 | 同一 `thread_id` | 同一进程 |
| Approval | stdin 协议交互 | `--permission-mode` / `--dangerously-skip-permissions` |

Multi-turn 循环：

1. 启动 Claude Code 进程
2. `for turn := 1; turn <= maxTurns; turn++ {`
3. `buildTurnPrompt` → 写入 stdin JSON
4. 流式读取 stdout JSON 事件
5. 收到 `type=result` → 当前 turn 结束
6. 若成功则 `refetchIssue`，仍活跃则继续
7. 失败则返回错误
8. 循环结束后关闭 stdin，等待进程退出

### 5.6 Session ID

- 从 `type=system, subtype=init` 事件提取 `session_id`
- 格式保持 agent 原样
- 可通过前缀区分来源

## 6. OpenCode 适配器

### 6.1 协议概述

OpenCode 使用 CLI `run` 子命令的非交互模式：

```bash
opencode run [message..] --format json
```

`--format json` 输出 newline-delimited JSON 事件流。与 Claude Code 不同，OpenCode `run` 是单次调用，进程执行完毕即退出。Multi-turn 通过 `--continue` 或 `--session` 续接上一次会话。

### 6.2 进程启动

每个 turn 启动一个独立的 `opencode run` 进程。Prompt 通过 repo 外部临时文件传递，避免写入 workspace 工作树：

```go
func (r *OpenCodeRunner) startTurn(ctx context.Context, cwd string, promptFile string, sessionID string, turn int) (Process, error) {
    cfg := r.configProvider()
    args := []string{"run", "--format", "json"}

    if cfg.OpenCodeModel != "" {
        args = append(args, "--model", cfg.OpenCodeModel)
    }
    if cfg.OpenCodeAgent != "" {
        args = append(args, "--agent", cfg.OpenCodeAgent)
    }
    if turn > 1 && sessionID != "" {
        args = append(args, "--session", sessionID, "--continue")
    }

    args = append(args, "--file", promptFile, "Execute the task described in the attached file")
    return r.processFactory.StartProcess(ctx, cwd, cfg.OpenCodeCommand, args)
}
```

Prompt 文件管理：

```go
func writeTurnPrompt(prompt string) (string, error) {
    dir, err := os.MkdirTemp("", "symphony-opencode-*")
    if err != nil {
        return "", err
    }
    path := filepath.Join(dir, "prompt.md")
    if err := os.WriteFile(path, []byte(prompt), 0o644); err != nil {
        return "", err
    }
    return path, nil
}
```

- Prompt 文件写入 repo 外部的临时目录
- 每次 turn 覆盖新建
- `Run` 返回前清理临时文件和临时目录
- 不依赖目标仓库 `.gitignore` 保证安全，因为文件不进入工作树

### 6.3 事件映射

OpenCode `--format json` 输出 schema 在官方文档中未完整定义。实现阶段须以真实 CLI 输出为准，以下仅为初始映射策略：

| 事件特征 | `AgentEvent.Event` | 说明 |
|---|---|---|
| 首条消息（含 session 元信息） | `session_started` | 提取 session ID |
| 中间消息（assistant/tool 内容） | `notification` | 信息性 |
| 最终结果消息 | `turn_completed` | 提取 token 用量（若有） |
| 错误消息 | `turn_failed` | 提取错误信息 |
| 进程非零退出 | `turn_failed` | 进程级失败 |
| 非 JSON 行 | 静默忽略 | 诊断输出混入 |

OpenCode 适配器在本 RFC 中标注为 experimental，实现阶段须通过集成测试固定快照。

### 6.4 Token 计量

- 若 JSON 事件中存在 token 用量，则按实际字段提取
- 若不存在，则 `TokenUsage` 置零
- 零值不会破坏 orchestrator 聚合

### 6.5 Multi-turn 语义

| 维度 | Codex app-server | OpenCode |
|---|---|---|
| 进程生命周期 | 单进程跨 turn | 每 turn 一个进程 |
| Turn 触发 | 发送 `turn/start` JSON-RPC | 启动新 `opencode run` |
| Turn 边界 | `turn/completed` | 进程退出 |
| Session 持续 | 同一 `thread_id` | `--session` + `--continue` |
| Fork | 无 | `--fork` 可选 |

Multi-turn 循环：

1. `var sessionID string`
2. 每轮 `buildTurnPrompt`
3. 写入临时 prompt 文件
4. 启动 `opencode run`
5. 读取 stdout JSON 事件
6. 提取 `sessionID`
7. 成功则 `refetchIssue`，仍活跃则继续
8. 失败则返回错误
9. 最终清理临时文件

## 7. 事件映射统一契约

### 7.1 必须发出的事件

| 事件 | 时机 | 必带字段 |
|---|---|---|
| `session_started` | 会话初始化成功 | `CodexAppServerPID`, `Timestamp` |
| `turn_completed` | turn 成功完成 | `Usage`（可全零）, `Timestamp` |
| `turn_failed` | turn 失败 | `Message`, `Timestamp` |

### 7.2 可选事件

| 事件 | 时机 |
|---|---|
| `notification` | 中间输出 |
| `approval_auto_approved` | 仅 Codex |
| `unsupported_tool_call` | 仅 Codex |
| `turn_input_required` | 仅 Codex，硬失败 |

### 7.3 适配器实现检查清单

1. 首次初始化成功时发出 `session_started`
2. 每个 turn 成功结束时发出 `turn_completed`
3. 每个 turn 失败时发出 `turn_failed`
4. 设置 `AgentEvent.CodexAppServerPID` 为进程 PID
5. 所有 `Timestamp` 使用 `r.now()`
6. token 用量使用绝对累计值，兼容 `applyUsageLocked` 的 delta 检测

### 7.4 prompt 渲染复用

所有适配器复用现有 `buildTurnPrompt(params, issue, turnNumber, maxTurns)`：

- Turn 1：调用 `workflow.RenderPrompt`
- Turn 2+：返回 continuation 指导字符串

`RunParams` 结构体不做修改。

## 8. 热重载拒绝策略

### 问题

当前 runner 在启动时创建一次，配置热重载不会重建 runner。若运行中将 `agent.kind` 从 `codex` 改为 `claude-code`，系统会继续使用旧 runner。

### 解决方案

继续复用现有 `runtimeState.ApplyReload` 作为唯一 reload gate，顺序保持一致：

1. `config.NewFromWorkflow`
2. 重新应用 CLI override（例如 `--port`）
3. `ValidateForDispatch`
4. 检测 restart-required 字段
5. 仅在全部通过后原子替换 last known good

在第 4 步中检测 `agent.kind` 变更：

```go
if s.config.AgentKind != newCfg.AgentKind {
    return nil, fmt.Errorf("agent.kind changed from %q to %q: restart required", s.config.AgentKind, newCfg.AgentKind)
}
```

### Restart-required 字段清单

| 字段 | 原因 |
|---|---|
| `agent.kind` | Runner 实例类型在启动时确定，无法热切换 |

`*.command` 不需要重启。现有 Codex runner 在每次 `Run` 调用时重读 `configProvider()` 获取命令；CLI 适配器沿用同一模式。

## 9. Runner 工厂路由

### 当前架构

```go
func NewRunner(configProvider func() *model.ServiceConfig, logger *slog.Logger, processFactory ProcessFactory) Runner {
    return &AppServerRunner{...}
}
```

### 目标架构

```go
func NewRunner(configProvider func() *model.ServiceConfig, logger *slog.Logger, processFactory ProcessFactory) (Runner, error) {
    cfg := configProvider()
    kind := cfg.AgentKind
    if kind == "" {
        kind = "codex"
    }
    switch kind {
    case "codex":
        return newAppServerRunner(configProvider, logger, processFactory), nil
    case "claude-code":
        return newClaudeCodeRunner(configProvider, logger, nil), nil
    case "opencode":
        return newOpenCodeRunner(configProvider, logger, nil), nil
    default:
        return nil, model.NewAgentError(model.ErrUnsupportedAgentKind, fmt.Sprintf("unsupported agent.kind: %q", kind), nil)
    }
}
```

### 设计决策

1. `NewRunner` 新增 `error` 返回值
2. `AppServerRunner` 构造器改为内部 `newAppServerRunner`
3. Claude/OpenCode 接收可选 `ArgsProcessFactory`
4. Codex 路径保持不变

### `main.go` 调用变更

本 RFC 复用统一的 `main.go` seam 基线，不改 `runCLI/execute` 入口命名；只扩展既有的 runner factory 注入点。

```go
newAgentRunnerFactory = func(configFn func() *model.ServiceConfig, logger *slog.Logger) (agent.Runner, error) {
    return agent.NewRunner(configFn, logger, nil)
}

runner, err := newAgentRunnerFactory(state.CurrentConfig, logger)
if err != nil {
    return err
}
```

## 10. 接口变化

### `agent.Runner`（不变）

```go
type Runner interface {
    Run(ctx context.Context, params RunParams) error
}
```

### `RunParams`（不变）

```go
type RunParams struct {
    Issue          *model.Issue
    Attempt        *int
    WorkspacePath  string
    PromptTemplate string
    MaxTurns       int
    RefetchIssue   func(context.Context, string) (*model.Issue, error)
    IsActive       func(string) bool
    OnEvent        func(AgentEvent)
}
```

### `ProcessFactory`（不变）

```go
type ProcessFactory interface {
    StartProcess(ctx context.Context, cwd string, command string) (Process, error)
}
```

仅 Codex 使用。CLI 适配器使用新增的 `ArgsProcessFactory`。

### `ValidateForDispatch` 扩展

```go
func ValidateForDispatch(cfg *model.ServiceConfig) error {
    switch cfg.AgentKind {
    case "codex", "":
        // 现有验证
    case "claude-code":
        if cfg.ClaudeCodeCommand == "" {
            return model.NewAgentError(model.ErrMissingAgentCommand, "claude_code.command is required", nil)
        }
        validPerms := map[string]bool{
            "dangerously-skip-permissions": true,
            "default": true,
            "acceptEdits": true,
            "plan": true,
            "bypassPermissions": true,
            "": true,
        }
        if !validPerms[cfg.ClaudeCodePermission] {
            return model.NewAgentError(model.ErrAgentConfigConflict, fmt.Sprintf("claude_code.permission %q is not a valid value", cfg.ClaudeCodePermission), nil)
        }
    case "opencode":
        if cfg.OpenCodeCommand == "" {
            return model.NewAgentError(model.ErrMissingAgentCommand, "opencode.command is required", nil)
        }
    default:
        return model.NewAgentError(model.ErrUnsupportedAgentKind, "unsupported agent.kind", nil)
    }
    return nil
}
```

## 11. 与现有模块兼容性

| 模块 | 影响 | 说明 |
|---|---|---|
| `model` | 兼容 | 新增 `AgentKind` 等 `ServiceConfig` 字段 + 3 个 typed errors |
| `config` | 兼容 | 新增 `agent.kind` 解析 + 各 agent 段落解析 |
| `orchestrator` | 无改动 | 继续依赖 `agent.Runner` 接口 |
| `agent` | 扩展 | `NewRunner` 签名新增 error；新增 CLI 适配器 |
| `server` | 无改动 | Snapshot 与 agent 类型无关 |
| `workspace` | 无改动 | 工作目录管理与 agent 无关 |
| `workflow` | 无改动 | Liquid 模板渲染不变 |
| `logging` | 无改动 | 秘密过滤继续生效 |
| `shell` | 无改动 | 仅 Codex 适配器继续使用 |
| `tracker` | 无改动 | 完全解耦 |
| `cmd/symphony` | 修改 | runner 工厂签名变更 + 共享 `ApplyReload` gate 中的 restart-required 检测 |

Core Conformance 不受影响，新代码仅在 `agent.kind != codex` 时激活。

## 12. 测试计划

### 单元测试

| 测试 | 文件 | 覆盖点 |
|---|---|---|
| `TestNewRunner_Codex` | `runner_test.go` | 默认 kind 和 codex 返回正确类型 |
| `TestNewRunner_ClaudeCode` | `runner_test.go` | `claude-code` 返回 ClaudeCodeRunner |
| `TestNewRunner_OpenCode` | `runner_test.go` | `opencode` 返回 OpenCodeRunner |
| `TestNewRunner_UnsupportedKind` | `runner_test.go` | 返回 `ErrUnsupportedAgentKind` |
| `TestClaudeCode_ArgsAssembly` | `claude_code_test.go` | 参数组装 |
| `TestClaudeCode_StdinPrompt` | `claude_code_test.go` | prompt 通过 stdin 发送 |
| `TestClaudeCode_EventMapping` | `claude_code_test.go` | stdout JSON → `AgentEvent` |
| `TestClaudeCode_TokenUsage` | `claude_code_test.go` | usage best-effort 提取 |
| `TestClaudeCode_MultiTurn` | `claude_code_test.go` | 同一进程内多 turn |
| `TestClaudeCode_ProcessFailure` | `claude_code_test.go` | 非零退出码 |
| `TestClaudeCode_MalformedJSON` | `claude_code_test.go` | 非 JSON stdout 行容错 |
| `TestOpenCode_ArgsAssembly` | `opencode_test.go` | 参数组装 |
| `TestOpenCode_PromptFile` | `opencode_test.go` | prompt 写入临时文件 |
| `TestOpenCode_EventMapping` | `opencode_test.go` | JSON 事件映射 |
| `TestOpenCode_MultiTurn` | `opencode_test.go` | 多进程 turn |
| `TestConfig_AgentKindValidation` | `config_test.go` | kind 校验 |
| `TestConfig_ClaudeCodePermValues` | `config_test.go` | permission 枚举值校验 |
| `TestApplyReload_AgentKindReject` | `main_test.go` | `ApplyReload` 在校验通过后拒绝 `agent.kind` 变更 |
| `TestArgsProcessFactory` | `exec_args_test.go` | args 数组正确传递 |

### 集成测试（可跳过）

```go
func TestClaudeCodeIntegration(t *testing.T) {
    if os.Getenv("SYMPHONY_CLAUDE_CODE_INTEGRATION") != "1" {
        t.Skip("set SYMPHONY_CLAUDE_CODE_INTEGRATION=1 and install claude CLI")
    }
}

func TestOpenCodeIntegration(t *testing.T) {
    if os.Getenv("SYMPHONY_OPENCODE_INTEGRATION") != "1" {
        t.Skip("set SYMPHONY_OPENCODE_INTEGRATION=1 and install opencode CLI")
    }
}
```

### Core Conformance 回归

`go test ./...` 全部通过。`NewRunner` 签名变更需同步更新 `main_test.go` 和 `runner_test.go` 中的 helper。

## 13. 运维影响

| 项目 | 说明 |
|---|---|
| 新增凭证 | `claude-code` 需 `ANTHROPIC_API_KEY`；`opencode` 需对应 API key |
| 端口 | 无新增 |
| 资源消耗 | 与 Codex 相近，各自消耗一个子进程 |
| 依赖变化 | 无新增三方 Go 依赖 |
| 运行时依赖 | 需安装对应 CLI |
| 监控项 | `/api/v1/state` 可通过 `session_id` 区分 agent |
| 热重载限制 | 仅 `agent.kind` 变更需重启；`*.command` 等运行期读取字段可热更新 |

### 配套文档落点

- `docs/operator-runbook.md`：补充安装前置、凭证配置、常见故障
- `automation/project.yaml`：补充 `runtime.agent.kind` 与各后端配置说明

## 14. 风险与回滚

| 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|
| Claude Code 输出格式变更 | 中 | 事件解析失败 | 宽松 JSON 解析 + 版本说明 |
| OpenCode `--format json` schema 未文档化 | 高 | 适配器频繁调整 | experimental + 集成测试快照 |
| Claude stdin `stream-json` 协议细节不稳定 | 中 | 多 turn 行为偏差 | 集成测试覆盖；必要时降级为每 turn 重启 |
| `NewRunner` 签名变更导致编译失败 | 低 | `main.go`/tests 需适配 | 测试覆盖 |
| Windows 下 agent CLI 路径解析 | 低 | agent 找不到 | 直接 `exec.Command`，依赖 PATH |

### 回滚方式

1. 将 `agent.kind` 改回 `codex`（或删除该字段）
2. 新适配器代码仅在对应 kind 分支中激活
3. 如需彻底移除：删除 `claude_code.go`、`opencode.go`、`exec_args.go` 及测试，还原 `NewRunner` 签名

## 附录：文件改动清单

### 新建文件

| 文件 | 说明 |
|---|---|
| `docs/rfcs/multi-agent-runner.md` | 本 RFC |
| `internal/agent/claude_code.go` | ClaudeCodeRunner 实现 |
| `internal/agent/claude_code_test.go` | 单元测试 + 可跳过集成测试 |
| `internal/agent/opencode.go` | OpenCodeRunner 实现 |
| `internal/agent/opencode_test.go` | 单元测试 + 可跳过集成测试 |
| `internal/agent/exec_args.go` | `ArgsProcessFactory` 默认实现 |
| `internal/agent/exec_args_test.go` | 参数传递安全性测试 |

### 修改文件

| 文件 | 改动类型 | 说明 |
|---|---|---|
| `internal/model/model.go` | 修改 | `ServiceConfig` 新增 agent 字段 + 3 个 typed errors |
| `internal/config/config.go` | 修改 | 解析 `agent.kind` + 各 agent 段落 + `ValidateForDispatch` 扩展 |
| `internal/config/config_test.go` | 修改 | 新增 kind 校验 + permission 枚举校验 |
| `internal/agent/runner.go` | 修改 | `NewRunner` 新增 error + 工厂路由；`buildTurnPrompt` 导出或提取 |
| `internal/agent/runner_test.go` | 修改 | 新增工厂路由测试；现有 helper 适配新签名 |
| `cmd/symphony/main.go` | 修改 | runner 工厂签名变更 + `ApplyReload` 检测 |
| `cmd/symphony/main_test.go` | 修改 | stub 适配新签名 + reload 拒绝测试 |
| `automation/project.yaml` | 修改 | 补充 `runtime.agent.kind` 配置说明 |
| `docs/operator-runbook.md` | 修改 | 补充各 agent 安装与凭证说明 |
| `docs/cycles/cycle-05-post-mvp.md` | 微调 | “多 agent runner” 补 RFC 链接 |
