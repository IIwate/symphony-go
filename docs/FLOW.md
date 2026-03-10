# Symphony-Go 完整流程功能文档

## 1. 文档目标

本文档基于当前仓库中的**实际代码实现**整理，并对照 `SPEC.md`、`REQUIREMENTS.md`、`IMPLEMENTATION.md` 校正边界。

本文档同时回答四个问题：

1. 当前代码实际能做什么。
2. 一次完整运行从启动到关闭会经过哪些模块和状态。
3. 哪些行为属于 **SPEC 定义的核心能力**。
4. 哪些行为属于 **Go 实现的扩展或偏离点**。

### 1.1 标注约定

- `[核心规范]`：与 `SPEC.md` 的核心约束直接一致。
- `[实现扩展]`：当前 Go 实现额外提供，但不是 SPEC 的必需能力。
- `[实现扩展 — 超出 SPEC 边界]`：当前代码存在，但超出了 SPEC 对 Symphony 的推荐抽象边界。
- `[实现决策]`：规范允许，但具体做法由当前实现自行选择。

---

## 2. 项目定位与规范边界

`Symphony-Go` 是一个面向 issue 驱动开发流程的长运行编排服务。其主职责是：

- 从 issue tracker 拉取候选 issue。
- 为每个 issue 准备独立工作区。
- 在工作区里启动 `codex app-server` 会话。
- 将 agent 运行产生的事件、用量和状态汇总到 orchestrator。
- 通过日志和可选 HTTP 状态面暴露运行状态。

### 2.1 当前实现边界

当前代码的实际能力边界如下：

- `[核心规范]` 调度前校验当前只接受 `tracker.kind=linear`。
- `[核心规范]` tracker 客户端当前只有 Linear 实现。
- `[实现扩展]` agent 会话支持动态工具 `linear_graphql`。
- `[实现扩展]` HTTP 服务是可选能力，监听 `127.0.0.1:<port>`。
- `[实现扩展]` Git 分支准备逻辑已落在 workspace 层。
- `[实现扩展 — 超出 SPEC 边界]` orchestrator 可在特定条件下检测关联 PR，并在 `orchestrator.auto_close_on_pr=true` 时采用 merge-aware 收口：open PR 进入 `AwaitingMerge`，merged PR 才尝试把 issue 转成 `Done`。

### 2.2 与 SPEC 的关键关系

- `[核心规范]` SPEC 将 Symphony 定位为 **scheduler / runner / tracker reader**。
- `[核心规范]` SPEC 允许可选扩展，例如 HTTP 状态面和 `linear_graphql` 工具。
- `[核心规范]` SPEC **不要求** orchestrator 拥有一等的 tracker 写入 API。
- `[实现扩展 — 超出 SPEC 边界]` 当前 Go 实现把部分 tracker 写入能力直接接入了 orchestrator 成功路径，而不是完全留给 workflow prompt + agent 工具链。

---

## 3. 目录与模块职责

### 3.1 根目录

- `cmd/symphony/main.go`：CLI 入口，负责参数解析、依赖装配、启动与关闭。
- `WORKFLOW.md`：运行时契约源，front matter 提供配置，正文提供 prompt 模板。
- `docs/SPEC.md`：语言无关规格。
- `docs/REQUIREMENTS.md`：Go 版本的需求与模块要求。
- `docs/IMPLEMENTATION.md`：实现分层和阶段结果。
- `docs/FLOW.md`：本文档。

### 3.2 `internal/` 模块

| 模块 | 作用 |
|---|---|
| `internal/model` | 共享领域模型、运行状态、typed error、`CloneIssue` 拷贝辅助 |
| `internal/workflow` | 读取 `WORKFLOW.md`、解析 front matter、渲染 prompt、监听热更新 |
| `internal/config` | front matter → `ServiceConfig`，并执行 dispatch preflight 校验 |
| `internal/tracker` | 当前为 Linear GraphQL 客户端，实现 issue 查询、状态刷新、可选状态流转 |
| `internal/workspace` | 工作区创建/复用/清理、hooks、工作分支准备 |
| `internal/agent` | `codex app-server` 子进程协议客户端，多 turn 会话控制 |
| `internal/orchestrator` | 主状态机、调度、重试、对账、快照发布 |
| `internal/server` | HTTP API、SSE、Dashboard |
| `internal/logging` | `slog` 初始化、日志落盘、多 sink、脱敏 |
| `internal/shell` | 统一 `bash -lc` 调用；Windows 下优先定位 Git Bash |

### 3.3 测试分布

当前主要模块均有对应测试：

- `cmd/symphony/main_test.go`
- `cmd/symphony/main_integration_test.go`
- `internal/agent/runner_test.go`
- `internal/config/config_test.go`
- `internal/logging/logging_test.go`
- `internal/orchestrator/orchestrator_test.go`
- `internal/server/server_test.go`
- `internal/shell/bash_test.go`
- `internal/tracker/linear_test.go`
- `internal/tracker/linear_integration_test.go`
- `internal/workflow/workflow_test.go`
- `internal/workspace/manager_test.go`

补充说明：

- `model.CloneIssue(issue)` 现已导出，供 orchestrator / server / 测试等调用方复用。
- 它会复制 `Issue` 结构体本身，并对 `Labels`、`BlockedBy` 做独立切片拷贝，避免共享底层数组。
- `internal/testutil/integration.go` 现在提供 `RequireEnv`，用于 integration build tag 测试在缺少环境变量时按 skipped 退出，而不是误报通过。

### 3.4 代码中新增但值得关注的 error sentinel

以下错误码在当前实现中已经落地，适合在排障和阅读日志时关注：

- workflow/config：
  - `invalid_codex_command`
- workspace：
  - `workspace_path_escape`
  - `workspace_path_conflict`
  - `workspace_hook_failed`
  - `workspace_hook_timeout`
- agent：
  - `codex_not_found`
  - `invalid_workspace_cwd`
  - `response_timeout`
  - `turn_timeout`
  - `port_exit`
  - `response_error`
  - `turn_failed`
  - `turn_cancelled`
  - `turn_input_required`
- tracker：
  - `unsupported_tracker_kind`
  - `missing_tracker_api_key`
  - `missing_tracker_project_slug`
  - `linear_api_request`
  - `linear_api_status`
  - `linear_graphql_errors`
  - `linear_unknown_payload`
  - `linear_missing_end_cursor`
  - `linear_state_not_found`
  - `linear_transition_failed`

---

## 4. 运行时契约：`WORKFLOW.md`

`WORKFLOW.md` 是运行时唯一配置源，结构分为两部分：

1. YAML front matter：配置 tracker、workspace、hooks、agent、codex、server 等。
2. Markdown 正文：作为首轮 turn 的 prompt 模板。

### 4.1 当前仓库根部示例

当前根目录 `WORKFLOW.md` 的有效信息可以概括为：

- tracker 使用 Linear。
- API key 从 `$LINEAR_API_KEY` 注入。
- 项目标识使用 `project_slug`。
- 工作区根目录为 `H:/code/temp/symphony_workspaces`。
- `before_run` hook 会清空工作区并重新 clone 仓库。
- codex 命令是 `codex app-server`。
- prompt 明确要求：
  - 先创建并切换工作分支。
  - 完成开发后推送分支并创建 PR。
  - 不要自行 merge PR。
  - 创建/更新 PR 后，使用 `linear_graphql` 将 issue 设为完成态。

