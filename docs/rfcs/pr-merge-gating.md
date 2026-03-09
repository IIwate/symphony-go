# RFC: PR Merge Gating

> **状态**: 草案
> **对应**: Cycle 5 扩展池 "PR Merge Gating" / `docs/cycles/cycle-05-post-mvp.md`
> **前置**: Cycle 1-4 首版交付完成

---

## 1. 目标

为 `symphony-go` 增加“等待 PR 合并后再收口 issue / 放行后续 blocker issue”的能力，使 orchestrator 不再在“发现 open PR”时立即尝试关闭 issue，而是以“PR 已 merged”作为外部事实信号，再辅助完成 issue 收口。

完成后：

- `orchestrator.auto_close_on_pr=true` 时，含义从“发现 open PR 即尝试关闭”调整为“检测到 PR merged 后再尝试关闭”
- 成功创建 PR 的 issue 不会被立即重新 dispatch，而是进入明确的 `AwaitingMerge` 运行时状态
- 依赖该 issue 的 blocker task 可以继续复用现有 blocker 规则，在前置 issue 被 merge-aware auto-close 后自动放行
- merge 状态以外部系统观测结果为准，不依赖 agent 自觉关闭 issue

## 2. 范围

### In Scope

- merge-aware auto-close：仅在 PR merged 后调用 `TransitionIssue("Done")`
- `AwaitingMerge` 作为 orchestrator 一等运行时状态
- PR 状态查询抽象与 GitHub CLI 默认实现
- 运行时快照中暴露 awaiting-merge 信息
- 合并后收口、未合并关闭、状态查询失败的处理语义
- 对现有 `auto_close_on_pr` 行为的语义修正和文档更新

### Out of Scope

- 任意 PR provider 的统一插件系统
- merge queue / stacked PR / branch protection 细节建模
- 直接解析 GitHub review 线程或 CI 结论
- 多仓库、多远端的 PR 关联
- 取代现有 issue blocker 模型的新依赖图系统

## 3. 核心设计决策

### 3.1 `auto_close_on_pr` 改为 merge-aware，而不是 open-PR-aware

当前实现里，`orchestrator.auto_close_on_pr=true` 时，只要检测到“本轮运行创建了新的 open PR”，就会立即尝试 `TransitionIssue("Done")`。这会把“PR 已创建”和“工作已真正可交付”混为一谈。

本 RFC 明确将其语义调整为：

- `auto_close_on_pr=false`：保持现有 continuation retry 路径
- `auto_close_on_pr=true`：成功运行后若检测到关联 PR，则进入 `AwaitingMerge`
- 只有在 PR merged 后，才触发 `TransitionIssue("Done")`

### 3.2 复用现有 issue blocker，不新增 PR 依赖图

本 RFC 不新增“PR A 阻塞 issue B”的独立模型，而是复用现有 issue blocker 规则：

- 前置 issue 在 PR merge 前保持非终态
- 后置 issue 仍通过现有 `BlockedBy` + `Todo` blocker 规则阻塞
- 当前置 issue 因 merge-aware auto-close 进入终态后，后置 issue 自动放行

也就是说，这个 RFC 解决的是“让 blocker issue 在 merge 时被外力正确收口”，而不是新增一套平行依赖系统。

### 3.3 `AwaitingMerge` 是一等 runtime state

“成功退出但等待 PR merge”不能继续伪装成 retry，也不能回到普通 candidate。

因此首版新增一等运行时状态：

- worker 已结束
- issue 暂不重跑
- workspace 保留
- orchestrator 定期观察 PR 状态

这既避免无意义 continuation retry，也为后续 TUI / Session 持久化 / Notifications / Reactions 提供稳定状态面。

### 3.4 merge 状态以外部观测为准

merge 状态不信 agent 文本输出，不信 prompt 约定，只信外部系统可验证事实：

- 当前默认实现：workspace 内通过 `gh` CLI 读取 PR 状态
- 后续若支持其他 provider，再扩展抽象层

## 4. 当前行为与问题

### 4.1 当前真实行为

当前 `handleWorkerExit` 的成功路径是：

