# RFC: GitHub Issues Tracker

> **状态**: 草案
> **对应**: ../REQUIREMENTS.md §11.3 "GitHub Issues tracker" / Cycle 5 扩展池
> **前置**: Cycle 1-4 首版交付完成

---

## 1. 目标

为 symphony-go 的 `tracker.Client` 接口新增 GitHub Issues 适配器，使编排器可以从 GitHub Issues 拉取候选任务并跟踪状态。这是首版 Linear-only tracker 的第一个平台扩展。

完成后，用户只需把 `tracker.kind` 改为 `github` 并提供 GitHub Token，即可将 GitHub Issues 作为任务源驱动 agent 派发。

## 2. 范围

### In Scope

- `GitHubClient` 实现 `tracker.Client` 三个方法
- GitHub REST API v3 调用（纯 `net/http`，不引入 GitHub SDK）
- 过滤 `pull_request` 条目，仅读取真正的 Issue
- Labels 模拟状态映射（`symphony:` 前缀约定）
- 配置解析与验证（`tracker.kind=github`）
- 单元测试（`httptest` mock）
- 集成测试可跳过策略

### Out of Scope

- GitHub Projects v2 集成（需 GraphQL + 额外权限）
- GitHub Webhooks 实时推送（仍用轮询）
- Issue 写回（评论、状态变更、label 修改）
- 依赖关系解析（`BlockedBy` 保持 nil）
- `agent.Runner` 扩展（属于下一个主题周期）

## 3. 配置字段

`WORKFLOW.md` 中 `tracker:` 块新增以下字段：

```yaml
tracker:
  kind: github
  api_key: $GITHUB_TOKEN          # PAT，Bearer 认证
  endpoint: https://api.github.com # 可选，默认值；支持 GHES
  owner: octocat                   # 仓库所有者
  repo: my-project                 # 仓库名
  state_label_prefix: "symphony:"  # 可选，默认 "symphony:"
  active_states: [todo, in-progress]
  terminal_states: [closed, cancelled]
```

### 字段说明

| 字段 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `kind` | 是 | — | 值为 `github` |
| `api_key` | 是 | — | GitHub PAT，支持 `$ENV_VAR` 语法 |
| `endpoint` | 否 | `https://api.github.com` | REST API 基地址（GHES 可覆盖） |
| `owner` | 是 | — | GitHub 用户名或组织名 |
| `repo` | 是 | — | 仓库名（不含 owner 前缀） |
| `state_label_prefix` | 否 | `symphony:` | 状态 label 的前缀 |
| `active_states` | 否 | `["todo", "in-progress"]` | 候选状态列表 |
| `terminal_states` | 否 | `["closed", "cancelled"]` | 终止状态列表 |

### 默认值策略

- 当 `tracker.kind=github` 时，`endpoint` 默认值为 `https://api.github.com`
- `active_states` 默认值为 `["todo", "in-progress"]`
- `terminal_states` 默认值为 `["closed", "cancelled"]`
- `state_label_prefix` 默认值为 `symphony:`
- 以上默认值必须按 `tracker.kind` 分支选择，不能沿用 Linear 的 GraphQL endpoint 和状态默认值

### ServiceConfig 新增字段

```go
// internal/model/model.go — ServiceConfig 新增
TrackerOwner            string   // GitHub owner
TrackerRepo             string   // GitHub repo name
TrackerStateLabelPrefix string   // label 前缀，默认 "symphony:"
```

`project_slug` 字段在 `kind=github` 时不使用（`owner` + `repo` 替代）。

完整示例见 `docs/examples/WORKFLOW.github-issues.md`。

### Typed Errors 新增

```go
// internal/model/model.go — TrackerError 新增
ErrMissingTrackerOwner = &TrackerError{Code: "missing_tracker_owner"}
ErrMissingTrackerRepo  = &TrackerError{Code: "missing_tracker_repo"}
```

## 4. GitHub Issue → model.Issue 映射

| GitHub REST 字段 | model.Issue 字段 | 转换规则 |
|---|---|---|
| `number` | `ID` | `strconv.Itoa(number)` |
| — | `Identifier` | `"{owner}/{repo}#{number}"` |
| `title` | `Title` | 直接映射 |
| `body` | `Description` | `*string`，空字符串转 nil |
| — | `Priority` | nil（GitHub 无内建优先级） |
| labels / issue state | `State` | `closed` 优先于 active label；若 issue 已 closed 且存在终态前缀 label（如 `symphony:cancelled`）则使用该终态，否则回退为 `"closed"`；open issue 仅在且仅在存在一个前缀 label 时取其值 |
| — | `BranchName` | `"issue-{number}"` |
| `html_url` | `URL` | 直接映射 |
| `labels[].name` | `Labels` | 全部 label 名（小写、去空格） |
| — | `BlockedBy` | nil（首版不解析依赖） |
| `created_at` | `CreatedAt` | `time.Parse(time.RFC3339, ...)` |
| `updated_at` | `UpdatedAt` | `time.Parse(time.RFC3339, ...)` |