### 4.2 front matter 解析规则

- `[核心规范]` 若文件以 `---` 开头，则把下一段 `---` 之前内容解析为 YAML front matter。
- `[核心规范]` 若没有 front matter，则整份文件正文都作为 prompt body，配置为空 map。
- `[核心规范]` front matter 必须解码成 `map[string]any`；非 map 会返回 `workflow_front_matter_not_a_map`。
- `[核心规范]` prompt body 在使用前会做 trim。
- `[核心规范]` 未知顶层 key 应忽略，以保持前向兼容。
- `[实现扩展]` `server.port` 是当前实现支持的扩展顶层键。

需要明确：

- `[核心规范]` 文件读取失败、YAML 解析失败、front matter 非 map 都不会静默回退到默认 prompt，而是直接返回 workflow 级错误并阻断新调度。
- `[实现决策]` 只有在 **模板内容为空** 时，渲染阶段才会回退到内置默认提示词。

### 4.3 内置默认提示词

当前 `internal/workflow` 的 `DefaultPrompt` 文案为：

- `You are working on an issue from Linear.`

### 4.4 模板严格模式

`internal/workflow.RenderPrompt` 当前行为：

- `[核心规范]` 使用 Liquid 模板引擎。
- `[核心规范]` 开启 `StrictVariables`。
- `[核心规范]` 对未知变量和未知 filter 都视为 `template_render_error` / `template_parse_error` 范畴处理。
- `[核心规范]` 当前仅注入：
  - `issue`
  - `attempt`

### 4.5 Workflow 错误面与调度语义

当前实现与 SPEC 的关系如下：

- `[核心规范]` workflow 文件读错 / YAML 解析错：
  - 启动时：启动失败。
  - 运行中 reload：保留最后一次有效配置，输出可见错误。
- `[核心规范]` 模板渲染错误：
  - 只影响当前 run attempt。
  - 由 worker 失败路径接管，进入 orchestrator 的重试策略。

### 4.6 Workflow 监听与热更新

`internal/workflow.WatchWithErrors` 的行为：

- 监听目标 workflow 文件所在目录。
- 通过 debounce 合并频繁文件事件。
- 仅在变更命中目标文件路径时触发 reload。

热更新进入 CLI 的链路：

1. watcher 读到变更。
2. 重新 `Load` 新 workflow。
3. `runtimeState.ApplyReload` 调用 `config.NewFromWorkflow`。
4. 若 CLI 启动时传了 `--port`，该值会继续覆盖新配置。
5. 再次执行 `ValidateForDispatch`。
6. 只有校验通过时，才替换当前 `definition/config`。
7. orchestrator 收到 `NotifyWorkflowReload` 后刷新当前配置快照。

补充说明：

- `[核心规范]` invalid reload 不会导致服务崩溃，而是继续沿用最后一次有效配置。
- `[核心规范]` reload 影响未来 dispatch、retry 调度、reconcile、hook 执行与 agent 启动。
- `[实现决策]` 当前 HTTP server 已经启动后不会因为 `server.port` 变化而热重绑；该类 listener 资源仍需重启进程。

---

## 5. 配置层：`ServiceConfig`

### 5.1 当前已解析的字段

当前 `internal/config` 已处理的字段包括：

- `tracker.kind`
- `tracker.endpoint`
- `tracker.api_key`
- `tracker.project_slug`
- `tracker.repo` `[实现扩展]`
- `tracker.active_states`
- `tracker.terminal_states`
- `polling.interval_ms`
- `workspace.root`
- `workspace.linear_branch_scope` `[实现扩展]`
- `hooks.after_create`
- `hooks.before_run`
- `hooks.after_run`
- `hooks.before_remove`
- `hooks.timeout_ms`
- `agent.max_concurrent_agents`
- `agent.max_turns`
- `agent.max_retry_backoff_ms`
- `agent.max_concurrent_agents_by_state`
- `orchestrator.auto_close_on_pr` `[实现扩展]`
- `codex.command`
- `codex.approval_policy`
- `codex.thread_sandbox`
- `codex.turn_sandbox_policy`
- `codex.turn_timeout_ms`
- `codex.read_timeout_ms`
- `codex.stall_timeout_ms`
- `server.port` `[实现扩展]`

### 5.2 默认值

未显式配置时，当前代码中的默认值包括：

- `tracker.endpoint = https://api.linear.app/graphql`
- `active_states = ["Todo", "In Progress"]`
- `terminal_states = ["Closed", "Cancelled", "Canceled", "Duplicate", "Done"]`
- `polling.interval_ms = 30000`
- `workspace.root = <temp>/symphony_workspaces`
- `hooks.timeout_ms = 60000`
- `agent.max_concurrent_agents = 10`
- `agent.max_turns = 20`
- `agent.max_retry_backoff_ms = 300000`
- `codex.command = codex app-server`
- `codex.approval_policy = never`
- `codex.thread_sandbox = workspace-write`
- `codex.turn_sandbox_policy = {"type":"workspaceWrite"}`
- `codex.turn_timeout_ms = 3600000`
- `codex.read_timeout_ms = 5000`
- `codex.stall_timeout_ms = 300000`

### 5.3 值解析与强制转换规则

`internal/config` 当前实现了以下规则：

- `[核心规范]` `$VAR` 形式的字符串会读取环境变量。
- `[核心规范]` `$VAR` 解析后若环境变量不存在，返回空字符串；在 preflight 校验中等价于缺失。
- `[核心规范]` `~` 只对路径类字段展开到用户目录，不会任意重写 URI 或普通 shell 命令字符串。
- `[核心规范]` `active_states` / `terminal_states` 支持数组或逗号分隔字符串。
- `[核心规范]` 状态名会做 trim + lowercase 归一化参与比较。
- `[核心规范]` 字符串整数会尝试转为数值，例如 `"30000" -> 30000`。
- `[实现决策]` `max_concurrent_agents_by_state` 只保留正整数，非数字和非正数会被忽略。

### 5.4 dispatch preflight 的真实约束

`ValidateForDispatch` 当前检查：

- `tracker.kind` 必须存在。
- `tracker.kind` 当前必须是 `linear`。
- `tracker.api_key` 必填。
- `tracker.project_slug` 必填。
- `codex.command` 必填。

### 5.5 启动校验与每 tick 校验的差异

- `[核心规范]` 启动时校验失败：
  - CLI 启动失败。
  - `runCLI` 返回退出码 `1`。
- `[核心规范]` 每 tick 校验失败：
  - 该 tick 跳过 dispatch。
  - `reconcileRunning` 仍然先执行。
  - 记录 operator-visible warning。

---

## 6. CLI 启动与关闭流程

### 6.1 参数

CLI 当前支持：

- 位置参数：`WORKFLOW.md` 路径，默认 `./WORKFLOW.md`
- `--port`
- `--dry-run`
- `--log-file`
- `--log-level`

### 6.2 退出码

当前 `runCLI` 的退出语义：

- `0`：正常退出
- `1`：启动失败或异常退出

### 6.3 正常启动主链