1. 若 issue 已是终态，直接 `completeSuccessfulIssue`
2. 否则若检测到新的 open PR 且 `auto_close_on_pr=true`
   - 调用 `TransitionIssue(issueID, "Done")`
   - 再刷新 issue 状态
   - 若已终态则清理并释放
   - 否则进入错误重试
3. 若未检测到 open PR，则进入 continuation retry

### 4.2 问题

这种实现有三个现实问题：

1. open PR 不等于 merged PR  
   当前收口时机过早，不能表达“等待合并后再放行下游任务”。

2. 依赖 agent 自主关闭 issue 不可靠  
   实践中 agent 经常创建 PR 后忘记关闭 issue，只能依赖外部辅助。

3. continuation retry 会错误重跑  
   对“已经开 PR、只等 merge”的 issue 来说，继续短间隔重跑 agent 没有意义。

## 5. PR 状态模型

### 5.1 `PullRequestInfo`

```go
type PullRequestInfo struct {
    Number     int
    URL        string
    HeadBranch string
    State      string // "open" | "merged" | "closed"
    MergedAt   *time.Time
}
```

### 5.2 `AwaitingMergeEntry`

```go
type AwaitingMergeEntry struct {
    IssueID       string
    Identifier    string
    Branch        string
    PR            PullRequestInfo
    WaitingSince  time.Time
    LastCheckedAt *time.Time
}
```

### 5.3 快照暴露

建议在 orchestrator `Snapshot` 中新增：

```go
type AwaitingMergeSnapshot struct {
    IssueID       string
    Identifier    string
    Branch        string
    PRNumber      int
    PRURL         string
    State         string
    WaitingSince  time.Time
    LastCheckedAt *time.Time
}
```

并在 `Snapshot.Counts` 中新增 `AwaitingMerge` 计数。

## 6. PR 查询抽象

### 6.1 抽象接口

```go
type PullRequestLookup interface {
    OpenHeadBranches(ctx context.Context, workspacePath string) (map[string]struct{}, error)
    FindByHeadBranch(ctx context.Context, workspacePath string, branch string) (*PullRequestInfo, error)
}
```

设计目标：

- 兼容当前“运行前/运行后 open PR 检测”逻辑
- 为 merge 状态查询补上正式接口
- 避免把 `gh` 命令散落在 orchestrator 各处

### 6.2 GitHub CLI 默认实现

当前默认实现仍基于 `gh`：

- `OpenHeadBranches`：`gh pr list --state open --json headRefName`
- `FindByHeadBranch`：`gh pr list --state all --head <branch> --json number,url,state,mergedAt,headRefName`

首版继续假设：

- workspace 内是 Git 仓库
- `gh` 可用且已认证

## 7. 调度与收口流程

### 7.1 成功运行后的分支路径

worker 成功退出后：

1. 若 issue 已是终态：立即清理与释放
2. 若检测到关联 PR 且 `auto_close_on_pr=true`：
   - 若 PR 已 merged：立即尝试收口
   - 若 PR 仍 open：进入 `AwaitingMerge`
   - 若 PR 已 closed 且未 merged：回退到 continuation retry
3. 否则：保持现有 continuation retry

### 7.2 `AwaitingMerge` 状态语义

进入 `AwaitingMerge` 后：

- issue 不再 running
- issue 不再执行 continuation retry
- workspace 保留
- issue 继续占有 orchestrator claim，避免重复 dispatch
- tick/reconcile 周期性检查 PR 状态

### 7.3 merged 路径

当观察到 `PR.State == "merged"`：

1. 若 tracker 支持 `IssueTransitioner`，调用 `TransitionIssue(issueID, "Done")`
2. 再刷新 issue 状态
3. 若已进入终态：清理 workspace 并释放 claim
4. 若未进入终态：进入错误重试，并保留明确错误文本

### 7.4 closed-unmerged 路径

当 PR 被关闭但未 merge：

- 清除 `AwaitingMerge`
- 不自动关闭 issue
- 回退到 continuation retry（attempt=1, no error）