### State 提取优先级

1. 若响应对象包含 `pull_request` 字段，直接跳过，不映射到 `model.Issue`
2. 若 issue state 为 `closed`：
   - 若存在终态前缀 label（如 `symphony:cancelled`），使用该终态值
   - 否则统一回退为 `"closed"`
3. 若 issue state 为 `open`，收集所有以 `state_label_prefix` 开头的 labels
4. 若恰好 1 个匹配，去掉前缀得到 state 值（如 `symphony:in-progress` → `"in-progress"`）
5. 若匹配数 > 1，视为冲突配置：记录告警并跳过该 issue（不参与调度/状态列表）
6. 若无匹配 label，state 为空字符串 — 该 issue 不参与调度

## 5. 候选 Issue 查询规则

### FetchCandidateIssues

查询所有 active 状态的 issue：

```
GET /repos/{owner}/{repo}/issues?state=open&labels={label1},{label2}&per_page=100
```

- `{label1},{label2}` = 将每个 active_state 加上前缀后逗号拼接
  - 例：active_states=["todo","in-progress"], prefix="symphony:"
  - → `labels=symphony:todo,symphony:in-progress`
- **注意**: GitHub REST API `labels` 参数是 AND 语义（必须同时拥有所有 label）
- 因此需要**按 state 逐个查询**再合并去重，而非一次性传入多个 label
- 分页：解析 `Link` response header，循环至无 `rel="next"`
- `/issues` 响应中若包含 `pull_request` 字段，必须在客户端侧过滤掉

```
# 实际查询（每个 active state 一次）
GET /repos/{owner}/{repo}/issues?state=open&labels=symphony:todo&per_page=100
GET /repos/{owner}/{repo}/issues?state=open&labels=symphony:in-progress&per_page=100
```

### FetchIssuesByStates

同 FetchCandidateIssues，但按传入的 `states []string` 参数生成查询；同样需要过滤 PR 并去重。

### FetchIssueStatesByIDs

GitHub REST API 无批量 by-ID（number）查询，需逐个请求：

```
GET /repos/{owner}/{repo}/issues/{number}
```

- 使用 `sync.WaitGroup` + semaphore channel 并发请求，上限 10 goroutine；不引入额外依赖
- 每个响应只需提取 `number`、`state`、`labels`、`title` 用于状态刷新
- 失败语义与现有 `LinearClient` 保持一致：任一请求失败则整个方法返回 error，不返回部分结果
- `/issues/{number}` 若返回 `pull_request` 字段，视为配置/数据错误并返回 operator-visible error

### Rate Limit 处理

- 检查响应头 `X-RateLimit-Remaining`
- 当 `remaining` 较低时记录 warn，便于 operator 调整 `poll_interval_ms`
- 当 `remaining = 0` 或收到 `429` / `403` secondary rate limit 时，读取 `Retry-After` / `X-RateLimit-Reset`
- 实现不得无界 sleep；必须尊重 `ctx.Done()`，并返回带重试提示的 tracker error，由现有 tick / reconciliation 逻辑在后续轮询中重试

## 6. Active/Terminal State 映射

```
GitHub Labels                    model.Issue.State    分类
─────────────────────────────    ─────────────────    ────────
symphony:todo                    "todo"               Active
symphony:in-progress             "in-progress"        Active
(issue closed, no state label)   "closed"             Terminal
symphony:cancelled               "cancelled"          Terminal
```

orchestrator 中的 `isActiveState()` / `isTerminalState()` 使用 `NormalizeState()` 做大小写无关匹配，**无需修改**。

`max_concurrent_agents_by_state` 按 state 值控制并发，例如：

```yaml
agent:
  max_concurrent_agents_by_state:
    todo: 3
    in-progress: 5
```

## 7. 认证方式

| 项目 | 说明 |
|---|---|
| 认证类型 | GitHub Personal Access Token (PAT) |
| 环境变量 | `$GITHUB_TOKEN`（通过 `resolveEnvString` 解析） |
| HTTP Header | `Authorization: Bearer {token}` |
| Accept | `application/vnd.github+json` |
| API Version | `X-GitHub-Api-Version: 2022-11-28`（GitHub.com 默认；GHES 按实例兼容性调整） |
| Classic PAT | 需要 `repo` scope（私有仓库）或 `public_repo`（公开仓库） |
| Fine-grained PAT | 需要仓库的 `Issues: Read` 权限 |
| GHES | 修改 `tracker.endpoint` 为企业实例地址即可 |