`cmd/symphony/main.go` 中的 `execute` 会按以下顺序执行：

1. 解析 flag 和位置参数。
2. 校验日志级别。
3. 加载 `WORKFLOW.md`。
4. 调用 `config.NewFromWorkflow` 生成类型化配置。
5. 应用 CLI `--port` 覆盖。
6. 初始化 logger。
7. 执行 `config.ValidateForDispatch`。
8. 构造 `runtimeState`，把 workflow 与 config 封装成可热更新状态。
9. 创建 tracker、workspace manager、agent runner、orchestrator。
10. 如果是 `--dry-run`，执行一次 `orch.RunOnce(context.Background(), false)` 后退出。
11. 如果是正常运行：
    - 建立信号上下文（`os.Interrupt`、`SIGTERM`）。
    - 注册 workflow watcher。
    - 如果有 `server.port`，启动 HTTP server。
    - 启动 orchestrator。
    - 阻塞等待 orchestrator 结束。
    - 关闭 HTTP server。
    - 输出停止日志。

### 6.4 `--dry-run` 的真实行为

当前 `--dry-run` 不是单纯“只做配置解析”，而是：

- 仍会执行 `startupCleanup`
- 仍会执行一次 `tickWithMode`
- 仍会进行：
  - `reconcileRunning`
  - preflight 校验
  - 拉取 candidate issues
  - 排序
- **不会 dispatch issue**，因为 `allowDispatch=false`

这意味着：

- dry-run 依然会访问 tracker
- dry-run 可能执行启动终态工作区清理

### 6.5 关闭流程

正常关闭时的主顺序是：

1. 停止 tick timer。
2. 停掉所有 retry timer。
3. 取消所有运行中 worker 的 context。
4. 最多等待 worker goroutine 结束 2 秒。
5. orchestrator 退出。
6. 如果启用了 HTTP server，执行 `Shutdown`。
7. 关闭日志文件句柄并结束进程。

---

## 7. Workspace 生命周期与安全不变量

### 7.1 三项安全不变量

以下三项是 SPEC 强调的**最重要可移植性约束**，当前实现也有对应落点：

| 不变量 | 规范要求 | 当前实现 |
|---|---|---|
| Agent cwd 必须等于 workspace path | 运行 coding agent 前必须保证 `cwd == workspace_path` | `runner.Run` 要求 `WorkspacePath` 是绝对路径，`processFactory.StartProcess` 在该路径下启动子进程，`thread/start` 与 `turn/start` 也把 `cwd` 明确写成 `params.WorkspacePath` |
| Workspace path 必须位于 workspace root 内 | 先绝对化，再检查 `workspace_path` 必须在 `workspace_root` 下 | `newWorkspace` 中先 `filepath.Abs`，再 `ensureWithinRoot` 拒绝越界路径 |
| Workspace key 必须消毒 | 目录名只允许 `[A-Za-z0-9._-]`，其他字符替换为 `_` | `model.SanitizeWorkspaceKey(identifier)` 用于目录名生成 |

### 7.2 工作区创建

`CreateForIssue` 的行为：

- 使用 `model.SanitizeWorkspaceKey(identifier)` 生成目录名。
- 拼出绝对路径并校验必须位于 `workspace.root` 内。
- 路径不存在则创建，已存在目录则复用。
- 如果路径存在但不是目录，直接失败。
- 如果是新建目录且配置了 `hooks.after_create`，则执行 fatal hook。
- `after_create` 失败时会删除刚创建的目录并返回错误。

### 7.3 运行前准备

`PrepareForRun` 的行为：

- `[实现决策]` 清理 `tmp`、`.elixir_ls`
- 执行 `hooks.before_run`（fatal）
- 若工作区没有 `.git`，跳过分支准备
- 若存在 `.git`，执行 `ensureWorkBranch`

### 7.4 hooks 失败语义

当前 hooks 语义与 SPEC 一致：

| Hook | 失败语义 |
|---|---|
| `after_create` | Fatal，工作区创建失败 |
| `before_run` | Fatal，当前 run attempt 失败 |
| `after_run` | Best-effort，仅记录 |
| `before_remove` | Best-effort，仅记录，清理继续 |

补充实现细节：

- `[核心规范]` hook 运行有超时，取 `hooks.timeout_ms`，默认 `60000ms`
- `[核心规范]` hook 输出会记日志
- `[实现决策]` 当前实现会把 `stdout/stderr` 截断到 256 字符后写日志

### 7.5 工作分支准备

`ensureWorkBranch` 的规则：

1. 读取 `git config user.name` 作为 `<namespace>`。
2. 根据 tracker 类型生成 `<issue-short>`：
   - `linear`：`linear-<workspace.linear_branch_scope>-<issue-identifier>`
   - `github`：`github-<tracker.repo>-<issue-number>`
   - 其他：退化为 identifier slug
3. 读取当前分支、本地分支、远端分支。
4. 选择已有分支或创建新分支。
5. 使用 `git switch` / `git switch -c` 切换。

边界说明：

- `[实现扩展]` 这套 Git 分支准备逻辑超出 SPEC 对 workspace manager 的最小要求。
- `[核心规范]` SPEC 明确允许实现自行决定是否做 workspace population / VCS bootstrap。
- `[实现扩展]` `PrepareForRun` / `FinalizeRun` 也是当前 Go 实现层面的扩展方法，而不是 `Manager` 最小接口的必备部分。

### 7.6 运行后收尾与清理

`FinalizeRun` 的行为：

- 若配置了 `hooks.after_run`，以 best-effort 方式执行。

`CleanupWorkspace` 的行为：

- 路径不存在则视为成功。
- 若配置了 `hooks.before_remove`，以 best-effort 执行。
- 最终执行 `os.RemoveAll` 删除工作区目录。

---

## 8. Agent Runner：Codex 协议会话

### 8.1 进程启动

`runner.Run` 的前提条件：

- `params.Issue` 不能为空。
- `params.WorkspacePath` 必须是绝对路径。

之后会：

1. 使用 `cfg.CodexCommand` 启动子进程。
2. 分离 `stdin/stdout/stderr`。
3. 对 `stdout/stderr` 逐行扫描。
4. 启动 wait goroutine 监听进程退出。

### 8.2 启动协议与握手

当前实现的协议顺序是：

1. `initialize`
2. `initialized`
3. `thread/start`
4. `turn/start`

关键细节：

- `[核心规范]` `initialize` 的 `capabilities` 当前显式携带 `experimentalApi: true`
- `[核心规范]` `thread/start` 会带上：
  - `approvalPolicy`
  - `sandbox`
  - `cwd`
  - 可选 `dynamicTools`
- `[核心规范]` `turn/start` 会带上：
  - `threadId`
  - 文本输入 prompt
  - `cwd`
  - `title`
  - `approvalPolicy`
  - `sandboxPolicy`

### 8.3 stdout / stderr / 行缓冲策略

- `[核心规范]` 只从 `stdout` 读取协议消息。
- `[核心规范]` `stderr` 不参与协议 JSON 解析，只作为诊断信息上报。
- `[实现决策]` `scanLines` 使用 `bufio.Scanner`，最大单行协议消息大小为 `10MB`。

### 8.4 多 turn 逻辑