这样 agent 可以在下一次运行中处理“PR 被关掉了，需要继续修改或重新提 PR”的情形。

### 7.5 查询失败路径

当 PR 状态查询失败：

- issue 保持在 `AwaitingMerge`
- 记录 warning
- 可选设置系统告警 `merge_status_unknown`
- 不错误关闭 issue，也不重新 dispatch

## 8. 调度资格规则

### 8.1 `isDispatchEligible` 扩展

在现有资格判定基础上新增一条：

- issue 不得处于 `AwaitingMerge`

这条规则的作用不是 gate 下游 issue，而是避免“自己已经开 PR 只等 merge 的 issue”被再次调度。

### 8.2 与 blocker 规则的关系

本 RFC 不修改现有 blocker 规则本身：

- 仍由 blocker issue 的终态来放行下游 issue
- merge-aware auto-close 负责把前置 issue 在 merge 时正确推进到终态

因此，“等待前置 PR 合并后再分配任务”的核心仍然是：

1. 前置 issue 成功创建 PR
2. orchestrator 等待 merged
3. merge 后自动关闭前置 issue
4. 后置 `Todo` issue 通过现有 blocker 规则自然放行

## 9. 配置设计

### 9.1 `WORKFLOW.md` 示例

```yaml
orchestrator:
  auto_close_on_pr: true
```

### 9.2 字段

首版不新增必需配置字段，继续复用：

| 字段 | 默认值 | 新语义 |
|---|---|---|
| `orchestrator.auto_close_on_pr` | `true` | 等待 PR merged 后再尝试关闭 issue |

### 9.3 兼容性说明

这是对现有实现语义的收敛，而不是保留“open PR 即 close issue”的旧行为。

原因：

- 字段名本身更贴近“基于 PR 自动关闭”，而不是“基于 open PR 预关闭”
- 等待 merged 才 close 更符合真实交付边界

### 9.4 `ValidateForDispatch`

首版无需新增 `ValidateForDispatch` 字段校验。  
但若未来新增 merge wait 超时或告警阈值配置，再在该层扩展。

## 10. Orchestrator 集成

### 10.1 状态结构

建议在 `OrchestratorState` 中新增：

```go
AwaitingMerge map[string]*AwaitingMergeEntry
```

### 10.2 `handleWorkerExit` 收口变化

成功路径中的 “new open PR” 分支改为：

- 识别关联 PR
- 写入 `AwaitingMerge`
- 发布快照
- return

不再在此处直接 `TransitionIssue("Done")`。

### 10.3 `tick` / `reconcile` 新增路径

每个 tick 新增：

```text
reconcileRunning
-> reconcileAwaitingMerge
-> ValidateForDispatch
-> FetchCandidateIssues
-> dispatchEligibleIssues
```

这样 merge 观察成为正式调度链的一部分。

## 11. `cmd/symphony/main.go` 集成

本 RFC 首版不要求新增 CLI flag，也不要求修改 `runCLI/execute` 签名以外的入口约束。

若 PR lookup 仍完全内聚在 orchestrator 内，则 `main.go` 无需新增 seam。

若实现时决定把 PR lookup 做成工厂注入，可新增：

```go
var newPRLookupFactory = func(logger *slog.Logger) orchestrator.PullRequestLookup {
    return orchestrator.NewGitHubPRLookup(logger)
}
```

但这不是首版必须项。

## 12. 与现有/计划能力的兼容性

| 模块 / RFC | 关系 | 说明 |
|---|---|---|
| `internal/orchestrator` | 核心改动 | 新增 `AwaitingMerge` 状态和 reconcile 路径 |
| `internal/server` | 扩展 | `/api/v1/state` 新增 awaiting-merge 只读数据 |
| TUI RFC | 受益 | 可直接展示 `AwaitingMerge` 面板或计数 |
| Notifications RFC | 受益 | 后续可新增 `issue_waiting_for_merge` / `issue_merged` 事件 |
| Session 持久化 RFC | 受益 | 可把 `AwaitingMerge` 作为 durable state 的一部分 |
| Reactions RFC | 受益 | 可把 merge 相关状态作为 trigger source |

