# Symphony-Go Operator Runbook

## 1. 文档目的

本手册面向 Symphony-Go 的操作者，用于支持以下操作：

- 启动与关闭服务
- 执行 dry-run 与 smoke test
- 观察运行时状态与日志
- 处理常见故障
- 做发布前检查与回滚准备

本手册默认面向当前实现范围：

- 已交付核心调度主流程
- 已交付 HTTP 状态面与 SSE
- 已交付 `linear_graphql` 动态工具扩展

## 2. 运行前提

### 2.1 基础环境

- 操作系统已具备运行 Go 二进制和 `codex app-server` 的条件
- 当前工作目录或显式传入路径下存在可用的 `WORKFLOW.md`
- `WORKFLOW.md` 中配置的 tracker / hooks / codex 参数与目标环境匹配

### 2.2 必要凭证

- 当前版本仅支持 `tracker.kind=linear`：注入 `LINEAR_API_KEY`
- `GITHUB_TOKEN` / `tracker.kind=github` 属于 Cycle 5 之后的扩展，不适用于当前已发布实现
- 不要把 token 明文写入日志、命令历史或共享文档

### 2.3 目录与权限

- `workspace.root` 指向的目录具备创建、写入、删除权限
- 日志文件目录具备追加写权限
- 若启用 hooks，目标主机 shell 环境中可以执行对应脚本
- Windows 主机建议安装 Git for Windows；实现层会优先使用 Git Bash，避免误命中 `C:/Windows/System32/bash.exe`（WSL 启动器）

### 2.4 GitHub Issues tracker 接入约定（Cycle 5 之后适用，当前版本未实现）

- 以下内容对应 `docs/rfcs/github-issues-tracker.md` 的设计草案，不适用于当前只支持 Linear 的版本

- `tracker.owner` / `tracker.repo` 必须指向目标仓库
- 状态标签默认使用 `state_label_prefix=symphony:`，例如 `symphony:todo`、`symphony:in-progress`、`symphony:cancelled`
- 同一 issue 若存在多个 `symphony:*` 状态标签，系统应记录告警并跳过该 issue；先人工清理标签冲突
- GitHub `/issues` 接口返回的 PR 条目会被过滤，不会参与调度
- 参考示例：`docs/examples/WORKFLOW.github-issues.md`

## 3. 启动命令

### 3.1 最小 dry-run

用于做配置和启动链路验证，不会进入持续运行状态。

```powershell
$env:LINEAR_API_KEY="<your-token>"
go run ./cmd/symphony --dry-run
```

若 `WORKFLOW.md` 不在当前目录：

```powershell
$env:LINEAR_API_KEY="<your-token>"
go run ./cmd/symphony --dry-run ./path/to/WORKFLOW.md
```
GitHub Issues tracker 示例（Cycle 5 之后适用，当前版本不要使用）：

```powershell
$env:GITHUB_TOKEN="<your-token>"
go run ./cmd/symphony --dry-run ./docs/examples/WORKFLOW.github-issues.md
```

### 3.2 正常启动（无 HTTP server）

```powershell
$env:LINEAR_API_KEY="<your-token>"
go run ./cmd/symphony --log-level info --log-file ./logs/symphony.log ./WORKFLOW.md
```

### 3.3 正常启动（启用 HTTP server）

```powershell
$env:LINEAR_API_KEY="<your-token>"
go run ./cmd/symphony --port 8080 --log-level info --log-file ./logs/symphony.log ./WORKFLOW.md
```

### 3.4 常用参数

- `WORKFLOW.md` 路径：可选位置参数；未传时默认 `./WORKFLOW.md`
- 当前 CLI 基于标准库 `flag`，请将 `--dry-run` / `--port` / `--log-file` / `--log-level` 写在路径参数之前
- `--dry-run`：执行单次 poll cycle 验证后退出；注意当前实现仍会访问 tracker，并执行 `startupCleanup`
- `--port`：启用 HTTP server，当前实现绑定 loopback 地址
- `--log-file`：同时输出到 stderr 与指定文件
- `--log-level`：`debug` / `info` / `warn` / `error`

## 4. 启动后应观察什么

### 4.1 日志基线

启动成功后，通常应该能看到：

- workflow 已加载
- 服务已启动
- 若启用 HTTP server，server 启动地址已打印

### 4.2 状态面基线

若启用 `--port`，可用以下端点确认服务状态：

- `GET /`：Dashboard 页面
- `GET /api/v1/state`：全局快照
- `POST /api/v1/refresh`：触发一次立即 refresh
- `GET /api/v1/events`：SSE 流

其中：

