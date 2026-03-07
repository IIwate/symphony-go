# Real Integration Profile 验证记录（2026-03-07）

## 结论

本次验证结果为：**通过（Cycle 4 最小真实验证已完成）**。

已确认：

- 真实 `LINEAR_API_KEY` 已用于 live 验证
- 当前主机可用 Git Bash：`GNU bash, version 5.2.37(1)-release (x86_64-pc-msys)`
- 当前 `WORKFLOW.md` 使用真实 `tracker.project_slug: db0a2d0d6058`
- 当前 `WORKFLOW.md` 已更新 `codex.read_timeout_ms: 15000`
- `go test ./...` 通过
- `go run ./cmd/symphony --dry-run` 通过
- `--port` 模式下已验证 `/api/v1/state`、`/api/v1/{identifier}`、`/api/v1/refresh`、`/api/v1/events`
- 真实候选 issue `IIW-5` 可被抓取并进入运行
- `before_run` hook 成功执行 git clone
- 已观察到真实运行中的 `dynamic_tool_call_request` / `dynamic_tool_call_response`
- 结合 live 日志与直接 Linear GraphQL 查询，`linear_graphql` 成功路径与 GraphQL 错误路径已验证

## 关键发现

- 在当前 Windows 11 + `codex-cli 0.111.0` 组合下，`thread/start` 首次响应通常约 9s 到 11s。
- 默认 `read_timeout_ms=5000` 会导致 `response_timeout: response timeout waiting for request id 2`。
- 本轮已将仓库 `WORKFLOW.md` 调整为 `codex.read_timeout_ms: 15000`，并在调整后重新验证通过。
- token totals 在 session 初期可能短暂为 `0`；当 `codex/event/token_count` 或 `thread/tokenUsage/updated` 里的绝对 totals 到达后会正常更新。本轮实测更新到 `15317`。

## 本次执行的检查

- 基线测试：`go test ./...`
- shell 可用性：`bash --version`
- Codex 可用性：`codex --version`、`codex app-server --help`
- 真实 Linear 连通性：viewer 查询 + 候选 issue 查询
- 调度前置：`go run ./cmd/symphony --dry-run`
- 可观测性：`/api/v1/state`、`/api/v1/IIW-5`、`/api/v1/refresh`、`/api/v1/events`
- 真实调度：`IIW-5` 进入运行并产生真实 session
- 真实动态工具：live 日志观察 + 独立 GraphQL 成功/错误查询

## 未覆盖但不阻塞收口

- startup terminal cleanup 对真实终态 issue 工作区的 live 行为
- `after_run` / `before_remove` 的真实环境回收路径
- 更长时间运行后的 continuation / terminal cleanup 长周期观测

## 记录结论

- Core / Extension 测试基线：**通过**
- Real Integration Profile：**通过**
- 本轮收口结论：**Cycle 4 技术验收完成**
- 关键修正：`WORKFLOW.md` 新增 `codex.read_timeout_ms: 15000`
- 备注：版本号、发布时间窗口与最终发布批准仍属于发布管理动作，不在本轮技术收口范围内。