当前 `runner.Run` 不是单 turn：

- 第 1 个 turn 使用 `WORKFLOW.md` 正文模板渲染 prompt。
- 第 2 个 turn 开始，prompt 变成固定 continuation 文案。

每个 turn 完成后：

1. 用 `RefetchIssue` 重新拉取 issue。
2. 若 issue 已不在 active states，停止继续。
3. 若达到 `MaxTurns`，停止继续。
4. 否则进入下一轮 turn。

### 8.5 当前会话策略：高信任姿态

当前实现明确采用高信任策略：

- `approvalPolicy` 默认是 `never`
- 线程级 sandbox 默认是 `workspace-write`
- turn 级 sandbox policy 默认是 `{"type":"workspaceWrite"}`
- approval 请求自动批准
- `user-input-required` 直接视为硬失败

这与 SPEC 的关系：

- `[核心规范]` SPEC 允许实现自行定义 approval / sandbox / user-input posture
- `[实现决策]` 当前 Go 版本选择了高信任、自动批准的姿态

### 8.6 流式事件处理策略

`handleStreamMessage` 当前实现的关键策略：

- 遇到 `requestUserInput`：立即报 `turn_input_required`，并返回硬失败。
- 遇到 approval 请求：自动回写 `{ approved: true }`。
- 遇到 tool call：
  - 只支持 `linear_graphql`
  - 其他工具统一返回 `unsupported_tool_call`
- `turn/completed`：会话正常结束
- `turn/failed`：失败
- `turn/cancelled`：取消
- 包含 usage 或 rate limits 的消息会作为通知事件上报给 orchestrator。
- `[实现决策]` delta / item / response 类高频消息不会被当作“高层状态变更事件”强调，但其中带 usage/rate-limit 的消息仍可能被作为通知事件上报，不是简单丢弃。

### 8.7 `linear_graphql` 动态工具

动态工具出现的条件：

- `tracker.kind=linear`
- `tracker.api_key` 非空

工具行为：

- `[核心规范/扩展]` 输入必须是单个 GraphQL operation。
- 使用当前运行时的 Linear endpoint 和 auth。
- 成功时返回 `{ success: true, body: ... }`
- HTTP 非 200、无效 JSON、GraphQL `errors`、非法参数、无鉴权都返回失败结果。

### 8.8 Token / rate-limit 计量

先说明规范，再说明当前代码：

- `[核心规范]` SPEC 建议优先使用 absolute thread totals，忽略 delta-style payload，把累计增量留给 orchestrator 处理。
- `[实现决策]` 当前 Go 实现已经收紧为“**特定 method + known totals**”策略：
  - 只在以下 method 上尝试提取 usage：
    - `thread/tokenUsage/updated`
    - `codex/event/token_count`
    - `turn/completed`
    - `turn/failed`
    - `turn/cancelled`
  - 优先从以下字段递归寻找**绝对 totals**：
    - `total_token_usage`
    - `totalTokenUsage`
    - `token_usage`
    - `tokenUsage`
    - `token_count`
    - `tokenCount`
  - 会显式跳过：
    - `last_token_usage`
    - `lastTokenUsage`
    - `last_usage`
    - `lastUsage`
- `[实现决策]` rate-limit 提取仍然是递归查找，只要 key 名包含 `ratelimit` / `rate_limit` 就会上报给 orchestrator。
- `[实现决策]` orchestrator 侧继续用“当前 totals - 上次已报告 totals 的正增量”累计，避免同一会话的 totals 重复计数。

### 8.9 主要错误映射

当前 runner 处理的典型错误类型：

- `codex_not_found`
- `invalid_workspace_cwd`
- `response_timeout`
- `turn_timeout`
- `port_exit`
- `response_error`
- `turn_failed`
- `turn_cancelled`
- `turn_input_required`

---

## 9. Tracker：Linear 适配器

### 9.1 当前支持范围

`internal/tracker` 当前实际实现的是 Linear 客户端：

- `NewClient` 只接受 `tracker.kind=linear`
- `NewDynamicClient` 也是返回动态 Linear 客户端

### 9.2 接口层级

当前代码中的接口分层是：

- `Client`
  - `FetchCandidateIssues`
  - `FetchIssuesByStates`
  - `FetchIssueStatesByIDs`
- `IssueTransitioner`
  - `TransitionIssue`

边界说明：

- `[核心规范]` `Client` 这三个读取接口构成调度主链所需的 tracker 能力。
- `[实现扩展]` `TransitionIssue` 不在 `Client` 上，而是在独立 `IssueTransitioner` 接口上。

### 9.3 读取能力、分页与网络约束

tracker 当前会：

- 通过 `projectSlug + active states` 获取候选 issue
- 通过 `terminal states` 获取启动清理用 issue
- 通过 `issue IDs` 刷新正在运行 issue 的状态
- 使用分页拉取 candidate issues

实现细节：

- `[核心规范]` 默认页大小 `50`
- `[核心规范]` network timeout `30s`
- `[核心规范]` 使用 `Authorization: <api_key>` 请求头
- `[核心规范]` candidate query 用 `project.slugId` 过滤项目
- `[核心规范]` issue-state refresh 查询使用 GraphQL `ids: [ID!]`
- `[核心规范]` 分页依赖 `endCursor`；若 `hasNextPage=true` 但缺少 `endCursor`，会报 `linear_missing_end_cursor`
- `[核心规范]` `FetchIssuesByStates([])` / `FetchIssueStatesByIDs([])` 会直接返回空切片，不发请求

### 9.4 归一化规则

tracker 当前做的归一化包括：

- `labels` 转小写
- `blocked_by` 从 inverse relations 中 `type=blocks` 提取
- `priority` 只保留整数
- `createdAt/updatedAt` 解析为时间

### 9.5 tracker 错误体系

当前实现已经覆盖的主要 tracker 错误类型：

- `unsupported_tracker_kind`
- `missing_tracker_api_key`
- `missing_tracker_project_slug`
- `linear_api_request`
- `linear_api_status`
- `linear_graphql_errors`
- `linear_unknown_payload`
- `linear_missing_end_cursor`

另外还有 Go 实现新增的扩展错误：

- `linear_state_not_found`
- `linear_transition_failed`

### 9.6 `TransitionIssue`

`TransitionIssue` 的逻辑是：

1. 先查 issue 所属 team 的 workflow states
2. 优先按名称匹配目标状态（例如 `Done`）
3. 如果没找到，再退回到 `type=completed`
4. 再调用 `issueUpdate` 完成状态更新

边界说明：

- `[实现扩展]` 这是当前 Go 实现额外提供的 tracker 写入能力。
- `[实现扩展 — 超出 SPEC 边界]` SPEC 11.5 明确指出 Symphony **不要求** orchestrator 拥有一等 tracker write API；ticket 写入通常应由 workflow prompt 驱动的 coding agent + tools 完成。
- `[实现决策]` “找不到 `Done` 时退回到 `type=completed`” 也是当前实现自己的决策，而不是 SPEC 强制要求。

### 9.7 Tracker Writes 的规范边界

需要明确：

- `[核心规范]` SPEC 将 tracker 写入视为 agent 工具链职责，而非调度器核心职责。
- `[核心规范]` 即使实现了 `linear_graphql`，它也应被视为 agent toolchain 的一部分，而不是 orchestrator 业务逻辑的默认组成。