现有日志秘密过滤（`logging.go` `redactSecrets`）已覆盖 `api_key` / `token` / `authorization` 关键字，**无需修改**。

## 8. 接口变化

### tracker.Client（不变）

```go
type Client interface {
    FetchCandidateIssues(ctx context.Context) ([]model.Issue, error)
    FetchIssuesByStates(ctx context.Context, states []string) ([]model.Issue, error)
    FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]model.Issue, error)
}
```

接口签名不变。`GitHubClient` 作为新实现接入。

### NewClient 工厂扩展

```go
// internal/tracker/linear.go:102 → 迁移至 client.go
func NewClient(cfg *model.ServiceConfig, httpClient *http.Client) (Client, error) {
    switch cfg.TrackerKind {
    case "linear":
        return NewLinearClient(cfg, httpClient)
    case "github":
        return NewGitHubClient(cfg, httpClient)
    default:
        return nil, model.NewTrackerError(...)
    }
}
```

### NewDynamicClient 同步扩展

```go
func NewDynamicClient(configProvider func() *model.ServiceConfig, httpClient *http.Client) (Client, error) {
    cfg := configProvider()
    switch cfg.TrackerKind {
    case "linear":
        return NewDynamicLinearClient(configProvider, httpClient)
    case "github":
        return NewDynamicGitHubClient(configProvider, httpClient)
    default:
        return nil, model.NewTrackerError(...)
    }
}
```

### ValidateForDispatch 扩展

```go
// internal/config/config.go:115
func ValidateForDispatch(cfg *model.ServiceConfig) error {
    // ...
    switch cfg.TrackerKind {
    case "linear":
        // 现有验证：api_key + project_slug
    case "github":
        // 新增验证：api_key + owner + repo，并应用 github 默认值分支
    default:
        return ErrUnsupportedTrackerKind
    }
    // ...
}
```

- 新增 `ErrMissingTrackerOwner` / `ErrMissingTrackerRepo` typed errors。
- `tracker.kind=github` 时不再复用 Linear 的 `project_slug` 必填约束，改为校验 `owner` + `repo`。

### `ApplyReload` / 热重载规则

本 RFC 继续复用现有 `runtimeState.ApplyReload` gate：

1. `config.NewFromWorkflow`
2. 重新应用 CLI override（例如 `--port`）
3. `ValidateForDispatch`
4. 检测 restart-required 字段
5. 仅在全部通过后替换 last known good

其中 `tracker.kind` 必须列为 restart-required。原因不是配置语义，而是 `newTrackerFactory -> NewDynamicClient(...)` 只在启动时决定具体 client 类型：

```go
if s.config.TrackerKind != newCfg.TrackerKind {
    return nil, fmt.Errorf("tracker.kind changed from %q to %q: restart required", s.config.TrackerKind, newCfg.TrackerKind)
}
```

同一 kind 内的字段仍可热更新：

- `owner` / `repo` / `endpoint` / `state_label_prefix`
- `active_states` / `terminal_states`

因为具体 client 仍通过 `configProvider()` 读取当前配置。

## 9. 与现有模块兼容性

| 模块 | 影响 | 说明 |
|---|---|---|
| `model` | 兼容 | 新增 3 个 ServiceConfig 字段和 2 个 typed errors，不改已有字段 |
| `config` | 兼容 | 新增 github 解析分支、kind-aware 默认值与校验，linear 逻辑不变 |
| `orchestrator` | 无改动 | 通过 `tracker.Client` 接口调用，与具体实现解耦 |
| `agent` | 无改动 | `Runner` 接收 `model.Issue`，与 tracker 来源无关 |
| `server` | 无改动 | API 端点返回的 issue 数据来自 model.Issue |
| `workspace` | 无改动 | 工作目录按 issue ID 创建，与 tracker 无关 |
| `workflow` | 无改动 | Liquid 模板渲染使用 model.Issue 字段 |
| `logging` | 无改动 | 秘密过滤已覆盖 token/api_key/authorization，新增 GitHub header 仍可复用现有脱敏逻辑 |
| `cmd/symphony` | 小改 | 在共享 `ApplyReload` gate 中拒绝 `tracker.kind` 跨实现切换 |

**Core Conformance 不受影响** — 新代码仅在 `tracker.kind=github` 时激活。

## 10. 测试计划

### 单元测试（无外部依赖，CI 常跑）