- `/api/v1/state` 会返回 `alerts` 字段，用于汇总 tracker 不可达、重复 stall、workspace hook 失败、PR merge 状态未知等需要 operator 关注的问题
- `/api/v1/state` 也会返回 `service.version`、`service.started_at`、`service.uptime_seconds`，用于快速确认当前进程版本和在线时长
- `/api/v1/state` 也会返回 `awaiting_merge`、`awaiting_intervention` 以及对应计数，用于确认哪些 issue 正在等待 PR 合并或等待人工介入
- `/api/v1/{identifier}` 会返回 `status`、`workspace_path`、`last_error`、`attempt_count`，便于按单 issue 排障
- 服务内部的 `Completed` 集合默认最多保留 `4096` 个 issue ID，仅用于 bookkeeping / 可观测性；它不是完整历史，也不参与重启恢复

建议先检查：

```powershell
curl http://127.0.0.1:8080/api/v1/state
```

## 5. Workflow 热加载操作

- Symphony-Go 启动后会监听 `WORKFLOW.md`
- 文件变更后会尝试 reload
- 若 reload 成功，新配置会应用到后续 dispatch / retry / reconcile
- 若 reload 失败，系统保留最后一次有效配置，并记录 warning 日志
- `orchestrator.auto_close_on_pr` 也支持热更新；默认值为 `true`

### 建议操作方式

- 先修改一小处可验证字段，再观察日志是否出现 reload 成功
- 对高风险字段（tracker、workspace.root、hooks、codex、orchestrator.auto_close_on_pr）变更，先用 `--dry-run` 验证再上服务

## 6. Smoke Test 操作指南

### 6.1 配置烟测

- 当前版本仅支持 `tracker.kind=linear`：设置 `LINEAR_API_KEY`
- 若 `--dry-run` 成功，表示当前 workflow/config 基本可用；但它仍会访问 tracker，并可能执行启动清理副作用
- 执行 `go run ./cmd/symphony --dry-run`
- 期望结果：退出码为 0，日志出现“dry-run 校验通过”

### 6.2 调度烟测

- 准备一条隔离的测试 issue
- 正常启动 Symphony-Go
- 观察是否：
  - 发现候选 issue
  - 创建或复用工作区
  - 启动 agent 会话
  - 完成至少一次 turn

### 6.3 HTTP / SSE 烟测

- 打开 Dashboard：`http://127.0.0.1:<port>/`
- 获取状态：`/api/v1/state`
- 打开 SSE：`/api/v1/events`
- 触发 refresh：

```powershell
curl -X POST http://127.0.0.1:8080/api/v1/refresh
```

## 7. 日志使用说明

### 7.1 推荐配置

- 常规运行：`--log-level info`
- 故障排查：`--log-level debug`
- 建议始终配置 `--log-file`

### 7.2 关键字段

- `issue_id`
- `issue_identifier`
- `session_id`

### 7.3 日志判读建议

- `Info`：正常流程、启动、reload、调度、完成
- `Warn`：可恢复问题，如 tracker 获取失败、reload 失败、hook 忽略型错误
- `Error`：启动失败、关键依赖失败、持续异常

### 7.4 安全注意事项

- 不要在日志中记录 API token
- 看到 `***masked***` 属于预期行为

## 8. 常见故障与处理

### 8.1 `missing_workflow_file`

现象：启动即失败。

处理：

- 检查位置参数路径是否正确
- 检查当前工作目录是否存在 `./WORKFLOW.md`

### 8.2 dispatch preflight 校验失败

常见原因：

- `tracker.kind` 缺失或不支持
- `tracker.kind=linear` 但 `LINEAR_API_KEY` 未注入或 `tracker.project_slug` 缺失
- 若误填 `tracker.kind=github`，当前版本会直接在 preflight 阶段判定为 unsupported tracker kind
- `codex.command` 为空

处理：

- 先执行 `--dry-run`
- 修复配置后再启动正式服务

### 8.3 没有候选 issue 被调度

常见原因：

- 当前没有活跃状态 issue
- Todo issue 被非终态 blocker 阻塞
- Linear 默认开启 `tracker.linear.children_block_parent=true`，因此父任务也可能被未完成子任务阻塞
- 并发槽位耗尽
- issue 已处于 claimed / running / awaiting_merge / awaiting_intervention / retrying
- 若你在对照 GitHub tracker 草案排查：注意这些规则尚未在当前版本实现

处理：

- 检查 `/api/v1/state`
- 检查日志中的 `retrying` / slots / blocker 相关信息
- 若是 Linear 父任务未被调度，检查 UI / GraphQL 中该 issue 是否仍有未终态 children
- 手动调用 `/api/v1/refresh`

### 8.4 工作区问题

常见原因：

- `workspace.root` 无权限
- 路径逃逸被拒绝
- 已存在同名文件而不是目录
- hooks 执行失败或超时

处理：