---

## 10. Orchestrator：核心状态机

### 10.1 核心职责

`internal/orchestrator` 负责：

- 持有运行中 issue、claimed issue、retry 队列、用量统计。
- 周期性轮询 tracker。
- 决定 issue 是否可调度。
- 启动 worker goroutine。
- 消费 worker 结果和 agent 实时事件。
- 处理停滞、终态清理、重试与 continuation retry。
- 发布运行时快照给 HTTP 层。

### 10.2 启动时初始化

`NewOrchestrator` 会：

- 注入 tracker / workspace / runner / config / workflow / logger。
- 初始化 channel：
  - `workerResultCh`
  - `codexUpdateCh`
  - `configReloadCh`
  - `refreshCh`
  - `retryFireCh`
  - `shutdownCh`
- 初始化状态：
  - `Running`
  - `Claimed`
  - `RetryAttempts`
  - `Completed`
- 初始化快照相关状态：
  - `subscribers`
  - `systemAlerts`
  - `pendingCleanup`
- 立即执行 `applyCurrentConfigLocked + refreshSnapshotLocked`，生成首份内存快照。

补充说明：

- `systemAlerts` 用于存放系统级告警，例如 preflight / tracker 不可达 / 启动清理失败。
- `pendingCleanup` 用于处理“对账先取消 worker，再等 worker 回传结果后做延迟清理”的场景，避免直接在对账路径里同步阻塞。
- `Completed` 目前仍只是 bookkeeping，不参与 dispatch 门控；`rememberCompletedLocked` 只会在首次看到某个 `issueID` 时追加记录，超过上限后淘汰最早写入的条目，避免长时间运行时无限增长。

### 10.3 `Start` 的主循环

`Start` 会先执行一次 `startupCleanup`，然后启动 goroutine，循环处理：

- `ctx.Done()`：优雅关闭
- `shutdownCh`：优雅关闭
- `tickTimer.C`：触发 `tick`
- `workerResultCh`：处理 worker 退出
- `codexUpdateCh`：处理 agent 事件
- `refreshCh`：立即触发一次 tick
- `retryFireCh`：执行某个 issue 的 retry
- `configReloadCh`：重新投影配置与快照

第一次 `tickTimer` 是 `0`，所以服务启动后会立即进行首轮调度。

补充说明：

- `RequestRefresh` 和 `NotifyWorkflowReload` 都是**非阻塞投递**；当对应 channel 已满时，新增信号会被丢弃，因此它们的语义更接近“poke/coalesce”，而不是强保证的排队任务。
- `SubscribeSnapshots` 在注册时会立即推送一份当前快照，之后每次快照发布时再继续推送更新。

### 10.4 启动时终态工作区清理

`startupCleanup` 会：

1. 用 `cfg.TerminalStates` 调用 `tracker.FetchIssuesByStates`
2. 对每个终态 issue 执行 `workspace.CleanupWorkspace`

这一步在：

- 正常启动时会执行
- `RunOnce` 也会执行

告警语义：

- 若终态拉取失败，会设置系统告警 `tracker_terminal_fetch_failed`
- 若终态拉取成功，会清除 `tracker_terminal_fetch_failed`
- 启动清理失败不会阻止服务继续进入后续 tick

### 10.5 每轮 tick 的真实顺序

`tickWithMode` 的顺序是：

1. `reconcileRunning`
2. `ValidateForDispatch`
3. `tracker.FetchCandidateIssues`
4. 按优先级排序
5. 若 `allowDispatch=false`，仅更新快照后返回
6. `dispatchEligibleIssues`
7. 刷新并发布快照

当前实现还带有明确的告警读写语义：

- preflight 失败时设置 `dispatch_preflight_failed`
- preflight 成功时清除 `dispatch_preflight_failed`
- candidate 拉取失败时设置 `tracker_unreachable`
- candidate 拉取成功时，**只有以下两种情况才会清除** `tracker_unreachable`：
  - 本轮没有对 running issue 做状态刷新
  - 本轮状态刷新成功

这样做的原因是：

- 若 `reconcileRunning` 里的 `FetchIssueStatesByIDs` 失败，但同一轮 tick 后续 `FetchCandidateIssues` 成功，不能让后者把前者的故障告警覆盖掉。
- 当前 `/api/v1/state` 暴露的是最终快照，因此告警清理时机必须与“本轮最终是否真的恢复对账视野”保持一致。

### 10.6 候选 issue 排序与资格判定

排序顺序：

1. `priority` 小的优先，未设置在实现中视为 `999` `[实现决策]`
2. `created_at` 早的优先
3. `identifier` 字典序

`isDispatchEligible` 当前要求：

- `ID / Identifier / Title / State` 都非空
- 当前状态属于 active states
- 当前状态不属于 terminal states
- 不在 `Running` 中
- 不在 `Claimed` 中（retry 检查时可选择忽略）
- 当 issue 处于 `todo` 时，若存在 blocker 且 blocker 不是终态，则不可调度

`hasAvailableSlots` 同时检查：

- 全局并发上限 `MaxConcurrentAgents`
- 按状态的并发上限 `MaxConcurrentAgentsByState[state]`

### 10.7 Worker 生命周期

`dispatchIssue` 在真正启动 worker 前会：

1. 创建 worker context
2. 将 issue 放入 `Claimed`
3. 停掉已有 retry timer
4. 从 `RetryAttempts` 删除旧记录
5. 把上一次 retry 条目里的 `Attempt/StallCount` 投影到新的 `RunningEntry`
6. 在 `Running` 中创建 `RunningEntry`
7. 刷新并发布快照

worker goroutine 的真实顺序是：

1. `workspace.CreateForIssue`
2. 成功拿到 workspace 后，把 `WorkspacePath` 写回 `RunningEntry` 并发布新快照
3. 若 workspace 支持 `PrepareForRun`，执行 `PrepareForRun`
4. 记录运行前的 PR 上下文
5. 调用 `runner.Run`
6. 若 workspace 支持 `FinalizeRun`，执行 `FinalizeRun`
7. 根据 `runner.Run` 结果构造 `WorkerResult`
8. 若运行成功，检查是否产生关联 PR，并根据 `open` / `merged` / `closed` 进入不同成功路径
9. 通过 `workerResultCh` 回传结果

### 10.8 正常退出与 continuation retry

`handleWorkerExit` 收到成功结果后，不会简单把任务标记为永久完成，而是分三段处理：

#### 路径 A：issue 已经是终态

- 立即调用 `completeSuccessfulIssue`
- 清理工作区
- 释放 `Claimed`

#### 路径 B：本轮运行创建了关联 PR

- `[实现扩展 — 超出 SPEC 边界]` 这是当前 Go 实现加出来的成功路径。
- `[核心规范]` 按 SPEC 16.6，正常 worker 退出后的标准行为应是：记录 bookkeeping 后安排短延迟 continuation retry，而不是由 orchestrator 主动推断 PR 并写 tracker。

当前实现的前提是：

- `detectNewOpenPR` 检测到：
  - 运行后 branch 发生变化
  - 新 branch 不在运行前 open PR heads 里
  - 运行后该 branch 已出现在 open PR heads 里

