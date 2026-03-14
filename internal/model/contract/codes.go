package contract

type ReasonDescriptor struct {
	Category CodeCategory
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
)

const (
	ErrorAPIMethodNotAllowed ErrorCode = "api.method_not_allowed"
	ErrorAPIInvalidRequest   ErrorCode = "api.invalid_request"
	ErrorAPINotFound         ErrorCode = "api.not_found"
	ErrorAPIConflict         ErrorCode = "api.conflict"
	ErrorServiceUnavailable  ErrorCode = "service.unavailable"
	ErrorConfigInvalid       ErrorCode = "config.invalid"
)

var reasonDescriptors = map[ReasonCode]ReasonDescriptor{
	ReasonServiceDegradedNotificationDelivery: {Category: CategoryService},
	ReasonServiceUnavailableCoreDependency:    {Category: CategoryService},
	ReasonServiceRecoveryInProgress:           {Category: CategoryService},
	ReasonRecordBlockedAwaitingMerge:          {Category: CategoryRecord},
	ReasonRecordBlockedAwaitingIntervention:   {Category: CategoryRecord},
	ReasonRecordBlockedRetryScheduled:         {Category: CategoryRecord},
	ReasonRecordBlockedRecoveryUncertain:      {Category: CategoryRecord},
	ReasonControlRefreshAccepted:              {Category: CategoryControl},
	ReasonControlRefreshRejectedServiceMode:   {Category: CategoryControl},
	ReasonRuntimeReloadHotReloadAllowed:       {Category: CategoryRuntime},
	ReasonRuntimeReloadRestartRequired:        {Category: CategoryRuntime},
	ReasonRuntimeIdentityMismatch:             {Category: CategoryRuntime},
}

var errorDescriptors = map[ErrorCode]ErrorDescriptor{
	ErrorAPIMethodNotAllowed: {Category: CategoryAPI, Retryable: false},
	ErrorAPIInvalidRequest:   {Category: CategoryAPI, Retryable: false},
	ErrorAPINotFound:         {Category: CategoryAPI, Retryable: false},
	ErrorAPIConflict:         {Category: CategoryControl, Retryable: true},
	ErrorServiceUnavailable:  {Category: CategoryService, Retryable: true},
	ErrorConfigInvalid:       {Category: CategoryConfig, Retryable: false},
}

func DescribeReason(code ReasonCode) (ReasonDescriptor, bool) {
	desc, ok := reasonDescriptors[code]
	return desc, ok
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
		ReasonCode: code,
		Category:   desc.Category,
		Details:    cloneDetails(details),
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
