package contract

type ReasonDescriptor struct {
	Category   CodeCategory
	Summary    string
	Visibility VisibilityLevel
}

type DecisionDescriptor struct {
	Category           CodeCategory
	Summary            string
	Visibility         VisibilityLevel
	RecommendedActions []DecisionAction
}

type ErrorDescriptor struct {
	Category  CodeCategory
	Retryable bool
}

const (
	ReasonServiceDegradedNotificationDelivery ReasonCode = "service.degraded.notification_delivery"
	ReasonServiceUnavailableCoreDependency    ReasonCode = "service.unavailable.core_dependency"
	ReasonServiceRecoveryInProgress           ReasonCode = "service.recovery.in_progress"
	ReasonRecordBlockedAwaitingMerge          ReasonCode = "record.blocked.awaiting_merge"
	ReasonRecordBlockedAwaitingIntervention   ReasonCode = "record.blocked.awaiting_intervention"
	ReasonRecordBlockedRetryScheduled         ReasonCode = "record.blocked.retry_scheduled"
	ReasonRecordBlockedRecoveryUncertain      ReasonCode = "record.blocked.recovery_uncertain"
	ReasonControlRefreshAccepted              ReasonCode = "control.refresh.accepted"
	ReasonControlRefreshRejectedServiceMode   ReasonCode = "control.refresh.rejected.service_mode"
	ReasonRuntimeReloadHotReloadAllowed       ReasonCode = "runtime.reload.hot_reload_allowed"
	ReasonRuntimeReloadRestartRequired        ReasonCode = "runtime.reload.restart_required"
	ReasonRuntimeIdentityMismatch             ReasonCode = "runtime.identity.mismatch"
	ReasonCapabilityStaticUnsupported         ReasonCode = "capability.static.unsupported"
	ReasonCapabilityCurrentlyUnavailable      ReasonCode = "capability.current.unavailable"
	ReasonCheckpointBusinessCaptured          ReasonCode = "checkpoint.business.captured"
	ReasonCheckpointRecoveryCaptured          ReasonCode = "checkpoint.recovery.captured"
	ReasonJobTargetReferenceMissing           ReasonCode = "job.target_reference.missing"
	ReasonRunContinuationPending              ReasonCode = "run.continuation.pending"
	ReasonRunReviewGateCandidateReady         ReasonCode = "run.review_gate.candidate_ready"
	ReasonRunReviewGateFixLimitReached        ReasonCode = "run.review_gate.fix_limit_reached"
	ReasonRunBlockedInterventionRequired      ReasonCode = "run.blocked.intervention_required"
	ReasonRunHardViolationDetected            ReasonCode = "run.hard_violation_detected"
	ReasonActionSourceClosurePending          ReasonCode = "action.source_closure.pending"
	ReasonActionExternalPending               ReasonCode = "action.external_pending"
	ReasonInterventionInputRequired           ReasonCode = "intervention.input.required"
)

const (
	DecisionRetryRun                DecisionCode = "decision.retry_run"
	DecisionContinueFromRecovery    DecisionCode = "decision.continue_from_recovery_checkpoint"
	DecisionOpenReviewGate          DecisionCode = "decision.open_review_gate"
	DecisionStartContinuationRun    DecisionCode = "decision.start_continuation_run"
	DecisionResumeAfterIntervention DecisionCode = "decision.resume_after_intervention"
	DecisionCancelJob               DecisionCode = "decision.cancel_job"
	DecisionReviewPullRequest       DecisionCode = "decision.review_pull_request"
	DecisionHandOffToLandChange     DecisionCode = "decision.hand_off_to_land_change"
	DecisionInspectDiagnosticReport DecisionCode = "decision.inspect_diagnostic_report"
	DecisionRetrySourceClosure      DecisionCode = "decision.retry_source_closure"
	DecisionEscalateHardViolation   DecisionCode = "decision.escalate_hard_violation"
	DecisionWaitForCapability       DecisionCode = "decision.wait_for_capability"
)

