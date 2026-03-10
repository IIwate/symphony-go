# Symphony-Go Conformance Matrix

## Scope

本文件把 `SPEC.md §17` 的验证矩阵映射到当前仓库中的自动化测试与手工验证记录，用于首版发布前核对实现证据。

### Profiles

- `Core Conformance`：`SPEC.md §17.1` ~ `§17.7`
- `Extension Conformance`：已 shipped 的可选能力
  - HTTP Server (`SPEC.md §13.7`)
  - `linear_graphql` tool
- `Real Integration Profile`：`SPEC.md §17.8`

### 状态口径

- `已覆盖`：已有自动化测试直接覆盖
- `部分覆盖`：已有主要证据，但仍有细项主要依赖文档或待后续补强
- `仅文档`：当前以 runbook / checklist 为主要证据
- `后续补强`：非首版阻塞项，已记录但不阻塞本轮发布

## Core Conformance

### §17.1 Workflow and Config Parsing

| SPEC 条目 | 实现包 | 现状 | 测试锚点 | 文档锚点 | 备注 |
|---|---|---|---|---|---|
| Workflow 路径优先级、默认路径、显式路径错误 | `cmd/symphony` | 已覆盖 | `cmd/symphony/main_test.go`：`TestRunCLIUsesDefaultWorkflowPath`、`TestRunCLIFailsForMissingExplicitWorkflow`、`TestRunCLIFailsWhenDefaultWorkflowMissing` | `REQUIREMENTS.md`、`docs/operator-runbook.md` | 覆盖 CLI 入口行为 |
| front matter 解析、无 front matter、front matter 非 map | `internal/workflow` | 已覆盖 | `internal/workflow/workflow_test.go`：`TestLoadWithFrontMatter`、`TestLoadWithoutFrontMatter`、`TestLoadFrontMatterNotMap` | `REQUIREMENTS.md` | 锁住 typed error |
| 默认 prompt、unknown variable、unknown filter | `internal/workflow` | 已覆盖 | `internal/workflow/workflow_test.go`：`TestRenderPromptUsesDefaultPrompt`、`TestRenderPromptUnknownVariable`、`TestRenderPromptUnknownFilter` | `REQUIREMENTS.md` | 本轮新增 unknown filter |
| watcher reload、invalid reload 保留旧配置 | `internal/workflow`、`cmd/symphony` | 已覆盖 | `internal/workflow/workflow_test.go`：`TestWatchReloadsOnChange`、`TestWatchSkipsInvalidReload`；`cmd/symphony/main_test.go`：`TestExecuteStartsWatcherAndNotifiesReload`、`TestRuntimeStateApplyReloadKeepsPortOverride` | `docs/operator-runbook.md` | 覆盖 last known good |
| 默认值、`$VAR`、`~`、字符串整数、hooks timeout 非正值回退默认 | `internal/config` | 已覆盖 | `internal/config/config_test.go`：`TestNewFromWorkflowAppliesDefaultsAndCoercions`、`TestNewFromWorkflowFallsBackToDefaultHookTimeoutForNonPositiveValues`、`TestValidateForDispatch` | `REQUIREMENTS.md` | 本轮新增非正 hooks timeout 测试 |
| `attempt` 模板变量 | `internal/workflow` | 已覆盖 | `internal/workflow/workflow_test.go`：`TestRenderPromptAttemptVariable` | `REQUIREMENTS.md`、`FLOW.md` | 覆盖 `attempt=nil` 与显式 attempt 渲染 |

### §17.2 Workspace Manager and Safety

