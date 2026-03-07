# Real Integration Profile 验证记录（2026-03-07）

## 结论

本次验证结果为：**部分完成，真实集成验证被阻塞**。

已完成：

- 本地代码与扩展能力基线验证通过
- `codex app-server` 命令存在且可调用帮助信息
- 全量单元/集成模拟测试通过

阻塞项：

- 当前环境中 **未设置** `LINEAR_API_KEY`
- 当前主机环境中 `bash` 命令不可用，而当前实现的 hooks 与 agent 启动路径默认依赖 `bash -lc`
- 当前仓库 `WORKFLOW.md` 中的 `tracker.project_slug` 为 `demo`，**推断**更像示例值；在未确认其为真实 Linear 项目标识前，不适合作为生产验证配置

因此，本次无法完成“真实凭证 + 真实 Linear + 真实 shell 环境”的端到端验证，只能形成一份**带阻塞说明的验证记录**。

## 验证日期

- 日期：2026-03-07
- 执行环境：Windows 11 / PowerShell
- 仓库路径：`H:/code/temp/symphony-go`

## 本次执行的检查

### 1. 基线测试

执行命令：

```powershell
go test ./...
```

结果：**通过**

说明：

- `cmd/symphony`
- `internal/agent`
- `internal/config`
- `internal/logging`
- `internal/orchestrator`
- `internal/server`
- `internal/tracker`
- `internal/workflow`
- `internal/workspace`

对应结论：当前代码基线、HTTP 状态面、SSE、`linear_graphql` 扩展和核心调度逻辑在测试环境下通过。

### 2. `LINEAR_API_KEY` 前提检查

执行命令：

```powershell
if ($env:LINEAR_API_KEY) { "LINEAR_API_KEY=present" } else { "LINEAR_API_KEY=missing" }
```

结果：**失败前提 / 缺失**

输出摘要：

```text
LINEAR_API_KEY=missing
```

对应结论：无法执行真实 Linear API smoke test。

### 3. `codex app-server` 可用性检查

执行命令：

```powershell
codex app-server --help
```

结果：**通过**

对应结论：目标主机上存在 `codex` 命令，且 `app-server` 子命令可被调用。

### 4. `bash` 可用性检查

执行命令：

```powershell
bash --version
```

结果：**失败**

输出摘要：

```text
execvpe(/bin/bash) failed: No such file or directory
```

对应结论：当前主机环境下，默认 `bash` 不可用。由于当前实现的 hooks 与 agent 进程启动使用 `bash -lc`，这会阻塞真实运行。

### 5. `--dry-run` 真实前置验证

执行命令：

```powershell
go run ./cmd/symphony --dry-run
```

结果：**失败**

输出摘要：

```text
workflow loaded
missing_tracker_api_key: tracker.api_key is required
exit status 1
```

对应结论：真实运行前置校验被凭证缺失阻塞，符合当前配置状态。

## 本次未能执行的验证项

以下条目属于 `Real Integration Profile`，但本次未完成：

- [ ] 使用真实 `LINEAR_API_KEY` 访问真实 Linear GraphQL endpoint
- [ ] 使用真实 `tracker.project_slug` 完成候选 issue 查询 smoke test
- [ ] 验证 startup terminal cleanup 对真实终态 issue 工作区的行为
- [ ] 在目标 shell 环境执行 hooks（`after_create` / `before_run` / `after_run` / `before_remove`）
- [ ] 以真实配置启动长期运行服务并验证 `/api/v1/state`
- [ ] 以真实配置验证 `/api/v1/events` SSE 增量推送
- [ ] 以真实配置验证 `POST /api/v1/refresh`
- [ ] 以真实凭证验证 `linear_graphql` 成功调用与 GraphQL 错误调用

## 阻塞原因

### 阻塞 1：缺少真实 Linear 凭证

- 现象：`LINEAR_API_KEY=missing`
- 影响：无法执行任何真实 Linear API 验证
- 风险：当前只能证明“代码路径正确”，不能证明“真实服务配置正确”

### 阻塞 2：主机缺少 `bash`

- 现象：`bash --version` 失败
- 影响：当前实现依赖 `bash -lc` 启动 hooks 与 `codex app-server`
- 风险：即使补齐凭证，也可能在运行阶段卡在 shell 启动问题

> 补记（2026-03-07，同日修复）：仓库随后已在实现层增加“Windows 下优先解析 Git Bash”的兜底逻辑，用于规避 PATH 先命中 `C:/Windows/System32/bash.exe`（WSL 启动器）的常见问题。该兼容层属于真实主机环境稳定性修复，不应视为无意义噪音或被顺手删除。

### 阻塞 3：`WORKFLOW.md` 仍是示例值

- 当前配置：`tracker.project_slug: demo`
- 这是**基于仓库内容的推断**：该值更像示例配置，而不是已确认的真实项目 slug
- 影响：即使有真实凭证，仍可能因为项目过滤条件不正确导致查询失败或命中错误项目

## 推荐下一步

### 立即需要补齐

1. 设置真实 `LINEAR_API_KEY`
2. 把 `WORKFLOW.md` 或专用验证用 workflow 文件中的 `tracker.project_slug` 改为真实可用的项目 slug
3. 确认目标环境提供可执行的 `bash`，或在实现层适配 Windows 下的可用 shell

### 补齐后建议重跑

1. `go run ./cmd/symphony --dry-run`
2. `go run ./cmd/symphony ./WORKFLOW.md --port 8080 --log-level debug --log-file ./logs/symphony.log`
3. `GET /api/v1/state`
4. `GET /api/v1/events`
5. `POST /api/v1/refresh`
6. 用隔离测试 issue 执行一次真实调度 smoke test
7. 验证一次 `linear_graphql` 成功调用和一次 GraphQL 错误调用

## 记录结论（用于 Release Checklist）

- Core / Extension 测试基线：**通过**
- Real Integration Profile：**未通过，已记录为阻塞 / skip**
- Skip 原因：
  - 缺失 `LINEAR_API_KEY`
  - 主机缺失 `bash`
  - `WORKFLOW.md` 中 `project_slug` 尚未确认可用于真实环境