const (
	ErrorAPIMethodNotAllowed       ErrorCode = "api.method_not_allowed"
	ErrorAPIInvalidRequest         ErrorCode = "api.invalid_request"
	ErrorAPINotFound               ErrorCode = "api.not_found"
	ErrorAPIConflict               ErrorCode = "api.conflict"
	ErrorAPILeaderRequired         ErrorCode = "api.leader_required"
	ErrorAPIUnsupportedJobType     ErrorCode = "api.unsupported_job_type"
	ErrorAPIInvalidStateTransition ErrorCode = "api.invalid_state_transition"
	ErrorAPIInterventionConflict   ErrorCode = "api.intervention_conflict"
	ErrorCapabilityUnavailable     ErrorCode = "capability.unavailable"
	ErrorServiceUnavailable        ErrorCode = "service.unavailable"
	ErrorCheckpointUnavailable     ErrorCode = "checkpoint.unavailable"
	ErrorCheckpointInvalid         ErrorCode = "checkpoint.invalid"
	ErrorReviewGateBlocked         ErrorCode = "review_gate.blocked"
	ErrorSourceClosureUnavailable  ErrorCode = "action.source_closure.unavailable"
	ErrorRunHardViolationDetected  ErrorCode = "run.hard_violation_detected"
	ErrorAuthUnauthorized          ErrorCode = "auth.unauthorized"
	ErrorAuthForbidden             ErrorCode = "auth.forbidden"
	ErrorConfigInvalid             ErrorCode = "config.invalid"
)

var reasonDescriptors = map[ReasonCode]ReasonDescriptor{
	ReasonServiceDegradedNotificationDelivery: {Category: CategoryService, Summary: "通知通道可用性下降。", Visibility: VisibilityLevelRestricted},
	ReasonServiceUnavailableCoreDependency:    {Category: CategoryService, Summary: "核心依赖不可用导致服务不可服务。", Visibility: VisibilityLevelRestricted},
	ReasonServiceRecoveryInProgress:           {Category: CategoryService, Summary: "服务正在恢复中。", Visibility: VisibilityLevelSummary},
	ReasonRecordBlockedAwaitingMerge:          {Category: CategoryRecord, Summary: "记录已阻塞，等待合并结果。", Visibility: VisibilityLevelRestricted},
	ReasonRecordBlockedAwaitingIntervention:   {Category: CategoryRecord, Summary: "记录已阻塞，等待人工介入。", Visibility: VisibilityLevelRestricted},
	ReasonRecordBlockedRetryScheduled:         {Category: CategoryRecord, Summary: "记录已阻塞，系统已安排重试。", Visibility: VisibilityLevelRestricted},
	ReasonRecordBlockedRecoveryUncertain:      {Category: CategoryRecord, Summary: "恢复状态不确定，需要人工确认。", Visibility: VisibilityLevelRestricted},
	ReasonControlRefreshAccepted:              {Category: CategoryControl, Summary: "刷新请求已被接受。", Visibility: VisibilityLevelSummary},
	ReasonControlRefreshRejectedServiceMode:   {Category: CategoryControl, Summary: "当前服务状态不允许执行刷新。", Visibility: VisibilityLevelSummary},
	ReasonRuntimeReloadHotReloadAllowed:       {Category: CategoryRuntime, Summary: "配置变更可通过受控热重载生效。", Visibility: VisibilityLevelRestricted},
	ReasonRuntimeReloadRestartRequired:        {Category: CategoryRuntime, Summary: "配置变更需要重启才能生效。", Visibility: VisibilityLevelRestricted},
	ReasonRuntimeIdentityMismatch:             {Category: CategoryRuntime, Summary: "运行时身份与期望不一致。", Visibility: VisibilityLevelRestricted},
	ReasonCapabilityStaticUnsupported:         {Category: CategoryCapability, Summary: "该能力不在当前实例的静态支持集合内。", Visibility: VisibilityLevelSummary},
	ReasonCapabilityCurrentlyUnavailable:      {Category: CategoryCapability, Summary: "该能力在当前状态下暂不可用。", Visibility: VisibilityLevelSummary},
	ReasonCheckpointBusinessCaptured:          {Category: CategoryCheckpoint, Summary: "已记录正式业务 checkpoint。", Visibility: VisibilityLevelRestricted},
	ReasonCheckpointRecoveryCaptured:          {Category: CategoryCheckpoint, Summary: "已记录可续跑的恢复 checkpoint。", Visibility: VisibilityLevelRestricted},
	ReasonJobTargetReferenceMissing:           {Category: CategoryJob, Summary: "任务缺少正式目标引用。", Visibility: VisibilityLevelRestricted},
	ReasonRunContinuationPending:              {Category: CategoryRun, Summary: "当前 Run 已安全收口，等待下一轮续跑。", Visibility: VisibilityLevelRestricted},
	ReasonRunReviewGateCandidateReady:         {Category: CategoryRun, Summary: "候选交付点已满足，平台应启动只读审查。", Visibility: VisibilityLevelRestricted},
	ReasonRunReviewGateFixLimitReached:        {Category: CategoryRun, Summary: "单个 Run 内的审查修复轮次已达上限。", Visibility: VisibilityLevelRestricted},
	ReasonRunBlockedInterventionRequired:      {Category: CategoryRun, Summary: "执行被阻塞，需要人工介入后继续。", Visibility: VisibilityLevelRestricted},
	ReasonRunHardViolationDetected:            {Category: CategoryRun, Summary: "检测到硬违规，当前 Run 必须立即转入人工介入。", Visibility: VisibilityLevelRestricted},
	ReasonActionSourceClosurePending:          {Category: CategoryAction, Summary: "主 Job 已完成，等待 SourceClosureAction 收口外部来源。", Visibility: VisibilityLevelRestricted},
	ReasonActionExternalPending:               {Category: CategoryAction, Summary: "外部能力暂不可用，Action 进入 external_pending。", Visibility: VisibilityLevelSummary},
	ReasonInterventionInputRequired:           {Category: CategoryIntervention, Summary: "人工介入缺少必需的结构化输入。", Visibility: VisibilityLevelRestricted},
}