| SPEC 条目 | 实现包 | 现状 | 测试锚点 | 文档锚点 | 备注 |
|---|---|---|---|---|---|
| 工作区创建、复用、路径冲突与逃逸拒绝 | `internal/workspace` | 已覆盖 | `internal/workspace/manager_test.go`：`TestCreateForIssueCreatesAndReusesWorkspace`、`TestCreateForIssueRejectsEscapeAndFileConflict` | `REQUIREMENTS.md`、`FLOW.md` | 包含 path escape 关键约束 |
| `after_create` 失败清理 | `internal/workspace` | 已覆盖 | `internal/workspace/manager_test.go`：`TestCreateForIssueCleansUpOnAfterCreateFailure` | `REQUIREMENTS.md` | 锁住 fatal hook 语义 |
| `before_run` 失败 / 超时 | `internal/workspace` | 已覆盖 | `internal/workspace/manager_test.go`：`TestPrepareForRunCleansArtifactsAndFailsOnBeforeRun`、`TestPrepareForRunTimeout` | `REQUIREMENTS.md` | 覆盖临时产物清理与 timeout |
| `after_run` / `before_remove` best-effort | `internal/workspace` | 已覆盖 | `internal/workspace/manager_test.go`：`TestFinalizeRunAndCleanupIgnoreBestEffortHooks` | `REQUIREMENTS.md` | 覆盖 ignore 语义 |
| Git 分支准备扩展 | `internal/workspace` | 已覆盖 | `internal/workspace/manager_test.go`：`TestPrepareForRunCreatesExpectedBranch`、`TestPrepareForRunAddsSuffixWhenRemoteBranchExists`、`TestPrepareForRunUsesGitHubIssueNumberShortName` | `FLOW.md` | Go 实现扩展，不属 SPEC 核心 |
| agent launch 以 workspace 为 cwd 的集成不变量 | `internal/workspace`、`internal/agent` | 部分覆盖 | `internal/agent/runner_test.go` 的绝对路径检查；workspace 路径测试 | `FLOW.md` | 跨包集成点，缺端到端自动化锚点 |

### §17.3 Issue Tracker Client

| SPEC 条目 | 实现包 | 现状 | 测试锚点 | 文档锚点 | 备注 |
|---|---|---|---|---|---|
| 空输入不发请求 | `internal/tracker` | 已覆盖 | `internal/tracker/linear_test.go`：`TestFetchIssuesByStatesEmptySkipsRequest` | `REQUIREMENTS.md` |  |
| 候选 issue 分页、归一化、label lowercase、blocker 提取 | `internal/tracker` | 已覆盖 | `internal/tracker/linear_test.go`：`TestFetchCandidateIssuesPaginatesAndNormalizes`、`TestFetchCandidateIssuesTreatsUnfinishedChildrenAsBlockers`、`TestFetchCandidateIssuesCanDisableChildBlockers` | `FLOW.md`、`REQUIREMENTS.md` | 包含 `slugId`、分页、显式 `blocks` 依赖，以及 `tracker.linear.children_block_parent` 默认开启/可关闭路径 |
| 按 ID 刷新状态使用 `[ID!]` | `internal/tracker` | 已覆盖 | `internal/tracker/linear_test.go`：`TestFetchIssueStatesByIDsUsesIDType` | `REQUIREMENTS.md` |  |
| GraphQL / HTTP / pagination 错误映射 | `internal/tracker` | 已覆盖 | `internal/tracker/linear_test.go`：`TestFetchCandidateIssuesMapsGraphQLErrors`、`TestFetchCandidateIssuesMissingEndCursor`、`TestFetchCandidateIssuesMapsHTTPStatus` | `REQUIREMENTS.md` |  |
| transport failure / 更广义 malformed payload | `internal/tracker` | 已覆盖 | `internal/tracker/linear_test.go`：`TestFetchCandidateIssuesMapsTransportFailure`、`TestFetchCandidateIssuesMapsMalformedPayload` | `docs/release-checklist.md` | 本轮补齐 transport / invalid JSON 负例 |

### §17.4 Orchestrator Dispatch, Reconciliation, and Retry