并且：

- `orchestrator.auto_close_on_pr=true`（默认开启）

然后：

1. 通过 `gh pr list --state all --head <branch>` 查询该 head branch 的真实 PR 状态
2. 若 PR 仍 open：把 issue 放入 `AwaitingMerge`
3. 若 PR 已 merged：尝试 `TransitionIssue("Done")`
4. 再刷新 issue 状态；若已进入终态，则清理工作区并释放 claim
5. 若 merge 后仍未进入终态，则进入错误重试
6. 若 PR 已 closed 但未 merged，则回退到标准 continuation retry

若 `orchestrator.auto_close_on_pr=false`：

1. orchestrator 不再调用 `TransitionIssue`
2. 直接回退到标准 continuation retry 路径

#### 路径 C：运行成功但 issue 仍非终态

当前实现会安排 **continuation retry**：

- `attempt` 重置为 `1`
- 无错误文本
- 固定延迟约 `1s`
- 保留当前 `StallCount`

这部分与 SPEC 的标准路径一致。

### 10.9 失败重试、退避与 stall 计数

`handleWorkerExit` 收到失败结果后会：

- 从 `Running` 移除该 issue
- 计算下一次 `attempt`
- 进入指数退避重试

当前实现的重试公式：

- continuation retry：固定 `1000ms`
- failure retry：
  - `delay = min(10000 * 2^(attempt-1), max_retry_backoff_ms) * (0.5 + rand(0, 0.5))`

`RetryEntry` 当前除了 `Attempt` 之外，还会额外记录：

- `WorkspacePath`
- `Error`
- `StallCount`

这几个字段分别用于：

- `/api/v1/{identifier}` 展示 retry 详情
- 从 retry 重新 dispatch 时恢复工作区上下文和 stall 历史
- 在快照层生成 issue 级告警

当前实现对 stall 的语义是：

- 只有当错误文本匹配 `stalled session` 时，才会递增 `StallCount`
- `Attempt` 代表“本轮 retry 编号”，会被普通失败、slot 不足、retry poll 失败、continuation retry 复用
- `StallCount` 代表“真实发生过多少次 stall”
- `repeated_stall` 告警只会在 `StallCount > 1` 时触发，而不是简单依赖 `Attempt > 1`

边界说明：

- `[核心规范]` SPEC 只定义了 failure retry 的指数退避主公式
- `[实现决策]` 当前 Go 版本额外加入了 0.5~1.0 的 jitter，这是 REQ 明确写出的 Go 实现策略

### 10.10 停滞与对账

`reconcileRunning` 每次 tick 都会处理两类问题：

#### 1）停滞检测

- 取 `entry.StartedAt` 或最后一条 Codex 事件时间
- 若超过 `CodexStallTimeoutMS`，则认定会话停滞
- 停止 worker，并安排错误重试
- 若错误文本为 `stalled session`，会把 `StallCount + 1`

#### 2）tracker 状态对账

- 调用 `FetchIssueStatesByIDs` 刷新所有运行中 issue 的状态
- 若 issue 已进入终态：
  - 停止 worker
  - 清理工作区
  - 释放 claim
- 若 issue 仍在 active states：
  - 更新 `RunningEntry.Issue`
- 若 issue 既非 active 也非 terminal：
  - 停止 worker
  - 释放 claim

细化说明：

- 若停滞检测已经把所有 running issue 都移出，本轮会直接跳过 `FetchIssueStatesByIDs`
- 若状态刷新失败，会：
  - 保持剩余 worker 继续运行
  - 设置 `tracker_unreachable`
  - 发布带告警的快照
- 若状态刷新成功，会清除 `tracker_unreachable`，再根据 refreshed state 更新 / 终止各个 running entry

### 10.11 retry timer 的真实路径

`handleRetryTimer` 的逻辑不是“直接重跑上一个 issue”，而是：

1. 取出并删除当前 `RetryEntry`
2. 再次拉取 active candidate issues
3. 在 candidate 列表中按 `issueID` 查找目标 issue
4. 根据结果分支：
   - 拉取失败：以 `retry poll failed` 再次排队 retry
   - issue 已不存在：释放 claim
   - issue 已不再 eligible：释放 claim
   - 当前没有可用 slot：以 `no available orchestrator slots` 再次排队 retry
   - 仍然 eligible 且有 slot：调用 `dispatchIssue`

注意：

- retry 不是“脱离 tracker 事实的本地重放”，而是始终以最新 candidate 集合为准
- `RetryEntry.StallCount` 会在 retry poll 失败 / slot 不足 / 成功重新 dispatch 之间继续透传，不会被这些非 stall 事件错误放大

### 10.12 快照、告警与订阅

`refreshSnapshotLocked` 每次都会重新生成完整快照，主要包括：

- `Running[]`
  - `IssueID`
  - `IssueIdentifier`
  - `WorkspacePath`
  - `State`
  - `SessionID`
  - `TurnCount`
  - `LastEvent`
  - `LastMessage`
  - `StartedAt`
  - `Input/Output/TotalTokens`
  - `CurrentRetryAttempt`
  - `AttemptCount`
- `Retrying[]`
  - `IssueID`
  - `IssueIdentifier`
  - `WorkspacePath`
  - `Attempt`
  - `DueAt`
  - `Error`
- `Alerts[]`
  - `systemAlerts`
  - 由 `RetryAttempts` 推导出的 issue 级告警
- `CodexTotals`
- `RateLimits`

当前会生成的主要告警有两类：

1. 系统级告警
   - `dispatch_preflight_failed`
   - `tracker_unreachable`
   - `tracker_terminal_fetch_failed`
2. issue 级告警
   - `repeated_stall`
   - `workspace_hook_failure`

其中：

- `repeated_stall` 需要同时满足“错误文本包含 `stalled session`”和“`StallCount > 1`”
- `workspace_hook_failure` 通过 retry 错误文本里是否包含 `workspace_hook_failed` / `workspace_hook_timeout` 推导
- 告警会按 `code -> issue_identifier -> message` 排序，保证状态面稳定可读

`CodexTotals.SecondsRunning` 的计算也有一个实现细节：

- 它不是单纯累计历史值，而是“累计历史值 + 当前 still-running entries 的在线时长”

### 10.13 重启恢复语义

当前设计是纯内存调度状态，不依赖持久数据库。

重启后：

- 不恢复旧进程里的 retry timer
- 不假定旧的 running session 还能恢复
- 依靠以下动作恢复：
  - 启动时 terminal workspace cleanup
  - 重新轮询 active issues
  - 重新 dispatch 仍然 eligible 的工作

另外一个运行时实现细节是：

- `Completed` 集合现在会按插入顺序做有界保留，默认上限 `4096`
- 已存在的 `issueID` 不会刷新在队列中的位置，因此这里更接近“有界完成集合 + FIFO 驱逐”，而不是严格意义上的 LRU
- 它依然只用于 bookkeeping / 可观测性，不参与重启恢复或调度资格判断

---

## 11. HTTP 状态面与日志可观测性

### 11.1 日志

`internal/logging` 当前提供：

- JSON 格式结构化日志
- 输出到 `stderr`
- 可选同时输出到日志文件
- 多 sink fanout
- sink 写失败时向 warning sink 报警，但尽量不中断主流程