- 检查工作区根目录权限
- 检查 issue identifier 是否产生了异常路径
- 检查 hooks 是否可在目标 shell 中运行

### 8.5 Agent 会话问题

常见原因：

- `codex app-server` 不存在
- response timeout / turn timeout
- `codex.stall_timeout_ms` 过小，长时间无 Codex 事件时被判定为 stalled session
- user-input-required 被按策略硬失败
- Windows 上 `bash` 命中了 WSL 启动器而不是 Git Bash
- 子进程意外退出

处理：

- 检查 `codex.command`
- 提升日志级别到 `debug`
- 检查目标环境对 `codex app-server` 的可执行性
- 检查 `/api/v1/state` 的 `alerts` 和 issue 级 `last_error`，确认是否出现 repeated stall / `stalled session`
- 结合日志里的 `session_id`、`turn_id`、最后一条 agent event，判断是真停滞还是 `codex.stall_timeout_ms` 阈值过紧
- 对确实会长时间无输出的任务，临时调大 `codex.stall_timeout_ms`；若仅用于短时排障，也可设为 `<=0` 暂时关闭 stall detection
- 在 Windows 上用 `Get-Command bash` 或 `where.exe bash` 确认优先命中 Git Bash；该兼容逻辑为保留项，不应删除

### 8.6 AwaitingMerge / AwaitingIntervention / PR merge gating 问题

常见原因：

- 关联 PR 仍为 open，系统正在等待 merge，而不是继续重跑 agent
- `gh` CLI 不可用、未认证，或当前工作区无法按 head branch 查询 PR
- PR 状态查询失败，`/api/v1/state` 中出现 `merge_status_unknown` 告警
- PR 已 closed 但未 merged，issue 被转入 `awaiting_intervention`，不会自动 retry / re-dispatch

处理：

- 先看 `/api/v1/state` 的 `awaiting_merge` / `awaiting_intervention` 列表，确认 `branch`、`pr_state`、`last_error`
- 在对应工作区内手动执行 `gh pr list --state all --head <branch> --json number,url,state,mergedAt,headRefName`
- 若 PR 已 merged 但 issue 未收口，检查 tracker 状态流转权限和日志中的 `post-merge transition` 错误
- 若 PR 已 closed 但未 merged，按人工决策处理：手动收口 issue，或先把 issue 调整到非 active state 释放 claim，再恢复到 active state 重新参与调度

### 8.7 HTTP / SSE 问题

常见原因：

- 端口被占用
- 未传 `--port`
- 仅绑定在 `127.0.0.1`，从远端主机访问不到

处理：

- 检查启动参数是否包含 `--port`
- 检查本机端口占用
- 在本机访问 `127.0.0.1:<port>` 验证，而不是直接从外部地址访问

### 8.8 GitHub API 权限或限流问题（Cycle 5 之后适用，当前版本未实现）

常见原因：

- `GITHUB_TOKEN` scope / fine-grained 权限不足
- `X-RateLimit-Remaining` 过低或命中 secondary rate limit
- `tracker.owner` / `tracker.repo` 指向了错误仓库

处理：

- 检查 token 是否具备仓库 `Issues: Read` 权限
- 提高 `polling.interval_ms`，避免高频轮询
- 观察日志中的 rate limit 告警与 GitHub 返回头
- 用示例文件 `docs/examples/WORKFLOW.github-issues.md` 对照当前配置

## 9. 关闭与回滚

### 9.1 正常关闭

- 推荐发送 `SIGINT` / `SIGTERM`
- 服务会停止轮询、等待 worker 结束、关闭 HTTP server、再退出

### 9.2 紧急关闭

- 只有在正常关闭失效时才使用强制终止
- 强制终止后，下一次启动依赖 tracker 与工作区状态自行恢复

### 9.3 回滚前检查

- 记录当前版本号 / 提交哈希
- 确认工作区目录和日志路径无需额外迁移
- 确认回滚版本与当前 `WORKFLOW.md` 契约兼容

## 10. 发布前建议操作顺序

1. 本地 `go test ./...`
2. 目标环境 `--dry-run`
3. 启动服务并完成最小 smoke test
4. 若启用 HTTP，验证 `/api/v1/state`、`/api/v1/events`、`/api/v1/refresh`
5. 若启用 `linear_graphql`，验证一次成功调用与一次错误调用
6. 完成 `docs/release-checklist.md` 勾选

## 11. 相关文档

- `docs/release-checklist.md`
- `docs/cycles/cycle-04-extension-release.md`
- `docs/rfcs/github-issues-tracker.md`（Cycle 5 草案）
- `docs/examples/WORKFLOW.github-issues.md`（Cycle 5 示例）
- `IMPLEMENTATION.md`
- `REQUIREMENTS.md`
- `SPEC.md`