| SPEC 条目 | 实现包 | 现状 | 测试锚点 | 文档锚点 | 备注 |
|---|---|---|---|---|---|
| 调度排序与 Todo blocker 规则 | `internal/orchestrator` | 已覆盖 | `internal/orchestrator/orchestrator_test.go`：`TestDispatchEligibleIssuesSortsAndBlocksTodo` | `REQUIREMENTS.md`、`FLOW.md` |  |
| 正常退出 continuation retry / 异常退出 backoff | `internal/orchestrator` | 已覆盖 | `internal/orchestrator/orchestrator_test.go`：`TestHandleWorkerExitSchedulesContinuationAndBackoffRetry`、`TestHandleWorkerExitNoNewPRSchedulesContinuation` | `FLOW.md` |  |
| 终态 / 非活跃对账 | `internal/orchestrator` | 已覆盖 | `internal/orchestrator/orchestrator_test.go`：`TestReconcileRunningStopsTerminalAndInactiveIssues` | `REQUIREMENTS.md` |  |
| stall timeout 杀 worker 并排队 retry | `internal/orchestrator` | 已覆盖 | `internal/orchestrator/orchestrator_test.go`：`TestReconcileRunningSchedulesRetryForStalledSession` | `FLOW.md` | 本轮新增 |
| preflight 失败仍先 reconcile | `internal/orchestrator` | 已覆盖 | `internal/orchestrator/orchestrator_test.go`：`TestRunOncePreflightFailureStillReconcilesRunningIssues` | `REQUIREMENTS.md` |  |
| token totals 聚合 / turn 计数去重 | `internal/orchestrator` | 已覆盖 | `internal/orchestrator/orchestrator_test.go`：`TestHandleCodexUpdateAggregatesUsage`、`TestHandleCodexUpdateTurnCountIncrementsOnTurnChangeOnly` | `FLOW.md` |  |
| PR merge gating 扩展、AwaitingMerge / AwaitingIntervention 对账与 feature flag 关闭路径 | `internal/orchestrator` | 已覆盖 | `internal/orchestrator/orchestrator_test.go`：`TestHandleWorkerExitHasNewOpenPRMergedTransitionsToDone`、`TestHandleWorkerExitHasNewOpenPRMovesToAwaitingMerge`、`TestHandleWorkerExitClosedPRMovesToAwaitingIntervention`、`TestHandleWorkerExitMergedPRTransitionFailureSchedulesBackoffRetry`、`TestReconcileAwaitingMergeMergedClosesIssue`、`TestReconcileAwaitingMergeClosedMovesToAwaitingIntervention`、`TestReconcileAwaitingMergeLookupFailureKeepsAwaitingAndAlert`、`TestIsDispatchEligibleRejectsAwaitingMerge`、`TestIsDispatchEligibleRejectsAwaitingIntervention`、`TestReconcileAwaitingInterventionReleasesInactiveIssue`、`TestHandleWorkerExitHasNewOpenPRDisabledSchedulesContinuation` | `FLOW.md`、`docs/operator-runbook.md` | Go 实现扩展 |
| slot exhaustion requeue、`max_retry_backoff_ms` 封顶 | `internal/orchestrator` | 已覆盖 | `internal/orchestrator/orchestrator_test.go`：`TestHandleRetryTimerRequeuesWhenNoSlotsAvailable`、`TestScheduleRetryLockedCapsBackoffAtConfiguredMaximum` | `REQUIREMENTS.md` | 本轮补齐 retry 边界 |

### §17.5 Coding-Agent App-Server Client

| SPEC 条目 | 实现包 | 现状 | 测试锚点 | 文档锚点 | 备注 |
|---|---|---|---|---|---|
| 握手序列、continuation turn 复用 thread | `internal/agent` | 已覆盖 | `internal/agent/runner_test.go`：`TestRunnerHandshakeAndContinuationTurns` | `REQUIREMENTS.md` |  |
| read timeout | `internal/agent` | 已覆盖 | `internal/agent/runner_test.go`：`TestRunnerReadTimeout` | `REQUIREMENTS.md` |  |
| approval 自动批准 / unsupported tool 不阻塞 | `internal/agent` | 已覆盖 | `internal/agent/runner_test.go`：`TestRunnerAutoApprovesAndRejectsUnsupportedToolCalls` | `FLOW.md` |  |
| user-input-required 硬失败 | `internal/agent` | 已覆盖 | `internal/agent/runner_test.go`：`TestRunnerFailsOnUserInputRequest` | `FLOW.md` |  |
| streaming noise 过滤 | `internal/agent` | 已覆盖 | `internal/agent/runner_test.go`：`TestStreamingNoiseNotEmitted` | `FLOW.md` |  |
| telemetry 提取与终端事件上报 | `internal/agent` | 已覆盖 | `internal/agent/runner_test.go`：`TestTelemetryEmittedOnce`、`TestTerminalEventsStillEmittedWithUsage`、`TestDeltaUsagePayloadIsIgnored` | `REQUIREMENTS.md`、`FLOW.md` | 本轮对齐 absolute totals 推荐口径 |
| turn timeout | `internal/agent` | 已覆盖 | `internal/agent/runner_test.go`：`TestRunnerTurnTimeout` | `REQUIREMENTS.md`、`FLOW.md` | 与 read timeout 分离建锚点 |
| 非 JSON stderr / rate-limit 提取 | `internal/agent` | 已覆盖 | `internal/agent/runner_test.go`：`TestWaitForTurnEndEmitsPlainStderrAsNotification`、`TestRateLimitsExtractedFromNotificationPayload` | `FLOW.md` | 本轮补齐 stderr 诊断与 rate-limit 提取 |