| 测试 | 覆盖点 |
|---|---|
| `TestGitHubClient_FetchCandidateIssues` | 分页、label 筛选、PR 过滤、issue 映射 |
| `TestGitHubClient_FetchIssuesByStates` | 按 state 查询、去重、PR 过滤 |
| `TestGitHubClient_FetchIssueStatesByIDs` | 并发请求、全成或全错语义 |
| `TestGitHubClient_StateExtraction` | label 前缀匹配、closed 优先、无匹配 |
| `TestGitHubClient_MultipleStateLabels` | 多个前缀 label 冲突时跳过并告警 |
| `TestGitHubClient_RateLimit` | `403` / `429`、retry hint、`ctx.Done()` 尊重 |
| `TestGitHubClient_Auth` | Bearer token、Accept、API Version header 验证 |
| `TestConfig_GitHubValidation` | kind=github 必填字段与 kind-aware 默认值验证 |
| `TestNewClient_GitHub` | 工厂路由到 GitHubClient |
| `TestApplyReload_TrackerKindReject` | 运行中切换 `tracker.kind` 被拒绝并保留 last known good |

所有单元测试使用 `httptest.NewServer` 模拟 GitHub REST API。

### 集成测试（可跳过）

```go
func TestGitHubIntegration(t *testing.T) {
    if os.Getenv("SYMPHONY_GITHUB_INTEGRATION") != "1" {
        t.Skip("set SYMPHONY_GITHUB_INTEGRATION=1 to run GitHub integration tests")
    }
    // 需要 GITHUB_TOKEN + GITHUB_TEST_OWNER + GITHUB_TEST_REPO
}
```

### Core Conformance 回归

现有 `go test ./...` 全部通过，GitHub 适配器不影响已有测试。

## 11. 运维影响

| 项目 | 说明 |
|---|---|
| 新增凭证 | `GITHUB_TOKEN` 环境变量 |
| 端口 | 无新增 |
| 资源消耗 | GitHub API 速率限制：认证用户 5000 req/h；按默认 30s 轮询 + 2 active states = ~240 req/h，远低于上限 |
| 监控项 | `X-RateLimit-Remaining` 值应纳入日志；可选：低于阈值时告警 |
| 依赖变化 | 无新增三方依赖 |
| 热更新限制 | `tracker.kind` 变更需重启；同一 kind 内字段可热更新 |

### 配套文档落点

- `docs/operator-runbook.md`：补充 GitHub tracker 的凭证、label 约定、rate limit 与常见故障处理。
- `docs/examples/WORKFLOW.github-issues.md`：提供可直接复制的最小 `WORKFLOW.md` 示例。

## 12. 风险与回滚

| 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|
| GitHub API 速率限制 | 中 | 轮询被节流 | 监控 remaining header；支持调大 poll_interval_ms；不得用无界 sleep 阻塞请求路径 |
| Labels AND 语义导致查询逻辑复杂 | 低 | 代码略多 | 按 state 逐个查询 + 去重，已在设计中考虑 |
| 大仓库 issue 量过多 | 低 | 分页耗时 | per_page=100 + 只查 open + label 筛选已缩小范围 |
| PAT 权限不足 | 低 | 请求 403 | 文档明确所需 scope；错误信息包含权限提示 |
| 多个状态 label 冲突 | 低 | issue 被跳过 | 记录告警并要求操作者清理冲突 label |

### 回滚方式

1. 将 `tracker.kind` 改回 `linear` 即可完全回退
2. GitHub 适配器代码仅在 `kind=github` 分支中激活，不影响 Linear 路径
3. 若需彻底移除：删除 `github.go`、`github_test.go`，还原 `NewClient` 工厂即可

## 附录：文件改动清单

| 文件 | 改动类型 | 说明 |
|---|---|---|
| `internal/model/model.go` | 修改 | ServiceConfig 新增 3 个字段 |
| `internal/config/config.go` | 修改 | 解析 github 配置 + ValidateForDispatch 扩展 |
| `internal/tracker/client.go` | 新建 | 抽取 Client 接口 + NewClient/NewDynamicClient 工厂 |
| `internal/tracker/github.go` | 新建 | GitHubClient 实现 |
| `internal/tracker/github_test.go` | 新建 | 单元测试 + 可跳过集成测试 |
| `internal/tracker/linear.go` | 微调 | 移除 Client 接口和 NewClient 工厂（已移至 client.go） |
| `cmd/symphony/main.go` | 小改 | 在 `ApplyReload` 中加入 `tracker.kind` restart-required 检测 |
| `docs/operator-runbook.md` | 修改 | 补充 GitHub Token、label 约定与 rate limit 排障说明 |
| `docs/examples/WORKFLOW.github-issues.md` | 新建 | 提供 GitHub Issues tracker 的最小可复制示例 |
| `docs/cycles/cycle-05-post-mvp.md` | 微调 | 补充 RFC 链接 |