当前会自动脱敏的字段关键字包括：

- `token`
- `secret`
- `password`
- `authorization`
- `api_key`

`WithIssue` / `WithSession` 约定的上下文字段是：

- `issue_id`
- `issue_identifier`
- `session_id`

另外，当前实现已经把多类运行日志统一成结构化字段：

- agent runner 的 `emit` 会输出：
  - `event`
  - `message`
  - `attempt`
  - `run_phase`
  - `session_id`
  - `thread_id`
  - `turn_id`
  - `codex_pid`
- orchestrator 的 dispatch / worker finished 日志会输出：
  - `issue_id`
  - `issue_identifier`
  - `attempt`
  - `run_phase`
  - `success`
  - `error`

其中 `run_phase` 使用 `RunPhase.String()` 统一序列化，当前会落成：

- `preparing_workspace`
- `building_prompt`
- `launching_agent`
- `initializing_session`
- `streaming_turn`
- `finishing`
- `succeeded`
- `failed`
- `timed_out`
- `stalled`
- `canceled_by_reconciliation`

### 11.2 运维可见性要求

- `[核心规范]` 启动失败、校验失败、dispatch 失败必须在不附加 debugger 的情况下对 operator 可见
- `[核心规范]` 日志 sink 失败时，服务应尽量继续运行，并通过剩余 sink 报告 warning

### 11.3 HTTP 服务的规范地位

- `[实现扩展]` HTTP server 是可选扩展，不是 conformance 必需项
- `[核心规范]` 若实现，HTTP API 只能作为 observability / operational trigger，不应成为 orchestrator 正确性的前提

### 11.4 启动方式

当配置中 `server.port` 存在，或 CLI 传入 `--port` 时：

- `internal/server.Start` 会在 `127.0.0.1:<port>` 上监听

### 11.5 端点列表

| 方法 | 路径 | 作用 |
|---|---|---|
| `GET` | `/` | 简单 Dashboard |
| `GET` | `/api/v1/state` | 全局状态快照 |
| `POST` | `/api/v1/refresh` | best-effort 触发一次立即轮询 |
| `GET` | `/api/v1/events` | SSE 快照流 |
| `GET` | `/api/v1/{identifier}` | 按 issue identifier 查询单个 issue 状态 |

### 11.6 关键响应格式

`GET /api/v1/state` 当前包含：

- `generated_at`
- `service.version`
- `service.started_at`
- `service.uptime_seconds`
- `counts.running`
- `counts.retrying`
- `running[]`
- `retrying[]`
- `alerts[]`
- `codex_totals`
- `rate_limits`

其中各数组的实际序列化字段是：

- `running[]`
  - `issue_id`
  - `issue_identifier`
  - `state`
  - `session_id`
  - `turn_count`
  - `last_event`
  - `last_message`
  - `started_at`
  - `last_event_at`
  - `current_retry_attempt`
  - `tokens`
- `retrying[]`
  - `issue_id`
  - `issue_identifier`
  - `attempt`
  - `due_at`
  - `error`
- `alerts[]`
  - `code`
  - `level`
  - `message`
  - 可选 `issue_id`
  - 可选 `issue_identifier`

需要注意：

- orchestrator 内部快照里的 `WorkspacePath` / `AttemptCount` 并不会完整直接透传到 `/api/v1/state`
- 这些更偏“单 issue 诊断”的字段主要通过 `/api/v1/{identifier}` 暴露
- `current_retry_attempt` 和 `attempt_count` 不是同一个概念：
  - `current_retry_attempt`：running entry 里保存的 retry attempt 原值，`0` 表示首轮运行，`1` 表示经历过一次 continuation / retry 后再次运行
  - `attempt_count`：面向操作者的人类可读次数，规则是 `retryAttempt <= 0 -> 1`，否则 `retryAttempt + 1`

`GET /api/v1/{identifier}` 当前实现返回：

- `generated_at`
- `identifier`
- `status`
- `workspace_path`
- `last_error`
- `attempt_count`
- `running`
- `retry`

说明：

- 这里的 `{identifier}` 匹配的是 `IssueIdentifier`，不是 tracker 的 `IssueID`
- `status` 当前只有 `running` / `retrying` 两类
- 当 `status=running` 时，`attempt_count` 来自 `RunningSnapshot.AttemptCount`
- 当 `status=retrying` 时，`attempt_count` 直接等于当前 `RetryEntry.Attempt`
- `[实现决策]` 当前返回已补充 `workspace_path` / `last_error` / `attempt_count`，但仍比 SPEC 推荐形状更精简，没有把 recent_events、logs、tracked 等调试信息全部暴露出来

`POST /api/v1/refresh` 当前返回：

- `queued`
- `coalesced`
- `requested_at`
- `operations`

但要注意它的真实语义：

- HTTP handler 只是调用一次 `orchestrator.RequestRefresh()`
- `RequestRefresh()` 通过非阻塞 send 往 `refreshCh` 投递信号
- 若 channel 已满，这次请求会被**合并/丢弃**
- 因此 `202 Accepted` 表示“已接受刷新请求”，不等价于“新 tick 一定已经排队成功”

`service` 对象的语义是：

- `version`：当前服务版本；默认值为 `dev`，可由启动入口覆盖
- `started_at`：当前服务实例启动时间
- `uptime_seconds`：`generated_at - started_at` 的秒数差

### 11.7 方法与错误处理

- `[核心规范]` 已定义路由上的不支持 HTTP 方法返回 `405`
- `[核心规范]` API 错误使用 JSON envelope：
  - `{"error":{"code":"...","message":"..."}}`
- `[实现决策]` SSE 事件名当前使用 `snapshot` / `update`
- `SubscribeSnapshots` 会先推送一次当前快照，因此每个 SSE 连接的第一条事件总是 `snapshot`
- `[实现决策]` 若 `http.ResponseWriter` 不支持 `http.Flusher`，当前会返回 `500`，错误码是 `stream_not_supported`
- `[实现决策]` SSE 客户端断开时，handler 应随 request context 结束而退出，并释放订阅资源
- `[实现决策]` 多个 SSE 客户端可以并发订阅；每个连接都会各自先收到 `snapshot`，之后再收到独立的 `update`

### 11.8 Dashboard

Dashboard 页面当前是一个简单 HTML：

- 首次请求 `/api/v1/state`
- 再通过 `EventSource('/api/v1/events')` 实时刷新

---

## 12. 安全与操作安全

### 12.1 当前信任边界

当前实现应被理解为**高信任部署**：

- 自动批准 approval 请求
- 默认 `workspace-write` sandbox
- 支持任意 shell hooks
- agent 可访问工作区内容

因此需要明确：

- `[核心规范]` 路径隔离和 workspace 校验只是基线控制
- `[核心规范]` 它们不能替代 approval / sandbox / 外部隔离策略

### 12.2 Secret 处理

当前实现满足以下基线：

- 支持 `$VAR` 间接引用敏感配置
- 不在日志里打印 token 或 secret 原文
- 脱敏关键字段为 `***masked***`

### 12.3 Hook 脚本安全

根据当前实现与 SPEC，hooks 需要这样理解：