var decisionDescriptors = map[DecisionCode]DecisionDescriptor{
	DecisionRetryRun: {
		Category:   CategoryRun,
		Summary:    "建议重试当前 Run。",
		Visibility: VisibilityLevelSummary,
		RecommendedActions: []DecisionAction{
			{Kind: DecisionActionKindControl, Control: ControlActionRetry, Summary: "执行正式 retry 控制动作。"},
		},
	},
	DecisionContinueFromRecovery: {
		Category:   CategoryCheckpoint,
		Summary:    "建议基于恢复 checkpoint 开启新的 Run 续跑。",
		Visibility: VisibilityLevelRestricted,
	},
	DecisionOpenReviewGate: {
		Category:   CategoryRun,
		Summary:    "建议由平台启动只读 reviewer 审查当前候选交付物。",
		Visibility: VisibilityLevelRestricted,
		RecommendedActions: []DecisionAction{
			{Kind: DecisionActionKindInspectArtifact, Summary: "检查候选交付物并发起审查。"},
		},
	},
	DecisionStartContinuationRun: {
		Category:   CategoryRun,
		Summary:    "建议在保存恢复 checkpoint 后自动开启下一轮 Run。",
		Visibility: VisibilityLevelRestricted,
	},
	DecisionResumeAfterIntervention: {
		Category:   CategoryIntervention,
		Summary:    "建议在介入完成后恢复执行。",
		Visibility: VisibilityLevelSummary,
		RecommendedActions: []DecisionAction{
			{Kind: DecisionActionKindControl, Control: ControlActionResolveIntervention, Summary: "先解决介入请求。"},
			{Kind: DecisionActionKindControl, Control: ControlActionResume, Summary: "再继续执行。"},
		},
	},
	DecisionCancelJob: {
		Category:   CategoryJob,
		Summary:    "建议取消当前 Job。",
		Visibility: VisibilityLevelSummary,
		RecommendedActions: []DecisionAction{
			{Kind: DecisionActionKindControl, Control: ControlActionCancel, Summary: "终止当前 Job 的继续推进。"},
		},
	},
	DecisionReviewPullRequest: {
		Category:   CategoryOutcome,
		Summary:    "建议检查生成的 PR 是否满足审查标准。",
		Visibility: VisibilityLevelRestricted,
		RecommendedActions: []DecisionAction{
			{Kind: DecisionActionKindInspectArtifact, Summary: "检查主 PR 产物。"},
		},
	},
	DecisionHandOffToLandChange: {
		Category:   CategoryOutcome,
		Summary:    "建议把后续落地交给独立的 land_change 类型处理。",
		Visibility: VisibilityLevelRestricted,
		RecommendedActions: []DecisionAction{
			{Kind: DecisionActionKindHandoffJobType, RelatedJobType: JobTypeLandChange, Summary: "交由 land_change 正式承接落地。"},
		},
	},
	DecisionInspectDiagnosticReport: {
		Category:   CategoryOutcome,
		Summary:    "建议审阅诊断报告与证据包。",
		Visibility: VisibilityLevelRestricted,
		RecommendedActions: []DecisionAction{
			{Kind: DecisionActionKindInspectArtifact, Summary: "检查 Diagnostic Report 与 Evidence Bundle。"},
		},
	},
	DecisionRetrySourceClosure: {
		Category:   CategoryAction,
		Summary:    "建议在外部能力恢复后重试 SourceClosureAction。",
		Visibility: VisibilityLevelRestricted,
		RecommendedActions: []DecisionAction{
			{Kind: DecisionActionKindControl, Control: ControlActionRetry, Summary: "重试 SourceClosureAction。"},
		},
	},
	DecisionEscalateHardViolation: {
		Category:   CategoryRun,
		Summary:    "建议停止当前 Run 并转入正式人工介入流程。",
		Visibility: VisibilityLevelRestricted,
	},
	DecisionWaitForCapability: {
		Category:   CategoryCapability,
		Summary:    "建议等待能力恢复后再继续写操作。",
		Visibility: VisibilityLevelSummary,
		RecommendedActions: []DecisionAction{
			{Kind: DecisionActionKindWaitForCapability, Summary: "待能力恢复后再执行。"},
		},
	},
}