### §17.6 Observability

| SPEC 条目 | 实现包 | 现状 | 测试锚点 | 文档锚点 | 备注 |
|---|---|---|---|---|---|
| 结构化日志输出到 stderr / file | `internal/logging` | 已覆盖 | `internal/logging/logging_test.go`：`TestNewLoggerWritesToStderrAndFile` | `REQUIREMENTS.md` |  |
| secrets 脱敏 | `internal/logging` | 已覆盖 | `internal/logging/logging_test.go`：`TestNewLoggerMasksSecrets` | `REQUIREMENTS.md` |  |
| sink 失败不拖垮主流程 | `internal/logging` | 已覆盖 | `internal/logging/logging_test.go`：`TestFanoutWriterSurvivesSinkFailure` | `REQUIREMENTS.md` |  |
| token totals 聚合 | `internal/orchestrator` | 已覆盖 | `internal/orchestrator/orchestrator_test.go`：`TestHandleCodexUpdateAggregatesUsage` | `FLOW.md` |  |
| alerts / issue 详情增强字段 / AwaitingMerge / AwaitingIntervention 状态面 | `internal/orchestrator`、`internal/server` | 已覆盖 | `internal/orchestrator/orchestrator_test.go`：`TestSnapshotIncludesAlertsAndWorkspaceContext`、`TestRunOnceSetsAndClearsTrackerAlert`、`TestReconcileAwaitingMergeLookupFailureKeepsAwaitingAndAlert`；`internal/server/server_test.go`：`TestStateEndpointReturnsSnapshot`、`TestIssueEndpointReturnsKnownIssueAnd404ForUnknown` | `FLOW.md`、`docs/operator-runbook.md` | 包含 `awaiting_merge`、`awaiting_intervention` 列表与计数，以及 `merge_status_unknown` 告警 |
| rate-limit 聚合展示 | `internal/orchestrator`、`internal/server` | 已覆盖 | `internal/orchestrator/orchestrator_test.go`：`TestHandleCodexUpdateStoresRateLimitsInSnapshot`；`internal/server/server_test.go`：`TestStateEndpointReturnsSnapshot` | `docs/release-checklist.md` | 本轮补齐聚合与状态面展示 |

### §17.7 CLI and Host Lifecycle

| SPEC 条目 | 实现包 | 现状 | 测试锚点 | 文档锚点 | 备注 |
|---|---|---|---|---|---|
| 默认 workflow 路径 / 显式路径错误 | `cmd/symphony` | 已覆盖 | `cmd/symphony/main_test.go`：`TestRunCLIUsesDefaultWorkflowPath`、`TestRunCLIFailsForMissingExplicitWorkflow`、`TestRunCLIFailsWhenDefaultWorkflowMissing` | `REQUIREMENTS.md`、`docs/operator-runbook.md` |  |
| reload 保持端口覆盖 | `cmd/symphony` | 已覆盖 | `cmd/symphony/main_test.go`：`TestRuntimeStateApplyReloadKeepsPortOverride` | `FLOW.md` |  |
| watcher 与 reload 通知链路 | `cmd/symphony` | 已覆盖 | `cmd/symphony/main_test.go`：`TestExecuteStartsWatcherAndNotifiesReload` | `REQUIREMENTS.md` |  |
| 正常 shutdown / HTTP server 关闭顺序 | `cmd/symphony` | 已覆盖 | `cmd/symphony/main_test.go`：`TestExecuteGracefulShutdownOnContextCancel`、`TestExecuteShutdownWaitsForWorkers`、`TestExecuteShutdownWithHTTPServer` | `docs/release-checklist.md` | 本轮补齐关闭链路与时序断言 |

## Extension Conformance

### HTTP Server (`SPEC.md §13.7`)