- hooks 来自 `WORKFLOW.md`
- hooks 是**完全受信任配置**
- hooks 在工作区目录内运行
- hook 输出应截断后写日志
- hook 必须有超时，避免拖死 orchestrator

### 12.4 Harness Hardening 指引

SPEC 没有强制统一安全姿态，但明确建议按部署风险加固。适合当前实现的加固方向包括：

- 收紧 approval 和 sandbox 设置，而不是默认高信任
- 额外叠加 OS / 容器 / VM 隔离
- 限制可调度的 tracker project / issue 来源
- 缩窄 `linear_graphql` 工具访问范围
- 缩小 agent 可见的凭据、路径、网络目的地集合

---

## 13. Shell 与运行环境

`internal/shell.BashCommand` 会统一使用：

- `bash -lc <script>`

在 Windows 下：

- 优先显式寻找 Git for Windows 的 `bash.exe`
- 找不到时才回退到 `PATH` 中的 `bash`

这样可以避免误命中 WSL 启动器导致行为不稳定。

---

## 14. 测试与验证 Profile

### 14.1 三种验证 profile

按照 SPEC 17，验证体系分三类：

- `Core Conformance`
  - 所有 conforming implementation 都必须覆盖
- `Extension Conformance`
  - 只对已交付的可选能力要求，例如 HTTP server、`linear_graphql`
- `Real Integration Profile`
  - 真实环境下的 smoke / integration 验证，建议在发布前执行

### 14.2 当前仓库中的落点

- `go test ./...`
  - 覆盖 Core + 已交付扩展的单测
- `go test -cover ./...`
  - 覆盖率版本
- `go test -tags=integration ./...`
  - 真实集成测试（默认不跑，需要显式 build tag）

当前已经落地的 integration seed tests 包括：

- `internal/tracker/linear_integration_test.go`
  - `TestLinearIntegration_FetchCandidates`
  - `TestLinearIntegration_FetchByIDs`
- `cmd/symphony/main_integration_test.go`
  - `TestMainIntegration_DryRun`
- `internal/testutil/RequireEnv`
  - 统一处理 `LINEAR_API_KEY` 缺失时的 skipped 语义

### 14.3 真实集成测试要求

- 需要真实凭据，例如 `LINEAR_API_KEY`
- 可选用 `LINEAR_PROJECT_SLUG` 覆盖 `WORKFLOW.md` 中的默认项目
- 应使用隔离测试标识符 / 工作区
- 应在可能时清理 tracker 侧产物
- `[核心规范]` skipped 的真实集成测试必须报告为 skipped，不能被静默当作 passed

---

## 15. 当前真实的端到端运行序列

下面用一条“活跃 Linear issue 被处理并最终完成”的路径概括整个系统。

### 15.1 启动阶段

1. 用户执行 `go run ./cmd/symphony ...`
2. CLI 读取 `WORKFLOW.md`
3. front matter 被解析为 `ServiceConfig`
4. logger 初始化
5. tracker / workspace / runner / orchestrator 被构造
6. orchestrator 做启动终态工作区清理
7. HTTP 服务（如果启用）启动
8. 首轮 tick 立即触发

### 15.2 调度阶段

1. orchestrator 先对账当前运行中任务
2. 向 Linear 拉取 candidate issues
3. 按优先级、创建时间、identifier 排序
4. 过滤掉：
   - 缺少关键字段的 issue
   - 非 active issue
   - terminal issue
   - 已 running / claimed 的 issue
   - 有 blocker 的 `todo` issue
   - 超过并发上限的 issue
5. 对合格 issue 调用 `dispatchIssue`

### 15.3 工作区阶段

1. 创建或复用 issue 工作区
2. 运行 `after_create`（仅新建时）
3. 清理 `tmp`、`.elixir_ls`
4. 运行 `before_run`
5. 若工作区内存在 Git 仓库，则准备工作分支

### 15.4 Agent 会话阶段

1. 启动 `codex app-server`
2. 完成 `initialize -> initialized -> thread/start -> turn/start`
3. 第 1 个 turn 使用 `WORKFLOW.md` 的 prompt 模板
4. 若发生 approval 请求，自动批准
5. 若发生 `linear_graphql` 工具请求，由 runner 直接代为访问 Linear
6. 若 agent 请求用户输入，立刻视为失败
7. turn 完成后刷新 issue 状态
8. 若仍为 active 且没到 `MaxTurns`，进入下一 turn

### 15.5 结果处理阶段

#### 失败路径

- worker 返回错误
- orchestrator 进入指数退避重试

#### 成功路径

- 若 issue 已是终态：清理工作区并释放
- 若发现关联 PR：
  - PR open：进入 `AwaitingMerge`
  - PR merged：`[实现扩展 — 超出 SPEC 边界]` orchestrator 尝试把 issue 转为 `Done`
  - PR closed/unmerged：回退 continuation retry
- 若 issue 仍是活跃态：
  - `[核心规范]` 安排 continuation retry

### 15.6 关闭阶段

1. 收到中断信号
2. 停止轮询和 retry timer
3. 取消所有运行中 worker
4. 最多等待 2 秒
5. 关闭 HTTP server
6. 输出停止日志并退出

---

## 16. 预留点、限制与与 SPEC 的差异

### 16.1 已有预留但未完全打通

- GitHub tracker：
  - 文档与 prompt 中已有说明
  - workspace 分支命名也已预留
  - 但 `ValidateForDispatch` 和 tracker 工厂当前仍只支持 Linear

### 16.2 当前实现限制

- dry-run 仍会访问 tracker，并可能清理终态工作区
- 工作流热更新只更新 config / prompt，不会重建已创建的 tracker / workspace / runner 对象
- `linear_graphql` 仅在 agent 会话里可用，不是独立 CLI 功能
- 成功运行后是否真正结束，不由“本轮无报错”决定，而由 issue 是否进入终态决定
- HTTP server 已启动后不做 live rebind

### 16.3 与 SPEC 的主要差异点

- `[实现扩展]` `TransitionIssue` 作为独立接口存在
- `[实现扩展 — 超出 SPEC 边界]` orchestrator 内部做了 PR 检测 + tracker 状态写入
- `[实现扩展]` workspace 层内置了 Git 分支准备
- `[实现扩展]` HTTP server 与 `server.port` 扩展键已 shipped

---

## 17. 结论

当前这套代码已经形成一条完整闭环：

- **配置层**：`WORKFLOW.md` → `WorkflowDefinition` → `ServiceConfig`
- **调度层**：`orchestrator` 轮询、筛选、dispatch、重试、对账
- **执行层**：`workspace` + `agent runner`
- **集成层**：Linear GraphQL + 可选 HTTP 状态面
- **安全基线**：workspace root containment、sanitized workspace key、agent cwd 固定到 workspace

同时也要明确：

- 它已经不只是一个“纯 tracker reader + scheduler”的最小实现
- 当前 Go 版本加入了若干工程化扩展，尤其是：
  - `linear_graphql`
  - HTTP 状态面
  - Git 分支准备
  - PR 检测 + issue 主动状态转换

因此，阅读或继续扩展本项目时，应该始终区分：

- 哪些是 `SPEC` 的跨语言核心契约
- 哪些是 Go 版本为了落地效率增加的实现层能力
