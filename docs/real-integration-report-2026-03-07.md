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

## 追加验证：`IIwate/linear-test` 两轮 smoke（2026-03-07 晚）

### 验证目标

验证以下整条链路在真实环境中可用：

- Linear issue 创建后可被 Symphony-Go 拉取
- issue 可在独立工作区中 clone 目标仓库并启动 agent
- agent 可在仓库内完成文件改动、提交、推送和发起 PR
- 运行结束后，Linear 状态与 Symphony-Go 运行态可正确收敛

### 本轮补充环境

- 目标仓库：`https://github.com/IIwate/linear-test`
- 仓库现状（验证开始前）：仅包含 `test.md`
- 宿主机 GitHub CLI：已登录 `IIwate`，具备 `repo` scope
- 工作区根目录：`H:/code/temp/symphony_workspaces`
- 观测方式：
  - Symphony-Go 状态面：`/api/v1/state`
  - GitHub PR 列表：`gh pr list --repo IIwate/linear-test`
  - 日志文件：`logs/linear-smoke.log`、`logs/linear-smoke-batch2.log`

### 第一轮 smoke（基础文档 / 配置类）

创建并执行以下 3 条 issue：

- `IIW-6`：`docs: 初始化 README 并说明仓库用途`
- `IIW-7`：`chore: 添加最小 .gitignore`
- `IIW-8`：`docs: 新增 CONTRIBUTING 并说明最小协作流程`

结果：

- 3 条 issue 全部进入 `Done`
- 工作区已正确创建在 `H:/code/temp/symphony_workspaces/IIW-6~8`
- 已成功发起/合并 PR：
  - `#1` `chore: 添加最小 .gitignore` → **MERGED**
  - `#2` `docs: 新增 CONTRIBUTING 并说明最小协作流程` → **OPEN**
  - `#3` `docs: 初始化 README 并说明仓库用途` → **OPEN**

### 第二轮 smoke（更接近真实开发）

创建并执行以下 5 条 issue：

- `IIW-9`：`feat: 新增仓库首页 index.html`
- `IIW-10`：`style: 抽离 styles.css 并优化首页布局`
- `IIW-11`：`feat: 为首页增加主题切换功能`
- `IIW-12`：`docs: 更新 README 并补充本地预览说明`
- `IIW-13`：`chore: 添加最小 GitHub Actions 校验工作流`

结果：

- 5 条 issue 全部进入 `Done`
- Symphony-Go 运行态最终收敛为 `running=0`、`retrying=0`
- 已成功发起/合并 PR：
  - `#4` `docs: 更新 README 并补充本地预览说明` → **OPEN**
  - `#5` `feat: 为首页增加主题切换功能` → **MERGED**
  - `#6` `chore: 添加最小 GitHub Actions 校验工作流` → **MERGED**
  - `#7` `style: 抽离 styles.css 并优化首页布局` → **MERGED**
  - `#8` `feat: 新增仓库首页 index.html` → **OPEN**

### 关键观察

- 使用宿主机现有 Git / GitHub 凭证缓存时，`before_run` 内的 `git clone` 与后续 push / PR 流程可正常工作
- agent 已不止能完成纯文档任务，也能完成多文件前端静态页、样式、原生 JS 与 GitHub Actions 配置类任务
- 第二轮执行期间观察到大量 `codex update channel is full` 告警，主要出现在 `IIW-11` 相关会话中；但该问题**未阻塞任务最终完成**
- `IIW-10` 的 PR 已提前合并，但对应会话在一段时间后才从运行态退出；最终状态仍正确收敛

### 追加验证结论

- 以 `IIwate/linear-test` 为目标仓库的两轮 smoke **通过**
- 已验证链路：**Linear → Symphony-Go → 工作区 clone → agent 改动 → commit / push / PR**
- 当前系统已具备处理轻量真实开发任务的能力，不再局限于最小文档 smoke
- 仍需人工决策的部分主要是 PR review / merge，而非自动化链路本身