| SPEC 条目 | 实现包 | 现状 | 测试锚点 | 文档锚点 | 备注 |
|---|---|---|---|---|---|
| `/` Dashboard、`/api/v1/state`、`/api/v1/{identifier}`、`/api/v1/refresh`、`/api/v1/events` | `internal/server` | 已覆盖 | `internal/server/server_test.go`：`TestStateEndpointReturnsSnapshot`、`TestIssueEndpointReturnsKnownIssueAnd404ForUnknown`、`TestRefreshEndpointAndMethodNotAllowed`、`TestEventsEndpointSendsSnapshotAndUpdate`、`TestDashboardAndMethodNotAllowed` | `docs/operator-runbook.md`、`docs/release-checklist.md` | `/api/v1/state` 包含 `awaiting_merge` / `awaiting_intervention` 列表与计数 |
| 404 / 405 / SSE `snapshot` + `update` | `internal/server` | 已覆盖 | 同上 | `FLOW.md` |  |
| SSE 不可用 / 客户端断开 / 并发订阅 / rate-limit 展示 | `internal/server` | 已覆盖 | `internal/server/server_test.go`：`TestStateEndpointReturnsSnapshot`、`TestEventsEndpointClientDisconnect`、`TestEventsEndpointNoFlusherReturns500`、`TestEventsEndpointConcurrentClients` | `docs/release-checklist.md` | 本轮补齐 SSE 负例与并发订阅 |

### `linear_graphql` Tool

| SPEC 条目 | 实现包 | 现状 | 测试锚点 | 文档锚点 | 备注 |
|---|---|---|---|---|---|
| dynamicTools 广告（正例） | `internal/agent` | 已覆盖 | `internal/agent/runner_test.go`：`TestRunnerHandshakeAndContinuationTurns` | `REQUIREMENTS.md` |  |
| 成功调用 / payload 变体兼容 | `internal/agent` | 已覆盖 | `internal/agent/runner_test.go`：`TestRunnerLinearGraphQLToolSuccess`、`TestRunnerLinearGraphQLToolSuccessWithStringToolField`、`TestRunnerLinearGraphQLToolSuccessWithNestedMsgPayload` | `docs/release-checklist.md` |  |
| GraphQL errors / invalid arguments / unsupported tool | `internal/agent` | 已覆盖 | `internal/agent/runner_test.go`：`TestRunnerLinearGraphQLToolGraphQLErrors`、`TestRunnerLinearGraphQLToolInvalidArguments`、`TestRunnerAutoApprovesAndRejectsUnsupportedToolCalls` | `docs/release-checklist.md` |  |
| missing auth / transport failure / 广告负例 | `internal/agent` | 已覆盖 | `internal/agent/runner_test.go`：`TestRunnerThreadStartOmitsDynamicToolsWithoutLinearAuth`、`TestRunnerLinearGraphQLToolMissingAuth`、`TestRunnerLinearGraphQLToolTransportFailure` | `docs/release-checklist.md` | 本轮补齐 missing auth / transport / 广告负例 |

## Real Integration Profile (`§17.8`)

| SPEC 条目 | 实现包 | 现状 | 测试锚点 | 文档锚点 | 备注 |
|---|---|---|---|---|---|
| 真实凭据 seed tests：候选 issue、按 ID 刷新、CLI dry-run | `tracker`、`cmd/symphony` | 已覆盖 | `internal/tracker/linear_integration_test.go`：`TestLinearIntegration_FetchCandidates`、`TestLinearIntegration_FetchByIDs`；`cmd/symphony/main_integration_test.go`：`TestMainIntegration_DryRun` | `docs/release-checklist.md` | 需 `-tags=integration`，缺少环境变量时按 skipped 处理 |
| 发布前 checklist / operator 操作指引 | 文档 | 仅文档 | 无自动化 | `docs/release-checklist.md`、`docs/operator-runbook.md` | 首版发布主证据 |

## Known Gaps / Evidence Notes

- 此前首版收口遗留的三项测试缺口现已覆盖：
  - `internal/server`：SSE client disconnect / no flusher / concurrent clients
  - `cmd/symphony`：graceful shutdown、worker wait 与 HTTP shutdown 顺序
  - `Real Integration Profile`：env-gated seed tests + `RequireEnv` 跳过约定
- 发布证据仍以：
  - `docs/release-checklist.md`
  - `docs/operator-runbook.md`
  为主，integration build tag 测试作为自动化补充证据。