这也是本 RFC 适合作为前置能力先落地的原因。

## 13. 测试计划

### 13.1 `internal/orchestrator/orchestrator_test.go`

| 测试 | 覆盖点 |
|---|---|
| `TestHandleWorkerExitNewPROpensAwaitingMerge` | 成功运行后进入 `AwaitingMerge`，不直接 close |
| `TestReconcileAwaitingMergeMergedClosesIssue` | PR merged 后触发 `TransitionIssue` 并清理 |
| `TestReconcileAwaitingMergeClosedUnmergedSchedulesContinuation` | PR closed/unmerged 回退 continuation retry |
| `TestDispatchEligibilityRejectsAwaitingMerge` | `AwaitingMerge` issue 不会被重新 dispatch |
| `TestAutoCloseOnPRFalseKeepsCurrentContinuationPath` | feature flag 关闭时保持现有行为 |

### 13.2 `internal/server/server_test.go`

| 测试 | 覆盖点 |
|---|---|
| `TestStateResponseIncludesAwaitingMerge` | `/api/v1/state` 返回 awaiting-merge 信息 |

### 13.3 `cmd/symphony/main_test.go`

若引入 PR lookup seam：

| 测试 | 覆盖点 |
|---|---|
| `TestExecuteUsesPRLookupFactory` | 工厂注入路径可替换 |

### 13.4 Core Conformance 回归

`go test ./...` 全部通过。  
当 `auto_close_on_pr=false` 时，行为保持与当前 continuation retry 路径一致。

## 14. 运维影响

| 项目 | 说明 |
|---|---|
| 新增运行时状态 | `AwaitingMerge` |
| 新增外部依赖假设 | workspace 内需可查询 PR 状态（当前为 `gh`） |
| 排障点 | `gh` 不可用、认证失效、PR 状态查询失败 |
| 调度行为变化 | 开 PR 后不会立刻自动关闭 issue，而是等待 merge |

### 配套文档落点

- `docs/operator-runbook.md`：补充 AwaitingMerge 排障与 `gh` 依赖说明
- `FLOW.md`：更新成功路径与 `auto_close_on_pr` 语义
- `docs/conformance-matrix.md`：新增 awaiting-merge 测试项

## 15. 风险与回滚

| 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|
| PR 状态查询失败 | 中 | issue 长时间停在 AwaitingMerge | warn + alert + runbook 排障 |
| `gh` 输出 schema 变化 | 低 | merge 检测失败 | 抽象查询层 + 单元测试 |
| PR 被关闭未合并 | 中 | issue 可能需要继续人工/agent 跟进 | 明确回退 continuation retry |
| 语义切换带来行为变化 | 中 | 与旧“open PR 即 close”预期不一致 | 文档明确、保留 feature flag |

### 回滚方式

1. 将 `orchestrator.auto_close_on_pr` 设为 `false`
2. 若需彻底回退实现：删除 `AwaitingMerge` 状态与 merge reconcile 路径，恢复旧 open-PR close 行为

## 16. 实现步骤

1. 抽取 PR 查询抽象，补 `FindByHeadBranch`
2. 引入 `AwaitingMergeEntry` 与 snapshot 暴露
3. 修改 `handleWorkerExit` 成功路径
4. 新增 `reconcileAwaitingMerge`
5. 补齐 server / orchestrator 测试
6. 更新 runbook / flow / conformance 文档

## 17. 未来演进

- merge queue / checks green / review-approved 等更细粒度 gate
- 与 Notifications / Reactions 联动的 merge 事件
- AwaitingMerge 的持久化恢复
- GitHub 以外 PR provider

## 附录：文件改动清单

### 新建文件

- `docs/rfcs/pr-merge-gating.md`

### 修改文件

- `internal/orchestrator/orchestrator.go`
- `internal/orchestrator/orchestrator_test.go`
- `internal/server/server.go`
- `internal/server/server_test.go`
- `docs/operator-runbook.md`
- `docs/conformance-matrix.md`
- `FLOW.md`
- `docs/cycles/cycle-05-post-mvp.md`
