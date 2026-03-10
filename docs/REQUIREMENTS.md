# Symphony-Go 需求文档

> 基于 SPEC.md (Draft v1) | Go 实现 | 2026-03-07

---

## 目录

1. [项目概述与 Go 实现定位](#1-项目概述与-go-实现定位)
2. [项目结构与模块划分](#2-项目结构与模块划分)
3. [技术选型](#3-技术选型)
4. [核心领域模型](#4-核心领域模型)
5. [模块需求详述](#5-模块需求详述)
6. [CLI 入口](#6-cli-入口)
7. [并发模型与 goroutine 架构](#7-并发模型与-goroutine-架构)
8. [错误处理策略](#8-错误处理策略)
9. [测试策略与验证标准](#9-测试策略与验证标准)
10. [实现路线图](#10-实现路线图)
11. [竞品对比与扩展路线图](#11-竞品对比与扩展路线图)

---

## 1. 项目概述与 Go 实现定位

### 1.1 问题域

Symphony 是一个长运行自动化服务，持续从 issue tracker（Linear）读取工作，为每个 issue 创建隔离工作区，并在工作区内运行 coding agent 会话。（参见 SPEC §1）

它解决四个操作问题：

- 将 issue 执行转为可重复的守护进程工作流
- 在隔离的 per-issue 工作区中执行 agent 命令
- 将工作流策略保存在仓库内（`WORKFLOW.md`），团队可版本化管理
- 提供足够的可观测性来操作和调试多个并发 agent 运行

**关键边界**：Symphony 是调度器/运行器和 tracker 读取器。Ticket 写入（状态转换、评论、PR 链接）由 coding agent 通过工作流环境中的工具执行。

### 1.2 Go 实现信任边界

本实现面向**受信环境（high-trust）**：

- 自动批准命令执行审批
- 自动批准文件变更审批
- 用户输入请求（user-input-required）视为**硬失败**，立即终止运行尝试

实现需明确记录此信任姿态。（参见 SPEC §15.1）

### 1.3 Go 版本

- **Go 1.23+**
- 利用：`log/slog` 结构化日志（1.21+）、`net/http` 路由模式匹配（1.22+）、增强的标准库特性

### 1.4 不做的事项（对齐 SPEC §2.2 Non-Goals）

- 不做 Web UI 或多租户控制面
- 不做通用工作流引擎或分布式作业调度器
- 不做内置 ticket/PR/comment 编辑的业务逻辑
- 不强制要求超出 coding agent 和主机 OS 提供的沙箱控制
- 不规定特定的 dashboard 或 terminal UI 实现

---

## 2. 项目结构与模块划分

### 2.1 目录布局

```
symphony-go/
├── cmd/
│   └── symphony/           # CLI 入口
│       └── main.go
├── internal/
│   ├── model/              # 共享领域模型（纯数据结构，无业务逻辑）
│   ├── workflow/            # Workflow Loader + 模板渲染
│   ├── config/              # Config Layer（类型化配置 + 校验）
│   ├── tracker/             # Issue Tracker Client（Linear 适配器）
│   ├── orchestrator/        # Orchestrator（状态机 + 调度 + 重试 + 对账）
│   ├── workspace/           # Workspace Manager（路径管理 + 钩子）
│   ├── agent/               # Agent Runner（app-server 子进程协议客户端）
│   ├── server/              # HTTP Server（可选扩展）
│   └── logging/             # 日志配置辅助
├── go.mod
├── go.sum
├── docs/
│   ├── SPEC.md
│   ├── REQUIREMENTS.md
│   ├── IMPLEMENTATION.md
│   └── FLOW.md
└── WORKFLOW.md              # 示例/测试用
```

### 2.2 包依赖方向

```
cmd/symphony
  └── internal/orchestrator
        ├── internal/tracker    (interface)
        ├── internal/workspace  (interface)
        ├── internal/agent      (interface)
        ├── internal/config
        └── internal/workflow
      internal/server
        └── internal/orchestrator (只读快照)
      internal/model            (所有包共享)
```

- `model` 包只放纯数据结构（struct + 枚举常量），**无业务逻辑**
- `orchestrator` 通过 **interface** 依赖 `tracker`、`workspace`、`agent`，便于测试 mock
- **扩展性设计**：`tracker.Client` 和 `agent.Runner` 接口应设计为可扩展点，便于未来添加 GitHub Issues tracker 或 OpenCode/Claude Code 等 agent runner（竞品 contrabass 和 agent-orchestrator 均支持多 tracker/agent）
- `internal/` 下所有包不对外暴露

---

## 3. 技术选型

| 领域 | 选型 | 理由 | SPEC 需求映射 |
|---|---|---|---|
| YAML 解析 | `gopkg.in/yaml.v3` | 成熟稳定，支持 map 解码 | §5.2 front matter 解析 |
| 模板引擎 | `github.com/osteele/liquid` | Liquid 兼容语义；与官方 Elixir 实现模板语法一致 | §5.4 严格模板渲染 |
| GraphQL 客户端 | 手写 HTTP + `encoding/json` | 查询字段可控，无代码生成维护负担 | §11.2 Linear GraphQL |
| HTTP 框架 | `net/http`（Go 1.22+ 路由模式匹配） | 仅 4 端点，标准库足够 | §13.7 可选 HTTP Server |
| 结构化日志 | `log/slog` | 标准库，直接支持 key=value | §13.1 结构化日志 |
| 文件监控 | `github.com/fsnotify/fsnotify` | 跨平台事实标准 | §6.2 动态配置热加载 |
| 进程管理 | `os/exec` | 标准库，`bash -lc` 子进程 + stdio pipe | §10.1 Agent 启动 |
| JSON 行协议 | `bufio.Scanner` + `encoding/json` | 逐行读 stdout 后 JSON 解析 | §10.3 流式 turn 处理 |
| 定时器/调度 | `time.Timer` / `time.AfterFunc` | 标准库，配合 channel 实现轮询 | §8.1 轮询循环 |
| 并发控制 | goroutine + channel | event loop 模式，状态变更序列化 | §7.4 幂等性/恢复 |
| 测试框架 | `testing` + `github.com/stretchr/testify` | 断言辅助，不引入重型框架 | §17 测试矩阵 |

**模板引擎说明**：SPEC 要求 "Liquid-compatible semantics"（§5.4）。采用 `github.com/osteele/liquid`（纯 Go Liquid 实现），与官方 Elixir 参考实现和 Go 竞品 contrabass 的模板语法完全兼容（`{{ issue.title }}`、`{% if attempt %}`）。这意味着可直接复用官方 WORKFLOW.md 模板，无需改写语法。该库支持严格模式（未知变量/过滤器报错）。

---

## 4. 核心领域模型

所有类型定义在 `internal/model/` 包中。以下为每个实体的 Go struct 映射。（参见 SPEC §4.1）

### 4.1 Issue（SPEC §4.1.1）

```go
type Issue struct {
    ID          string       // 稳定的 tracker 内部 ID
    Identifier  string       // 人类可读 ticket key（如 "ABC-123"）
    Title       string
    Description *string      // nullable
    Priority    *int         // nullable；数字越小优先级越高
    State       string       // 当前 tracker 状态名
    BranchName  *string      // nullable
    URL         *string      // nullable
    Labels      []string     // 归一化为小写
    BlockedBy   []BlockerRef
    CreatedAt   *time.Time   // nullable
    UpdatedAt   *time.Time   // nullable
}

type BlockerRef struct {
    ID         *string // nullable
    Identifier *string // nullable
    State      *string // nullable
}
```

### 4.2 WorkflowDefinition（SPEC §4.1.2）

```go
type WorkflowDefinition struct {
    Config         map[string]any // YAML front matter 根对象
    PromptTemplate string         // front matter 后的 Markdown body（已 trim）
}
```

### 4.3 ServiceConfig（SPEC §4.1.3 + §6.4）

```go
type ServiceConfig struct {
    // tracker
    TrackerKind       string   // 必填，当前仅 "linear"
    TrackerEndpoint   string   // 默认 "https://api.linear.app/graphql"
    TrackerAPIKey     string   // 支持 $VAR 间接引用
    TrackerProjectSlug string  // tracker.kind=linear 时必填
    TrackerLinearChildrenBlockParent bool // 默认 true；Linear 父任务被未终态子任务阻塞
    ActiveStates      []string // 默认 ["Todo", "In Progress"]
    TerminalStates    []string // 默认 ["Closed", "Cancelled", "Canceled", "Duplicate", "Done"]

    // polling
    PollIntervalMS int // 默认 30000

    // workspace
    WorkspaceRoot string // 默认 <os.TempDir()>/symphony_workspaces

    // hooks
    HookAfterCreate  *string // nullable shell 脚本
    HookBeforeRun    *string
    HookAfterRun     *string
    HookBeforeRemove *string
    HookTimeoutMS    int     // 默认 60000

    // agent
    MaxConcurrentAgents       int            // 默认 10
    MaxTurns                  int            // 默认 20
    MaxRetryBackoffMS         int            // 默认 300000（5 分钟）
    MaxConcurrentAgentsByState map[string]int // 默认 {}，状态键归一化（trim+lowercase）

    // codex
    CodexCommand          string // 默认 "codex app-server"
    CodexApprovalPolicy   string // 默认 "never"（对齐官方 Elixir）
    CodexThreadSandbox    string // 默认 "workspace-write"
    CodexTurnSandboxPolicy string // 默认 "{type: workspaceWrite}"
    CodexTurnTimeoutMS    int    // 默认 3600000（1 小时）
    CodexReadTimeoutMS    int    // 默认 5000
    CodexStallTimeoutMS   int    // 默认 300000（5 分钟）；<= 0 禁用停滞检测

    // server（可选扩展）
    ServerPort *int // nullable；正整数绑定该端口，0 用于临时端口
}
```

### 4.4 Workspace（SPEC §4.1.4）

```go
type Workspace struct {
    Path         string // 绝对工作区路径
    WorkspaceKey string // 消毒后的 issue identifier
    CreatedNow   bool   // 本次调用是否新创建
}
```

### 4.5 RunAttempt（SPEC §4.1.5）

```go
type RunAttempt struct {
    IssueID         string
    IssueIdentifier string
    Attempt         *int       // nil=首次运行，>=1 为重试/continuation
    WorkspacePath   string
    StartedAt       time.Time
    Status          RunPhase
    Error           *string    // nullable
}
```

### 4.6 LiveSession（SPEC §4.1.6）

```go
type LiveSession struct {
    SessionID             string    // "<thread_id>-<turn_id>"
    ThreadID              string
    TurnID                string
    CodexAppServerPID     *string   // nullable
    LastCodexEvent        *string   // nullable
    LastCodexTimestamp     *time.Time
    LastCodexMessage      string
    CodexInputTokens      int64
    CodexOutputTokens     int64
    CodexTotalTokens      int64
    LastReportedInputTokens  int64
    LastReportedOutputTokens int64
    LastReportedTotalTokens  int64
    TurnCount             int       // 当前 worker 生命周期内启动的 turn 数
}
```

### 4.7 RetryEntry（SPEC §4.1.7）

```go
type RetryEntry struct {
    IssueID     string
    Identifier  string       // 人类可读 ID，用于日志/状态面
    Attempt     int          // 1-based
    DueAt       time.Time    // 到期时间
    TimerHandle *time.Timer  // 运行时定时器引用
    Error       *string      // nullable
}
```

### 4.8 OrchestratorState（SPEC §4.1.8）

```go
type OrchestratorState struct {
    PollIntervalMS      int
    MaxConcurrentAgents int
    Running             map[string]*RunningEntry   // key=issue_id
    Claimed             map[string]struct{}         // issue_id 集合
    RetryAttempts       map[string]*RetryEntry     // key=issue_id
    Completed           map[string]struct{}         // 记账用，非调度门控
    CodexTotals         TokenTotals
    CodexRateLimits     any                        // 最新速率限制快照
}

type RunningEntry struct {
    Issue                *Issue
    Identifier           string
    Session              LiveSession
    RetryAttempt         int
    StartedAt            time.Time
    WorkerCancel         context.CancelFunc         // 终止 worker goroutine
}

type TokenTotals struct {
    InputTokens    int64
    OutputTokens   int64
    TotalTokens    int64
    SecondsRunning float64
}
```

### 4.9 枚举常量

```go
// Issue 编排状态（SPEC §7.1）
type OrchState int
const (
    OrchUnclaimed OrchState = iota
    OrchClaimed
    OrchRunning
    OrchRetryQueued
    OrchReleased
)

// 运行阶段（SPEC §7.2）
type RunPhase int
const (
    PhasePreparingWorkspace RunPhase = iota
    PhaseBuildingPrompt
    PhaseLaunchingAgent
    PhaseInitializingSession
    PhaseStreamingTurn
    PhaseFinishing
    PhaseSucceeded
    PhaseFailed
    PhaseTimedOut
    PhaseStalled
    PhaseCanceledByReconciliation
)
```

### 4.10 标识符归一化规则（SPEC §4.2）

| 标识符 | 用途 | 规则 |
|---|---|---|
| Issue ID | tracker 查找 + 内部 map key | 原始值 |
| Issue Identifier | 日志 + 工作区命名 | 原始值 |
| Workspace Key | 工作区目录名 | 替换 `[^A-Za-z0-9._-]` 为 `_` |
| Normalized State | 状态比较 | `strings.TrimSpace` + `strings.ToLower` |
| Session ID | 日志 | `<thread_id>-<turn_id>` |

---

## 5. 模块需求详述

### 5.1 workflow — Workflow Loader

**包路径**：`internal/workflow`

**职责**：读取 `WORKFLOW.md`，解析 YAML front matter + prompt body，渲染模板，监控文件变更。（参见 SPEC §5）

#### 5.1.1 接口

```go
// Load 读取并解析 WORKFLOW.md
func Load(path string) (*model.WorkflowDefinition, error)

// Watch 启动文件监控，变更时调用 onChange 回调
func Watch(ctx context.Context, path string, onChange func(*model.WorkflowDefinition)) error

// RenderPrompt 使用严格模式渲染模板
func RenderPrompt(tmpl string, issue *model.Issue, attempt *int) (string, error)
```

#### 5.1.2 实现要点

**文件解析**（SPEC §5.2）：
- 若文件以 `---` 开头，逐行扫描至下一个 `---`，中间内容作为 YAML front matter
- 剩余行作为 prompt body
- 无 front matter 时，整个文件为 prompt body，config 为空 map
- YAML 解码目标为 `map[string]any`；非 map 返回 `ErrFrontMatterNotMap`
- Prompt body 执行 `strings.TrimSpace`

**模板渲染**（SPEC §5.4）：
- 使用 `github.com/osteele/liquid` Liquid 模板引擎
- 启用严格模式：未知变量和未知过滤器均报错
- 输入变量：`issue`（`map[string]any` 形式，key 为小写蛇形）和 `attempt`（`*int`，nil 在模板中为 falsy）
- 保留嵌套数组/map（labels, blockers），使模板可用 `{% for %}` 遍历
- 模板语法与官方 Elixir 实现一致：`{{ issue.title }}`、`{% if attempt %}`
- 空 prompt body 时使用默认提示词：`"You are working on an issue from Linear."`
- 文件读取/解析失败是配置错误，**不**静默回退到默认提示词

**文件监控**（SPEC §6.2）：
- 使用 `fsnotify.Watcher` 监听 WORKFLOW.md
- 变更事件 debounce（避免保存时多次触发）
- 失败的 reload 保持上次有效配置，发出 operator-visible 错误日志
- 不崩溃服务

#### 5.1.3 错误类型

| 错误 | 触发条件 | SPEC 引用 |
|---|---|---|
| `ErrMissingWorkflowFile` | 文件不存在或无法读取 | §5.5 |
| `ErrWorkflowParseError` | YAML 解析失败 | §5.5 |
| `ErrFrontMatterNotMap` | YAML 解码结果非 map | §5.5 |
| `ErrTemplateParseError` | 模板语法错误 | §5.5 |
| `ErrTemplateRenderError` | 未知变量/过滤器 | §5.5 |

#### 5.1.4 验证标准

- [ ] 正常解析含 front matter 的文件
- [ ] 无 front matter 时返回空 config + 完整 prompt body
- [ ] 非 map front matter 返回 `ErrFrontMatterNotMap`
- [ ] 空 prompt body 使用默认提示词
- [ ] 未知模板变量触发 `ErrTemplateRenderError`
- [ ] 文件变更触发 reload 回调
- [ ] 无效 reload 保持上次有效配置

---

### 5.2 config — Config Layer

**包路径**：`internal/config`

**职责**：将 `WorkflowDefinition.Config` 解析为类型化的 `ServiceConfig`，处理默认值、环境变量和路径展开。（参见 SPEC §6）

#### 5.2.1 接口

```go
// NewFromWorkflow 从 WorkflowDefinition 解析类型化配置
func NewFromWorkflow(def *model.WorkflowDefinition) (*model.ServiceConfig, error)

// ValidateForDispatch 运行 dispatch preflight 校验
func ValidateForDispatch(cfg *model.ServiceConfig) error
```

#### 5.2.2 实现要点

**值解析**（SPEC §6.1）：
- `$VAR` 环境变量解析：检测 `^\$(\w+)$` 模式，调用 `os.Getenv`；空值视为缺失
- `~` 路径展开：`os.UserHomeDir()`，仅对路径类字段展开
- 字符串整数强制转换：`strconv.Atoi`（如 `polling.interval_ms` 可能是字符串 `"30000"`）
- `active_states`/`terminal_states`：支持 `[]string` 和逗号分隔字符串两种形式
- 状态归一化：`strings.TrimSpace` + `strings.ToLower`（用于 `max_concurrent_agents_by_state` 的 key）
- `max_concurrent_agents_by_state` 中非正整数或非数值条目忽略

**默认值**（SPEC §6.4）：

| 字段 | 默认值 |
|---|---|
| `tracker.endpoint` | `https://api.linear.app/graphql`（kind=linear 时） |
| `tracker.active_states` | `["Todo", "In Progress"]` |
| `tracker.terminal_states` | `["Closed", "Cancelled", "Canceled", "Duplicate", "Done"]` |
| `polling.interval_ms` | `30000` |
| `workspace.root` | `<os.TempDir()>/symphony_workspaces` |
| `hooks.timeout_ms` | `60000` |
| `agent.max_concurrent_agents` | `10` |
| `agent.max_turns` | `20` |
| `agent.max_retry_backoff_ms` | `300000` |
| `codex.command` | `codex app-server` |
| `codex.turn_timeout_ms` | `3600000` |
| `codex.read_timeout_ms` | `5000` |
| `codex.stall_timeout_ms` | `300000` |

**Dispatch Preflight 校验**（SPEC §6.3）：
- Workflow 文件可加载和解析
- `tracker.kind` 存在且受支持（当前仅 `linear`）
- `tracker.api_key` 在 `$` 解析后存在
- `tracker.project_slug` 在 kind=linear 时存在
- `codex.command` 存在且非空

启动时校验失败 → 启动失败。每 tick 校验失败 → 跳过该 tick 调度，对账继续。

#### 5.2.3 验证标准

- [ ] 所有默认值在可选值缺失时正确应用
- [ ] `$VAR` 解析正确获取环境变量
- [ ] `$VAR` 解析为空字符串时视为缺失
- [ ] `~` 路径展开正确
- [ ] 逗号分隔字符串正确拆分为列表
- [ ] 字符串整数强制转换成功
- [ ] `max_concurrent_agents_by_state` 忽略无效条目
- [ ] Dispatch preflight 校验：缺少 `tracker.kind` 报错
- [ ] Dispatch preflight 校验：缺少 API key 报错
- [ ] Dispatch preflight 校验：缺少 project slug 报错

---

### 5.3 tracker — Issue Tracker Client

**包路径**：`internal/tracker`

**职责**：从 Linear 获取候选 issue、按 ID 刷新状态、按状态获取 issue。（参见 SPEC §11）

#### 5.3.1 接口

```go
type Client interface {
    // FetchCandidateIssues 获取配置的活跃状态下的候选 issue
    FetchCandidateIssues(ctx context.Context) ([]model.Issue, error)

    // FetchIssuesByStates 按状态名获取 issue（用于启动清理）
    FetchIssuesByStates(ctx context.Context, states []string) ([]model.Issue, error)

    // FetchIssueStatesByIDs 按 issue ID 获取当前状态（用于对账）
    FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]model.Issue, error)
}
```

#### 5.3.2 Linear 适配器实现要点

**查询构建**（SPEC §11.2）：
- GraphQL endpoint：`tracker.endpoint`（默认 `https://api.linear.app/graphql`）
- Auth：`Authorization: <api_key>` header
- 项目过滤：`project: { slugId: { eq: $projectSlug } }`
- 候选 issue 查询按活跃状态过滤
- issue 状态刷新查询使用 GraphQL ID 类型 `[ID!]`

**分页**：
- 循环读取直到 `pageInfo.hasNextPage == false`
- 提取 `endCursor` 用于下一页
- 页大小默认 `50`
- 缺少 `endCursor` 时返回 `ErrLinearMissingEndCursor`

**网络**：
- `http.Client{Timeout: 30 * time.Second}`
- 使用 `context.Context` 传递取消信号

**Issue 归一化**（SPEC §11.3）：
- `labels` → `strings.ToLower`
- `blocked_by` → 从 inverse relations 中提取 type=`blocks` 的记录
- `[实现扩展]` `tracker.linear.children_block_parent=true` 时，将未终态 children 也归一化为父任务的 `blocked_by`
- `priority` → 仅整数，非整数变 nil
- `created_at`/`updated_at` → 解析 ISO-8601

**层级阻塞语义**：
- 默认开启 `tracker.linear.children_block_parent=true`
- 只在父任务仍处于 `Todo`、尚未被调度时生效
- 它复用现有 `BlockedBy` 判定，不新增独立状态机
- 若父任务已经进入 `In Progress` / `Running`，后续再出现子任务不回头中断或回收

**空输入处理**：
- `FetchIssuesByStates([])` → 直接返回空切片，不发 API 请求
- `FetchIssueStatesByIDs([])` → 直接返回空切片

#### 5.3.3 错误类型

| 错误 | 触发条件 | SPEC 引用 |
|---|---|---|
| `ErrUnsupportedTrackerKind` | kind 非 "linear" | §11.4 |
| `ErrMissingTrackerAPIKey` | API key 缺失 | §11.4 |
| `ErrMissingTrackerProjectSlug` | project slug 缺失 | §11.4 |
| `ErrLinearAPIRequest` | HTTP 传输错误 | §11.4 |
| `ErrLinearAPIStatus` | 非 200 HTTP 状态 | §11.4 |
| `ErrLinearGraphQLErrors` | GraphQL 级错误 | §11.4 |
| `ErrLinearUnknownPayload` | 响应结构异常 | §11.4 |
| `ErrLinearMissingEndCursor` | 分页完整性错误 | §11.4 |

#### 5.3.4 验证标准

- [ ] 候选 issue 使用活跃状态 + project slug 过滤
- [ ] Linear 查询使用 `slugId` 字段过滤项目
- [ ] 空 states 列表不发 API 请求
- [ ] 分页跨多页保持顺序
- [ ] Blockers 从 type=blocks 的 inverse relations 归一化
- [ ] `tracker.linear.children_block_parent=true` 时，未终态 children 会阻塞父任务调度
- [ ] children 全部终态时，不再阻塞父任务调度
- [ ] Labels 归一化为小写
- [ ] Issue 状态刷新查询使用 `[ID!]` 类型
- [ ] 各种 HTTP/GraphQL 错误正确映射
- [ ] 使用 `httptest.Server` mock Linear 响应进行测试

---

### 5.4 orchestrator — Orchestrator

**包路径**：`internal/orchestrator`

**职责**：拥有轮询 tick、内存运行时状态、决定哪些 issue 调度/重试/停止/释放。（参见 SPEC §7, §8）

这是系统中最复杂的模块。

#### 5.4.1 依赖注入

```go
type Orchestrator struct {
    tracker   tracker.Client
    workspace workspace.Manager
    runner    agent.Runner
    config    func() *model.ServiceConfig  // 获取当前配置的函数（支持热加载）
    // ... 内部状态 + channels
}
```

#### 5.4.2 状态机（SPEC §7.1）

Issue 编排状态（非 tracker 状态）：

```
Unclaimed ──(dispatch)──→ Claimed ──→ Running
                              │              │
                              │         (exit normal)
                              │              ↓
                              ├──→ RetryQueued ──(timer)──→ 重新检查
                              │
                              └──(terminal/inactive)──→ Released
```

- `Running`：worker goroutine 存在，issue 在 `running` map 中
- `RetryQueued`：worker 不运行，retry timer 存在于 `retry_attempts` map 中
- `Released`：claim 移除（issue 终态/非活跃/缺失/重试路径完成）

**重要细节**：
- Worker 正常退出**不**意味着 issue 永远完成
- Worker 可能在退出前执行多个连续 coding-agent turn
- 每次正常 turn 完成后，worker 重新检查 issue 状态
- 若 issue 仍在活跃状态且未达 `max_turns`，在同一线程启动下一个 turn
- 首次 turn 使用完整渲染的 task prompt；后续 turn 仅发送 continuation 指导
- Worker 正常退出后，orchestrator 仍调度短暂的 continuation retry（约 1 秒）以重新检查

#### 5.4.3 轮询调度（SPEC §8.1）

**启动序列**：
1. 校验配置
2. 执行启动终态工作区清理
3. 调度立即 tick（delay=0）
4. 进入事件循环

**Tick 序列**：
1. 对账运行中的 issue
2. 运行 dispatch preflight 校验
3. 从 tracker 获取候选 issue（活跃状态）
4. 按调度优先级排序 issue
5. 在有空闲槽位时调度合格 issue
6. 通知观察者/状态消费者

校验失败 → 跳过调度，但对账仍先执行。

#### 5.4.4 候选选择规则（SPEC §8.2）

Issue 满足**全部**条件时可调度：

- 有 `id`、`identifier`、`title`、`state`
- `state` 在 `active_states` 中且不在 `terminal_states` 中
- 不在 `running` map 中
- 不在 `claimed` set 中
- 全局并发槽位可用
- 按状态并发槽位可用
- **Todo 阻塞规则**：若 issue 状态为 Todo，任何 blocker 为非终态时不调度

**排序**（稳定排序）：
1. `priority` 升序（1..4 优先；nil 排最后）
2. `created_at` 最早优先
3. `identifier` 字典序（决胜）

#### 5.4.5 并发控制（SPEC §8.3）

- 全局限制：`available_slots = max(max_concurrent_agents - len(running), 0)`
- 按状态限制：若 `max_concurrent_agents_by_state[normalized_state]` 存在则使用，否则退回全局限制
- 按 `running` map 中 issue 的当前 tracked state 计数

#### 5.4.6 重试和退避（SPEC §8.4）

**Retry entry 创建**：
- 取消同一 issue 的现有 retry timer
- 存储 `attempt`、`identifier`、`error`、`due_at`、新 timer handle

**退避公式**：
- **正常退出 continuation retry**：固定 `1000ms` 延迟，attempt=1
- **失败驱动 retry**：`base = min(10000 * 2^(attempt-1), max_retry_backoff_ms)`，然后加 jitter：`delay = base * (0.5 + rand(0, 0.5))`
- 上限默认 `300000ms`（5 分钟）
- **Jitter 说明**：避免高并发场景下多个 retry 同时触发（thundering herd）。竞品 contrabass 同样采用 deterministic jitter。

**Retry 处理**：
1. 获取活跃候选 issue（非全部 issue）
2. 按 `issue_id` 查找
3. 未找到 → 释放 claim
4. 找到且仍合格 → 有槽位则调度，否则以 `"no available orchestrator slots"` 重新排队
5. 找到但不再活跃 → 释放 claim

#### 5.4.7 对账（SPEC §8.5）

每 tick 执行，分两部分：

**Part A：停滞检测**
- 对每个运行中 issue，计算 `elapsed` = `time.Since(lastCodexTimestamp)`（或 `startedAt`，若无事件）
- 若 `elapsed > stall_timeout_ms` → 终止 worker + 排队 retry
- 若 `stall_timeout_ms <= 0` → 跳过停滞检测

**Part B：Tracker 状态刷新**
- 批量获取所有运行中 issue ID 的当前状态
- 终态 → 终止 worker + 清理工作区
- 仍活跃 → 更新内存 issue 快照
- 非活跃也非终态 → 终止 worker，不清理工作区
- 刷新失败 → 保持 worker 运行，下次 tick 重试

#### 5.4.8 启动终态工作区清理（SPEC §8.6）

1. 查询 tracker 获取终态 issue
2. 对每个返回的 issue identifier，移除对应工作区目录
3. 查询失败 → 记录警告，继续启动

#### 5.4.9 验证标准

- [ ] 调度排序：priority 升序 → created_at 升序 → identifier 字典序
- [ ] Todo 状态有非终态 blocker 时不调度
- [ ] Todo 状态的 blocker 全部终态时可调度
- [ ] 活跃状态刷新更新 running entry 的 issue 快照
- [ ] 非活跃状态停止 worker，不清理工作区
- [ ] 终态停止 worker 并清理工作区
- [ ] 无运行 issue 时对账为 no-op
- [ ] 正常 worker 退出调度 continuation retry（attempt=1, delay=1s）
- [ ] 异常 worker 退出触发指数退避
- [ ] 退避上限使用 `max_retry_backoff_ms`
- [ ] retry 条目包含 attempt、due time、identifier、error
- [ ] 停滞检测杀死会话并排队 retry
- [ ] 槽位耗尽时以明确错误原因重新排队
- [ ] 配置热加载后轮询间隔和并发限制生效

---

### 5.5 workspace — Workspace Manager

**包路径**：`internal/workspace`

**职责**：管理 per-issue 工作区目录的创建/复用/清理和钩子执行。（参见 SPEC §9）

#### 5.5.1 接口

```go
type Manager interface {
    // CreateForIssue 创建或复用 issue 的工作区
    CreateForIssue(ctx context.Context, identifier string) (*model.Workspace, error)

    // CleanupWorkspace 清理 issue 的工作区（运行 before_remove 钩子后删除目录）
    CleanupWorkspace(ctx context.Context, identifier string) error
}
```

#### 5.5.2 实现要点

**路径管理**（SPEC §9.1）：
- 工作区根：`config.WorkspaceRoot`（归一化为绝对路径）
- per-issue 路径：`<workspace_root>/<sanitized_issue_identifier>`
- 工作区跨运行复用，成功运行不自动删除

**Workspace Key 消毒**（SPEC §4.2）：
- `regexp.MustCompile("[^A-Za-z0-9._-]").ReplaceAllString(identifier, "_")`

**创建/复用算法**（SPEC §9.2）：
1. 消毒 identifier → workspace_key
2. 计算路径 = workspace_root + workspace_key
3. `os.MkdirAll(path, 0755)` 确保目录存在
4. 判断 `created_now`：若调用前目录不存在则为 true
5. 若 `created_now && hook_after_create != nil` → 执行 `after_create` 钩子

**安全不变量**（SPEC §9.5 — 最重要的可移植性约束）：

| 不变量 | 要求 | 实现 |
|---|---|---|
| #1 Agent 只在工作区路径运行 | 启动前验证 `cwd == workspace_path` | 设置 `cmd.Dir` |
| #2 工作区路径在根目录内 | `filepath.Abs` 归一化后 `strings.HasPrefix` | 创建时检查 |
| #3 Workspace key 已消毒 | 只允许 `[A-Za-z0-9._-]` | 正则替换 |

**钩子执行**（SPEC §9.4）：
- 执行方式：`exec.CommandContext(ctx, "bash", "-lc", script)`
- `cmd.Dir` = workspace 路径
- 超时：`context.WithTimeout(ctx, hookTimeoutMS)`

**钩子失败语义**：

| 钩子 | 失败/超时行为 |
|---|---|
| `after_create` | 致命：中止工作区创建，可清理已创建目录 |
| `before_run` | 致命：中止当前运行尝试 |
| `after_run` | 记录日志，忽略 |
| `before_remove` | 记录日志，忽略；清理仍继续 |

#### 5.5.3 验证标准

- [ ] 同一 identifier 产生确定性路径
- [ ] 不存在时创建目录，已存在时复用
- [ ] 路径逃逸（`../`）被拒绝
- [ ] workspace key 消毒正确（特殊字符替换为 `_`）
- [ ] `after_create` 仅在新建时执行
- [ ] `before_run` 失败/超时中止当前尝试
- [ ] `after_run` 失败/超时仅记录
- [ ] `before_remove` 失败/超时仅记录，清理继续
- [ ] 钩子超时正确杀进程

---

### 5.6 agent — Agent Runner

**包路径**：`internal/agent`

**职责**：包装 workspace + prompt + app-server 子进程客户端，管理 Codex 协议会话。（参见 SPEC §10）

#### 5.6.1 接口

```go
type Runner interface {
    Run(ctx context.Context, params RunParams) error
}

type RunParams struct {
    Issue         *model.Issue
    Attempt       *int
    WorkspacePath string
    OnEvent       func(AgentEvent)  // 上报事件到 orchestrator
}

type AgentEvent struct {
    Event              string     // 事件类型枚举字符串
    Timestamp          time.Time
    CodexAppServerPID  *string
    Usage              *TokenUsage
    Payload            any        // 额外数据
}

type TokenUsage struct {
    InputTokens  int64
    OutputTokens int64
    TotalTokens  int64
}
```

#### 5.6.2 App-Server 协议客户端实现要点

**进程启动**（SPEC §10.1）：
- 命令：`exec.CommandContext(ctx, "bash", "-lc", config.CodexCommand)`
- `cmd.Dir` = workspace 路径
- `cmd.Stdin` = pipe（用于发送 JSON-RPC 消息）
- `cmd.Stdout` = pipe（用于接收协议消息）
- `cmd.Stderr` = 单独处理（日志记录，不解析协议 JSON）
- stdout 最大行大小：10MB（`scanner.Buffer(buf, 10*1024*1024)`）

**握手序列**（SPEC §10.2）：

```
→ {"id":1,"method":"initialize","params":{...}}
← 等待响应（read_timeout_ms）
→ {"method":"initialized","params":{}}
→ {"id":2,"method":"thread/start","params":{...}}
← 等待响应 → 提取 thread_id
→ {"id":3,"method":"turn/start","params":{...}}
← 等待响应 → 提取 turn_id
```

- `initialize` params：`clientInfo: {name: "symphony-orchestrator", version: "1.0"}`, `capabilities: {experimentalApi: true}`（`experimentalApi` 启用 dynamic tools 等实验性功能，与官方 Elixir 实现一致）
- `thread/start` params：`approvalPolicy`, `sandbox`, `cwd`（绝对路径），`dynamicTools`（可选，用于注册客户端工具如 `linear_graphql`）
  - `dynamicTools` 格式：`[{name: "linear_graphql", description: "...", inputSchema: {type: "object", required: ["query"], properties: {query: {type: "string"}, variables: {type: "object"}}}}]`
- `turn/start` params：`threadId`, `input`（text item 含渲染后的 prompt）, `cwd`, `title`（`<identifier>: <title>`）, `approvalPolicy`, `sandboxPolicy`

**Session ID**：
- `thread_id` 来自 `thread/start` result `result.thread.id`
- `turn_id` 来自 `turn/start` result `result.turn.id`
- `session_id = "<thread_id>-<turn_id>"`
- 同一 worker 运行内所有 continuation turn 复用同一 `thread_id`

**Turn 流式处理**（SPEC §10.3）：
- 从 stdout 逐行读取 JSON
- 完成条件：`turn/completed`（成功）、`turn/failed`/`turn/cancelled`（失败）、超时、进程退出
- 缓冲不完整行直到换行符到达

**多 Turn 循环**（SPEC §7.1 重要细节）：
- Turn 成功后检查 issue 状态（调用 tracker）
- 若仍活跃且 turn_number < max_turns → 在同一线程发送 continuation `turn/start`
- 首次 turn：完整渲染 prompt
- 后续 turn：仅发送 continuation 指导（不重发原始 prompt）

**Codex 安全默认值**（对齐官方 Elixir 参考实现）：
- `codex.approval_policy` 默认 `never`（即 `{"reject":{"sandbox_approval":true,"rules":true,"mcp_elicitations":true}}`）
- `codex.thread_sandbox` 默认 `workspace-write`
- `codex.turn_sandbox_policy` 默认 `{type: "workspaceWrite"}`（限制写操作在工作区内）

**Approval 策略**（SPEC §10.5 — 本实现高信任环境）：
- 命令执行审批 → auto-approve（返回 `{approved: true}`）
- 文件变更审批 → auto-approve
- 用户输入请求 → **硬失败**，立即终止运行尝试
- 不支持的 dynamic tool call → 返回 `{success: false, error: "unsupported_tool_call"}`，继续会话

**超时**（SPEC §10.6）：
- `read_timeout_ms`：握手和同步请求的读取超时
- `turn_timeout_ms`：单个 turn 流的总超时
- `stall_timeout_ms`：由 orchestrator 基于事件不活跃强制执行

**事件映射**（SPEC §10.4）：
- `session_started`、`turn_completed`、`turn_failed`、`turn_cancelled`
- `approval_auto_approved`、`unsupported_tool_call`、`turn_input_required`
- `notification`、`other_message`、`malformed`

**Token 计量**（SPEC §13.5）：
- 优先使用绝对线程总量：`thread/tokenUsage/updated`、`total_token_usage`
- 忽略 delta 类载荷（如 `last_token_usage`）
- 跟踪 delta = 当前总量 - 上次报告总量，避免重复计数

#### 5.6.3 可选：linear_graphql 客户端工具扩展（SPEC §10.5）

**功能**：通过 app-server 会话暴露原始 Linear GraphQL 访问，使用 Symphony 配置的 tracker 认证。

**输入**：
```json
{"query": "GraphQL 查询字符串", "variables": {"可选": "变量对象"}}
```

**规则**：
- `query` 必须为非空字符串
- `query` 必须仅含一个 GraphQL 操作（多操作拒绝）
- `variables` 可选，须为 JSON 对象
- 复用配置的 Linear endpoint 和 auth

**返回语义**：
- 传输成功 + 无 GraphQL `errors` → `success=true`
- 有 GraphQL `errors` → `success=false`，但保留响应体用于调试
- 无效输入/缺失 auth/传输失败 → `success=false` + 错误载荷

#### 5.6.4 验证标准

- [ ] 握手消息序列正确（initialize → initialized → thread/start → turn/start）
- [ ] `initialize` 含 clientInfo/capabilities
- [ ] 读取超时正确触发
- [ ] Turn 超时正确触发
- [ ] 非 JSON stderr 行记录但不崩溃解析
- [ ] 子进程退出被检测
- [ ] Approval 请求被 auto-approve
- [ ] User input 请求触发硬失败
- [ ] 不支持的 tool call 返回失败响应，不中断会话
- [ ] Token 使用量从嵌套载荷正确提取
- [ ] 多 turn 循环在同一线程上正确运行
- [ ] linear_graphql 工具：有效查询执行成功
- [ ] linear_graphql 工具：GraphQL errors 返回 success=false
- [ ] linear_graphql 工具：无效参数返回结构化失败

---

### 5.7 server — HTTP Server（可选扩展）

**包路径**：`internal/server`

**职责**：提供 HTTP API 和 dashboard，用于运行时可观测性和操作调试。（参见 SPEC §13.7）

#### 5.7.1 端点

| 方法 | 路径 | 功能 | 状态码 |
|---|---|---|---|
| GET | `/` | Dashboard HTML | 200 |
| GET | `/api/v1/state` | 全局状态快照 JSON | 200 |
| GET | `/api/v1/{identifier}` | 单 issue 详情 JSON | 200 / 404 |
| POST | `/api/v1/refresh` | 触发立即轮询 + 对账 | 202 |
| GET | `/api/v1/events` | SSE 实时事件流 | 200 (text/event-stream) |

不支持的方法返回 `405 Method Not Allowed`。

#### 5.7.2 实现要点

**启用条件**：
- CLI `--port` 参数提供时启动
- `server.port` 在 WORKFLOW.md front matter 中存在时启动
- 优先级：CLI `--port` > `server.port`
- `server.port` 必须为整数；正整数绑定该端口，`0` 用于临时端口

**绑定**：
- 默认绑定 `127.0.0.1:<port>`（loopback）
- HTTP listener 设置变更（如端口）不需要热重绑，restart-required 行为合规

**路由**：
- 使用 `http.NewServeMux()`（Go 1.22+ 支持方法+路径模式匹配）

**状态获取**：
- 通过 orchestrator 暴露的只读快照接口获取
- 快照包含：running sessions、retry queue、token totals、rate limits

**`GET /api/v1/state` 响应**（SPEC §13.7.2）：

```json
{
  "generated_at": "RFC3339 时间戳",
  "counts": {"running": 2, "retrying": 1},
  "running": [{"issue_id", "issue_identifier", "state", "session_id", "turn_count", "last_event", "last_message", "started_at", "last_event_at", "tokens": {...}}],
  "retrying": [{"issue_id", "issue_identifier", "attempt", "due_at", "error"}],
  "codex_totals": {"input_tokens", "output_tokens", "total_tokens", "seconds_running"},
  "rate_limits": null
}
```

**`GET /api/v1/{identifier}` 响应**（SPEC §13.7.2）：
- 已知 issue → 详细运行时/调试信息
- 未知 issue → `404` + `{"error":{"code":"issue_not_found","message":"..."}}`

**`POST /api/v1/refresh` 响应**（SPEC §13.7.2）：
- `202 Accepted` + `{"queued":true,"coalesced":false,"requested_at":"...","operations":["poll","reconcile"]}`
- 可合并重复请求

**`GET /api/v1/events` SSE 端点**（竞品 contrabass 同款功能）：
- Content-Type: `text/event-stream`
- 连接建立时推送一次完整状态快照（`event: snapshot`）
- 后续推送增量事件（`event: update`），包含 orchestrator 状态变更
- 事件格式：`event: <type>\ndata: <json>\n\n`
- 客户端断开时释放资源
- Dashboard 可消费此端点实现实时更新，无需轮询

**Dashboard**：
- 初期使用简单的 server-rendered HTML template（`html/template`）
- 展示：活跃会话、retry 延迟、token 消耗、运行时总量、健康/错误指标

**错误格式**：
- `{"error":{"code":"...","message":"..."}}`

#### 5.7.3 验证标准

- [ ] `GET /api/v1/state` 返回正确格式
- [ ] `GET /api/v1/{identifier}` 已知 issue 返回详情
- [ ] `GET /api/v1/{identifier}` 未知 issue 返回 404
- [ ] `POST /api/v1/refresh` 返回 202
- [ ] 不支持方法返回 405
- [ ] 绑定 loopback 地址
- [ ] `GET /api/v1/events` SSE 连接建立后收到初始快照
- [ ] SSE 端点推送增量事件

---

### 5.8 logging — 日志

**包路径**：`internal/logging`

**职责**：配置和管理结构化日志。（参见 SPEC §13.1, §13.2）

#### 5.8.1 实现要点

**日志库**：`log/slog`，默认 JSON handler 输出到 stderr。

**日志输出配置**（对齐官方 Elixir `--logs-root` 和竞品 contrabass `--log-file`/`--log-level`）：
- 支持 `--log-file <path>` 参数：同时写入文件和 stderr（multi-writer）
- 支持 `--log-level <level>` 参数：`debug`/`info`/`warn`/`error`，默认 `info`
- 日志文件使用追加模式（append），不截断

**上下文字段**（SPEC §13.1）：
- Issue 级别：`slog.With("issue_id", ..., "issue_identifier", ...)`
- Session 级别：追加 `With("session_id", ...)`

**日志级别**：
- `Info`：正常流程（调度、完成、配置加载）
- `Warn`：可恢复错误（tracker 获取失败跳过 tick、无效 reload 保持旧配置）
- `Error`：需操作者注意（启动校验失败、worker 持续失败）
- `Debug`：详细调试（协议消息、状态变更细节）

**格式要求**：
- 使用稳定的 `key=value` 措辞
- 包含动作结果（`completed`、`failed`、`retrying` 等）
- 包含简洁的失败原因
- 避免记录大型原始载荷

**秘密处理**（SPEC §15.3）：
- **不**记录 API token 或秘密环境变量的原始值
- 验证秘密存在性时不打印值（如 `"api_key=***masked***"`）

**Sink 故障**：
- 日志 sink 失败时服务继续运行
- 通过剩余可用 sink 发出警告

#### 5.8.2 验证标准

- [ ] 结构化字段（issue_id, issue_identifier, session_id）存在于日志中
- [ ] API token 不出现在日志明文中
- [ ] Sink 失败不崩溃编排

---

## 6. CLI 入口

**文件路径**：`cmd/symphony/main.go`

**职责**：解析参数，组装依赖，启动/关闭服务。（参见 SPEC §17.7）

### 6.1 参数定义

使用标准库 `flag` 包：

| 参数 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| 位置参数（可选） | string | `./WORKFLOW.md` | Workflow 文件路径 |
| `--port` | int | 无（不启动 HTTP server） | HTTP server 端口 |
| `--dry-run` | bool | false | 执行第一个 poll cycle 后退出，用于验证配置 |
| `--log-file` | string | 无（仅 stderr） | 日志文件路径 |
| `--log-level` | string | `info` | 日志级别（debug/info/warn/error） |

### 6.2 启动流程

```
1. 解析 CLI 参数
2. 配置日志
3. 加载 WORKFLOW.md（显式路径或 cwd 默认）
4. 解析类型化配置
5. 运行 dispatch preflight 校验 → 失败则退出
6. 启动文件监控（WORKFLOW.md）
7. 执行启动终态工作区清理
8. 创建并启动 Orchestrator
9. 若配置了端口 → 启动 HTTP Server
10. 阻塞等待关闭信号
```

### 6.3 关闭流程

- 使用 `signal.NotifyContext` 监听 `SIGINT`、`SIGTERM`
- 关闭序列：停止轮询 → 等待活跃 worker 完成（设超时） → 停止 HTTP server → 退出

### 6.4 退出码

| 码 | 含义 |
|---|---|
| 0 | 正常关闭 |
| 1 | 启动失败或异常退出 |

### 6.5 验证标准

- [ ] CLI 接受可选位置参数作为 workflow 路径
- [ ] 无参数时使用 `./WORKFLOW.md`
- [ ] 显式路径不存在时报错退出
- [ ] 默认 `./WORKFLOW.md` 不存在时报错退出
- [ ] 启动失败时清晰输出错误
- [ ] 正常关闭退出码为 0
- [ ] 启动失败退出码为非零
- [ ] `--dry-run` 在第一个 poll cycle 后正常退出
- [ ] `--log-file` 写入日志到指定文件
- [ ] `--log-level` 过滤日志级别

---

## 7. 并发模型与 goroutine 架构

### 7.1 核心原则

**Orchestrator 是单一状态权威（single authority）。** 所有 worker 结果报告回 orchestrator 并转换为显式状态转换。（参见 SPEC §7.4）

### 7.2 架构决策：channel-based event loop

采用 **event loop** 模式（select on channels），而非 mutex 保护共享状态：

- 所有状态变更事件汇入主循环的 select
- 无死锁风险
- 状态变更天然序列化
- 更容易推理正确性

> "Don't communicate by sharing memory; share memory by communicating."

### 7.3 goroutine 清单

| goroutine | 职责 | 通信方式 |
|---|---|---|
| **主 goroutine**（Orchestrator 事件循环） | 持有全部调度状态；通过 select 接收 tick timer / worker result / codex update / config reload / shutdown 事件 | select on channels |
| **Worker goroutine**（每个活跃 issue 一个） | 运行 `agent.Runner.Run()`；通过 channel 向 orchestrator 报告事件和退出结果 | `workerResultCh`, `codexUpdateCh` |
| **File watcher goroutine** | 监听 WORKFLOW.md 变更；变更事件发送到 orchestrator 的 config reload channel | `configReloadCh` |
| **HTTP server goroutine**（可选） | 处理 HTTP 请求；通过只读快照接口读取状态 | `sync.RWMutex` 保护快照 |

### 7.4 Channel 定义

```go
type Orchestrator struct {
    tickTimer      *time.Timer
    workerResultCh chan WorkerResult    // worker 退出结果
    codexUpdateCh  chan CodexUpdate     // coding agent 事件
    configReloadCh chan *model.WorkflowDefinition // 配置变更
    refreshCh      chan struct{}        // HTTP refresh 触发
    shutdownCh     chan struct{}        // 关闭信号
}
```

### 7.5 主循环伪代码

```go
for {
    select {
    case <-tickTimer.C:
        tick()
        tickTimer.Reset(pollIntervalMS)
    case result := <-workerResultCh:
        handleWorkerExit(result)
    case update := <-codexUpdateCh:
        handleCodexUpdate(update)
    case def := <-configReloadCh:
        reloadConfig(def)
    case <-refreshCh:
        tick()  // 立即触发
    case <-shutdownCh:
        gracefulShutdown()
        return
    }
}
```

### 7.6 HTTP Server 的跨 goroutine 状态读取

- orchestrator 维护一个 `sync.RWMutex` 保护的状态快照
- 每次状态变更后更新快照（写锁）
- HTTP handler 读取快照（读锁）
- 快照是值拷贝，不持有 channel/timer 等运行时引用

---

## 8. 错误处理策略

### 8.1 错误类型体系

在 `internal/model/errors.go` 中定义 typed error：

```go
// WorkflowError 工作流/配置错误
type WorkflowError struct {
    Code    string // missing_workflow_file, workflow_parse_error, etc.
    Message string
    Err     error  // 可选包装错误
}

// WorkspaceError 工作区错误
type WorkspaceError struct {
    Code    string
    Message string
    Err     error
}

// AgentError 编码代理错误
type AgentError struct {
    Code    string // codex_not_found, turn_timeout, turn_input_required, etc.
    Message string
    Err     error
}

// TrackerError issue tracker 错误
type TrackerError struct {
    Code    string // linear_api_request, linear_graphql_errors, etc.
    Message string
    Err     error
}
```

每个类型实现 `error` 接口 + `Unwrap() error`，支持 `errors.Is()` / `errors.As()` 链式判断。

### 8.2 恢复行为映射（SPEC §14.2）

| 错误场景 | 恢复行为 |
|---|---|
| Dispatch 校验失败 | 跳过新调度，服务存活，对账继续 |
| Worker 失败 | 转为 retry（指数退避） |
| Tracker 候选获取失败 | 跳过本 tick，下次 tick 重试 |
| 对账状态刷新失败 | 保持当前 worker，下次 tick 重试 |
| Dashboard/日志失败 | 不崩溃 orchestrator |
| Workflow 热加载失败 | 保持上次有效配置，记录错误 |

### 8.3 规范

- 不使用 `panic`/`recover` 做流程控制
- 所有错误通过返回值传播
- 在系统边界（外部 API、用户输入、文件 I/O）做验证
- 内部函数间信任调用，不做冗余验证

---

## 9. 测试策略与验证标准

按 SPEC §17 的三个 profile 组织。

### 9.1 Core Conformance（必须）

每个 `internal/` 包都有 `*_test.go`：

| SPEC 章节 | 包 | 关键测试点 |
|---|---|---|
| §17.1 | `workflow`, `config` | 文件解析、front matter、默认值、$VAR、模板渲染、文件监控热加载 |
| §17.2 | `workspace` | 路径确定性、创建/复用、路径逃逸、钩子语义、安全不变量 |
| §17.3 | `tracker` | 候选获取、分页、归一化、空输入、错误映射 |
| §17.4 | `orchestrator` | 排序、阻塞规则、对账、retry、停滞检测、槽位耗尽 |
| §17.5 | `agent` | 握手序列、超时、stderr 处理、approval、tool call、token 计量 |
| §17.6 | `logging` | 结构化字段、秘密过滤、sink 故障 |
| §17.7 | `cmd/symphony` | CLI 参数、启动/关闭、退出码 |

### 9.2 Extension Conformance（可选功能实现时必须）

| 功能 | 关键测试点 |
|---|---|
| HTTP Server | 端点响应格式、404/405、refresh 触发 |
| linear_graphql tool | 查询执行、GraphQL errors、无效参数、auth 缺失 |

### 9.3 Real Integration Profile（推荐，CI 中可跳过）

- 需要 `LINEAR_API_KEY` 环境变量
- 使用隔离的测试标识符/工作区，实际完成后清理
- 跳过的真实集成测试报告为 skipped，不静默视为 passed
- 使用 `go test -tags=integration` 或 `testing.Short()` 控制

### 9.4 Mock 策略

| 外部依赖 | Mock 方式 |
|---|---|
| Linear API | `httptest.Server` mock 响应 |
| Codex app-server 子进程 | mock server 程序或 stdio pipe 模拟 |
| 文件系统 | `t.TempDir()` |
| 时间 | 可注入的 clock 接口（需要时） |
| tracker.Client | interface mock 实现 |

### 9.5 测试运行命令

```bash
# 单元测试（Core + Extension）
go test ./...

# 含覆盖率
go test -cover ./...

# 真实集成测试（需环境变量）
LINEAR_API_KEY=xxx go test -tags=integration ./...
```

---

## 10. 实现路线图

### 阶段 1：基础框架

**目标**：领域模型 + 配置解析 + CLI 骨架可编译运行。

| 模块 | 交付物 | 验证 |
|---|---|---|
| `internal/model` | 所有 struct + 枚举常量 + typed errors | 编译通过 |
| `internal/workflow` | Load + RenderPrompt | §17.1 测试通过 |
| `internal/config` | NewFromWorkflow + ValidateForDispatch | §17.1 测试通过 |
| `cmd/symphony` | CLI 骨架（参数解析 + 加载 + 校验 + 退出） | §17.7 基础测试 |

### 阶段 2：基础设施层

**目标**：工作区管理 + tracker 通信 + 日志可用。

| 模块 | 交付物 | 验证 |
|---|---|---|
| `internal/workspace` | CreateForIssue + CleanupWorkspace + 钩子 | §17.2 测试通过 |
| `internal/tracker` | Linear 适配器（候选/状态/终态查询） | §17.3 测试通过 |
| `internal/logging` | slog 配置 + 上下文 logger | §17.6 测试通过 |

### 阶段 3：核心引擎

**目标**：完整的调度/运行/重试/对账循环。

| 模块 | 交付物 | 验证 |
|---|---|---|
| `internal/agent` | App-Server 协议客户端 + turn 循环 | §17.5 测试通过 |
| `internal/orchestrator` | 完整状态机 + event loop | §17.4 测试通过 |
| `cmd/symphony` | 完整启动/关闭流程集成 | §17.7 完整测试 |

### 阶段 4：可选扩展

**目标**：HTTP 可观测性 + 工具扩展 + 端到端验证。

| 模块 | 交付物 | 验证 |
|---|---|---|
| `internal/server` | HTTP API + Dashboard | §13.7 端点测试 |
| `internal/agent`（扩展） | linear_graphql tool | §10.5 tool 测试 |
| 集成 | 端到端流程 | Real Integration Profile |

每阶段交付标准：对应 SPEC 测试矩阵中的验证项全部通过。

---

## 11. 竞品对比与扩展路线图

### 11.1 竞品概览

基于 2026-03-07 的网络调研，以下三个项目与 Symphony 直接相关：

| 项目 | 语言 | Stars | 定位 | 关键差异化特性 |
|---|---|---|---|---|
| [openai/symphony](https://github.com/openai/symphony) | Elixir/OTP | 7.8k | 官方参考实现 | Liquid 模板、Phoenix dashboard、Skills 系统、`linear_graphql` tool |
| [junhoyeo/contrabass](https://github.com/junhoyeo/contrabass) | Go (1.25+) | 新项目 | Go + Charm 实现 | Charm TUI、React dashboard + SSE、多 tracker（GitHub Issues）、多 agent（OpenCode）、Git worktree 工作区、Homebrew 分发 |
| [ComposioHQ/agent-orchestrator](https://github.com/ComposioHQ/agent-orchestrator) | TypeScript | 3.8k | 插件架构 | 8 槽位插件、多 agent（Claude Code/Codex/Aider/OpenCode）、多 runtime（tmux/Docker/k8s）、Reactions 系统（CI 自修复）、Slack/webhook 通知、session restore |

### 11.2 已在本文档中修复的不足（P0 + P1）

以下问题已在本次修订中纳入 REQUIREMENTS.md 的对应章节：

| # | 问题 | 修复位置 |
|---|---|---|
| P0-1 | 模板引擎改为 Liquid（与官方兼容） | §3 技术选型、§5.1.2 模板渲染 |
| P0-2 | Codex 安全默认值明确化（`never` / `workspace-write`） | §4.3 ServiceConfig、§5.6.2 安全默认值 |
| P0-3 | `experimentalApi` capability | §5.6.2 握手序列 |
| P0-4 | `dynamicTools` 注册机制 | §5.6.2 thread/start params |
| P1-5 | 日志文件输出 + 级别配置 | §5.8.1、§6.1 CLI 参数 |
| P1-6 | `--dry-run` 模式 | §6.1 CLI 参数 |
| P1-7 | SSE 实时推送端点 | §5.7.1 端点表、§5.7.2 SSE 描述 |
| P1-8 | 退避 jitter | §5.4.6 退避公式 |
| P1-9 | 接口扩展性设计说明 | §2.2 包依赖方向 |

### 11.3 阶段 5 扩展路线图（P2 项，不阻塞首版）

以下为首版发布后的扩展方向，按优先级排列：

| 优先级 | 扩展 | 竞品参照 | 说明 |
|---|---|---|---|
| 高 | **Charm TUI** | contrabass | 使用 Bubble Tea 实现终端状态界面；`--no-tui` 切换 headless 模式 |
| 高 | **GitHub Issues tracker** | contrabass, agent-orchestrator | 实现 `tracker.Client` 的 GitHub 适配器 |
| 中 | **多 agent runner** | contrabass (OpenCode), agent-orchestrator (Claude Code, Aider) | 实现 `agent.Runner` 的 OpenCode/其他适配器 |
| 中 | **打包分发** | contrabass | GoReleaser + Homebrew formula + pre-built binaries |
| 中 | **通知系统** | agent-orchestrator | Slack webhook / Desktop 通知，在 issue 完成或阻塞时触发 |
| 低 | **Reactions 系统** | agent-orchestrator | CI 失败自动重试、review 反馈自动处理 |
| 低 | **Session 持久化** | agent-orchestrator, SPEC TODO | 跨进程重启保留 retry 队列和会话元数据 |
| 低 | **Skills 系统** | openai/symphony | `.codex/skills/` 目录，预定义 commit/push/pull/land 工作流 |