var errorDescriptors = map[ErrorCode]ErrorDescriptor{
	ErrorAPIMethodNotAllowed:       {Category: CategoryAPI, Retryable: false},
	ErrorAPIInvalidRequest:         {Category: CategoryAPI, Retryable: false},
	ErrorAPINotFound:               {Category: CategoryAPI, Retryable: false},
	ErrorAPIConflict:               {Category: CategoryControl, Retryable: true},
	ErrorAPILeaderRequired:         {Category: CategoryControl, Retryable: true},
	ErrorAPIUnsupportedJobType:     {Category: CategoryAPI, Retryable: false},
	ErrorAPIInvalidStateTransition: {Category: CategoryRun, Retryable: false},
	ErrorAPIInterventionConflict:   {Category: CategoryIntervention, Retryable: true},
	ErrorCapabilityUnavailable:     {Category: CategoryCapability, Retryable: true},
	ErrorServiceUnavailable:        {Category: CategoryService, Retryable: true},
	ErrorCheckpointUnavailable:     {Category: CategoryCheckpoint, Retryable: true},
	ErrorCheckpointInvalid:         {Category: CategoryCheckpoint, Retryable: false},
	ErrorReviewGateBlocked:         {Category: CategoryRun, Retryable: false},
	ErrorSourceClosureUnavailable:  {Category: CategoryAction, Retryable: true},
	ErrorRunHardViolationDetected:  {Category: CategoryRun, Retryable: false},
	ErrorAuthUnauthorized:          {Category: CategorySecurity, Retryable: false},
	ErrorAuthForbidden:             {Category: CategorySecurity, Retryable: false},
	ErrorConfigInvalid:             {Category: CategoryConfig, Retryable: false},
}

func DescribeReason(code ReasonCode) (ReasonDescriptor, bool) {
	desc, ok := reasonDescriptors[code]
	return desc, ok
}

func DescribeDecision(code DecisionCode) (DecisionDescriptor, bool) {
	desc, ok := decisionDescriptors[code]
	if !ok {
		return DecisionDescriptor{}, false
	}
	desc.RecommendedActions = cloneDecisionActions(desc.RecommendedActions)
	return desc, true
}

func DescribeError(code ErrorCode) (ErrorDescriptor, bool) {
	desc, ok := errorDescriptors[code]
	return desc, ok
}

func MustReason(code ReasonCode, details map[string]any) Reason {
	desc, ok := DescribeReason(code)
	if !ok {
		panic("unknown reason code: " + string(code))
	}
	return Reason{
		ObjectType: ObjectTypeReason,
		ReasonCode: code,
		Category:   desc.Category,
		Summary:    desc.Summary,
		Visibility: desc.Visibility,
		Details:    cloneDetails(details),
	}
}

func MustDecision(code DecisionCode, details map[string]any) Decision {
	desc, ok := DescribeDecision(code)
	if !ok {
		panic("unknown decision code: " + string(code))
	}
	return Decision{
		ObjectType:         ObjectTypeDecision,
		DecisionCode:       code,
		Category:           desc.Category,
		Summary:            desc.Summary,
		Visibility:         desc.Visibility,
		RecommendedActions: cloneDecisionActions(desc.RecommendedActions),
		Details:            cloneDetails(details),
	}
}

func MustErrorResponse(code ErrorCode, message string, details map[string]any) ErrorResponse {
	desc, ok := DescribeError(code)
	if !ok {
		panic("unknown error code: " + string(code))
	}
	return ErrorResponse{
		ErrorCode: code,
		Message:   message,
		Category:  desc.Category,
		Retryable: desc.Retryable,
		Details:   cloneDetails(details),
	}
}

func cloneDecisionActions(actions []DecisionAction) []DecisionAction {
	if len(actions) == 0 {
		return nil
	}
	clone := make([]DecisionAction, len(actions))
	copy(clone, actions)
	return clone
}

func cloneDetails(details map[string]any) map[string]any {
	if details == nil {
		return map[string]any{}
	}
	clone := make(map[string]any, len(details))
	for key, value := range details {
		clone[key] = value
	}
	return clone
}
