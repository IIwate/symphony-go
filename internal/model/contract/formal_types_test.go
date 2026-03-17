package contract

import "testing"

func TestFormalEnumsAndStateMachinesAreClosedSets(t *testing.T) {
	jobStatuses := []JobStatus{
		JobStatusQueued,
		JobStatusRunning,
		JobStatusInterventionRequired,
		JobStatusCompleted,
		JobStatusFailed,
		JobStatusCanceled,
		JobStatusRejected,
		JobStatusAbandoned,
	}
	for _, status := range jobStatuses {
		if !status.IsValid() {
			t.Fatalf("JobStatus(%q).IsValid() = false", status)
		}
	}
	if JobStatus("paused").IsValid() {
		t.Fatal(`JobStatus("paused").IsValid() = true, want false`)
	}
	if !JobStatusCompleted.IsTerminal() || !JobStatusRejected.IsTerminal() {
		t.Fatal("terminal job statuses must report IsTerminal() = true")
	}
	if JobStatusRunning.IsTerminal() {
		t.Fatal("running job status must not be terminal")
	}

	if !RunStatusRunning.CanTransitionTo(RunStatusInterventionRequired) {
		t.Fatal("running -> intervention_required must be allowed")
	}
	if !RunStatusRunning.CanTransitionTo(RunStatusContinuationPending) {
		t.Fatal("running -> continuation_pending must be allowed")
	}
	if RunStatusInterventionRequired.CanTransitionTo(RunStatusRunning) {
		t.Fatal("intervention_required -> running must not be allowed")
	}
	if RunStatusContinuationPending.CanTransitionTo(RunStatusRunning) {
		t.Fatal("continuation_pending -> running must not be allowed")
	}
	if RunStatusCompleted.CanTransitionTo(RunStatusRunning) {
		t.Fatal("completed -> running must not be allowed")
	}
	if len(AllowedRunTransitions(RunStatusQueued)) != 3 {
		t.Fatalf("AllowedRunTransitions(queued) = %d, want 3", len(AllowedRunTransitions(RunStatusQueued)))
	}

	interventionStatuses := []InterventionStatus{
		InterventionStatusOpen,
		InterventionStatusResolved,
		InterventionStatusCanceled,
		InterventionStatusExpired,
	}
	for _, status := range interventionStatuses {
		if !status.IsValid() {
			t.Fatalf("InterventionStatus(%q).IsValid() = false", status)
		}
	}
	if !InterventionStatusOpen.CanTransitionTo(InterventionStatusResolved) {
		t.Fatal("open -> resolved must be allowed")
	}
	if InterventionStatusResolved.CanTransitionTo(InterventionStatusOpen) {
		t.Fatal("resolved -> open must not be allowed")
	}

	phases := []RunPhaseSummary{
		RunPhaseSummaryPreparing,
		RunPhaseSummaryExecuting,
		RunPhaseSummaryVerifying,
		RunPhaseSummaryPublishing,
		RunPhaseSummaryBlocked,
	}
	for _, phase := range phases {
		if !phase.IsValid() {
			t.Fatalf("RunPhaseSummary(%q).IsValid() = false", phase)
		}
	}
	if RunPhaseSummary("thinking").IsValid() {
		t.Fatal(`RunPhaseSummary("thinking").IsValid() = true, want false`)
	}

	conclusions := []OutcomeConclusion{
		OutcomeConclusionSucceeded,
		OutcomeConclusionFailed,
		OutcomeConclusionCanceled,
		OutcomeConclusionRejected,
		OutcomeConclusionAbandoned,
		OutcomeConclusionHumanTerminated,
	}
	for _, conclusion := range conclusions {
		if !conclusion.IsValid() {
			t.Fatalf("OutcomeConclusion(%q).IsValid() = false", conclusion)
		}
	}
	if OutcomeConclusion("partial").IsValid() {
		t.Fatal(`OutcomeConclusion("partial").IsValid() = true, want false`)
	}

	actionStatuses := []ActionStatus{
		ActionStatusQueued,
		ActionStatusRunning,
		ActionStatusExternalPending,
		ActionStatusInterventionRequired,
		ActionStatusCompleted,
		ActionStatusFailed,
		ActionStatusCanceled,
	}
	for _, status := range actionStatuses {
		if !status.IsValid() {
			t.Fatalf("ActionStatus(%q).IsValid() = false", status)
		}
	}
	if !ActionStatusRunning.CanTransitionTo(ActionStatusExternalPending) {
		t.Fatal("running -> external_pending must be allowed for actions")
	}
	if ActionStatusCompleted.CanTransitionTo(ActionStatusRunning) {
		t.Fatal("completed -> running must not be allowed for actions")
	}

	roles := []InstanceRole{
		InstanceRoleLeader,
		InstanceRoleStandby,
	}
	for _, role := range roles {
		if !role.IsValid() {
			t.Fatalf("InstanceRole(%q).IsValid() = false", role)
		}
	}
	if InstanceRole("follower").IsValid() {
		t.Fatal(`InstanceRole("follower").IsValid() = true, want false`)
	}

	actions := []ControlAction{
		ControlActionCancel,
		ControlActionRetry,
		ControlActionResume,
		ControlActionResolveIntervention,
		ControlActionTerminate,
	}
	for _, action := range actions {
		if !action.IsValid() {
			t.Fatalf("ControlAction(%q).IsValid() = false", action)
		}
	}
	if ControlActionRefresh.IsValid() {
		t.Fatal(`ControlActionRefresh.IsValid() = true, want false`)
	}
	if !ControlActionRefresh.IsServiceAction() {
		t.Fatal("ControlActionRefresh.IsServiceAction() = false, want true")
	}

	if !CandidateDeliveryPointReviewablePullRequest.IsValid() {
		t.Fatal("CandidateDeliveryPointReviewablePullRequest.IsValid() = false, want true")
	}
	if CandidateDeliveryPointKind("local_commit").IsValid() {
		t.Fatal(`CandidateDeliveryPointKind("local_commit").IsValid() = true, want false`)
	}
	if !CheckpointTypeRecovery.IsValid() {
		t.Fatal("CheckpointTypeRecovery.IsValid() = false, want true")
	}
	if CheckpointType("snapshot").IsValid() {
		t.Fatal(`CheckpointType("snapshot").IsValid() = true, want false`)
	}
	if !ReviewerModeReadOnly.IsValid() {
		t.Fatal("ReviewerModeReadOnly.IsValid() = false, want true")
	}
	if !ReviewGateStatusChangesRequested.IsValid() {
		t.Fatal("ReviewGateStatusChangesRequested.IsValid() = false, want true")
	}
	if ReviewGateStatus("approved").IsValid() {
		t.Fatal(`ReviewGateStatus("approved").IsValid() = true, want false`)
	}
	if !ActionTypeSourceClosure.IsValid() {
		t.Fatal("ActionTypeSourceClosure.IsValid() = false, want true")
	}
}

func TestReasonDecisionAndErrorDescriptorsAreStructured(t *testing.T) {
	inputDetails := map[string]any{"checkpoint_id": "chk-1"}
	reason := MustReason(ReasonCheckpointRecoveryCaptured, inputDetails)
	inputDetails["checkpoint_id"] = "mutated"

	if reason.ObjectType != ObjectTypeReason {
		t.Fatalf("reason.ObjectType = %q, want %q", reason.ObjectType, ObjectTypeReason)
	}
	if reason.Category != CategoryCheckpoint {
		t.Fatalf("reason.Category = %q, want %q", reason.Category, CategoryCheckpoint)
	}
	if reason.Visibility != VisibilityLevelRestricted {
		t.Fatalf("reason.Visibility = %q, want %q", reason.Visibility, VisibilityLevelRestricted)
	}
	if reason.Summary == "" {
		t.Fatal("reason.Summary must not be empty")
	}
	if reason.Details["checkpoint_id"] != "chk-1" {
		t.Fatalf("reason.Details[checkpoint_id] = %v, want %q", reason.Details["checkpoint_id"], "chk-1")
	}

	decision := MustDecision(DecisionRetrySourceClosure, map[string]any{"action_id": "act-1"})
	if decision.ObjectType != ObjectTypeDecision {
		t.Fatalf("decision.ObjectType = %q, want %q", decision.ObjectType, ObjectTypeDecision)
	}
	if decision.Category != CategoryAction {
		t.Fatalf("decision.Category = %q, want %q", decision.Category, CategoryAction)
	}
	if len(decision.RecommendedActions) != 1 {
		t.Fatalf("len(decision.RecommendedActions) = %d, want 1", len(decision.RecommendedActions))
	}
	if decision.RecommendedActions[0].Control != ControlActionRetry {
		t.Fatalf("first recommended control = %q, want %q", decision.RecommendedActions[0].Control, ControlActionRetry)
	}

	errResp := MustErrorResponse(ErrorCheckpointUnavailable, "checkpoint storage unavailable", map[string]any{"checkpoint_type": CheckpointTypeRecovery})
	if errResp.Category != CategoryCheckpoint {
		t.Fatalf("errResp.Category = %q, want %q", errResp.Category, CategoryCheckpoint)
	}
	if !errResp.Retryable {
		t.Fatal("errResp.Retryable = false, want true")
	}
	if _, ok := DescribeReason(ReasonRunHardViolationDetected); !ok {
		t.Fatal("DescribeReason(ReasonRunHardViolationDetected) = false, want true")
	}
	if _, ok := DescribeDecision(DecisionOpenReviewGate); !ok {
		t.Fatal("DescribeDecision(DecisionOpenReviewGate) = false, want true")
	}
	if _, ok := DescribeError(ErrorSourceClosureUnavailable); !ok {
		t.Fatal("DescribeError(ErrorSourceClosureUnavailable) = false, want true")
	}
}

func TestObjectQuerySurfaceTypesStayWithinFormalBoundary(t *testing.T) {
	for _, objectType := range []ObjectType{
		ObjectTypeJob,
		ObjectTypeRun,
		ObjectTypeIntervention,
		ObjectTypeOutcome,
		ObjectTypeArtifact,
		ObjectTypeAction,
		ObjectTypeInstance,
	} {
		if !objectType.SupportsObjectQuery() {
			t.Fatalf("ObjectType(%q).SupportsObjectQuery() = false, want true", objectType)
		}
	}
	for _, objectType := range []ObjectType{
		ObjectTypeReference,
		ObjectTypeReason,
		ObjectTypeDecision,
	} {
		if objectType.SupportsObjectQuery() {
			t.Fatalf("ObjectType(%q).SupportsObjectQuery() = true, want false", objectType)
		}
	}
}

func TestVisibilityExtensionsAndSensitivityRulesValidate(t *testing.T) {
	rule := SensitiveFieldRule{
		FieldPath:         "job.references[0].external_id",
		Class:             SensitiveFieldClassExternalReference,
		MinimumVisibility: VisibilityLevelRestricted,
		Redaction:         "mask",
	}
	if err := rule.Validate(); err != nil {
		t.Fatalf("SensitiveFieldRule.Validate() error = %v", err)
	}

	extensions := Extensions{
		"symphony.contract": {
			"owner": "formal-contract",
		},
	}
	if err := extensions.Validate(); err != nil {
		t.Fatalf("Extensions.Validate() error = %v", err)
	}

	invalid := Extensions{
		"local": {
			"owner": "broken",
		},
	}
	if err := invalid.Validate(); err == nil {
		t.Fatal("Extensions.Validate() = nil for invalid namespace, want error")
	}

	candidate := CandidateDeliveryPoint{
		Kind:    CandidateDeliveryPointReviewablePullRequest,
		Summary: "draft/open PR",
		RequiredArtifactKinds: []ArtifactKind{
			ArtifactKindPullRequest,
		},
	}
	if err := candidate.Validate(); err != nil {
		t.Fatalf("CandidateDeliveryPoint.Validate() error = %v", err)
	}

	reviewGate := ReviewGatePolicy{
		Required:     true,
		ReviewerMode: ReviewerModeReadOnly,
		MaxFixRounds: 2,
	}
	if err := reviewGate.Validate(); err != nil {
		t.Fatalf("ReviewGatePolicy.Validate() error = %v", err)
	}

	checkpointRule := BusinessCheckpointRule{
		Type:                  CheckpointTypeBusiness,
		CandidateDeliveryKind: CandidateDeliveryPointReviewablePullRequest,
		Summary:               "远端 draft/open PR",
		RequiredArtifactKinds: []ArtifactKind{ArtifactKindPullRequest},
	}
	if err := checkpointRule.Validate(); err != nil {
		t.Fatalf("BusinessCheckpointRule.Validate() error = %v", err)
	}
}

func TestFormalObjectContractsMarshalStableFields(t *testing.T) {
	ref := Reference{
		BaseObject: BaseObject{
			ID:              "ref-1",
			ObjectType:      ObjectTypeReference,
			DomainID:        "domain-main",
			Visibility:      VisibilityLevelRestricted,
			ContractVersion: APIVersionV1,
			CreatedAt:       "2026-03-15T00:00:00Z",
			UpdatedAt:       "2026-03-15T00:00:00Z",
		},
		State:       ReferenceStatusActive,
		Type:        ReferenceTypeGitHubRepo,
		System:      "github",
		Locator:     "IIwate/symphony-go",
		URL:         "https://github.com/IIwate/symphony-go",
		ExternalID:  "IIwate/symphony-go",
		DisplayName: "symphony-go",
	}
	decision := MustDecision(DecisionReviewPullRequest, map[string]any{"artifact_id": "art-1"})
	decision.ID = "dec-1"
	decision.DomainID = "domain-main"
	decision.ContractVersion = APIVersionV1
	decision.CreatedAt = "2026-03-15T00:00:00Z"
	decision.UpdatedAt = "2026-03-15T00:00:00Z"
	candidateDelivery := CandidateDelivery{
		Kind:      CandidateDeliveryPointReviewablePullRequest,
		Reached:   true,
		ReachedAt: "2026-03-15T00:00:03Z",
		Summary:   "draft PR 已创建",
		ArtifactIDs: []string{
			"art-1",
		},
	}
	reviewGate := ReviewGate{
		ReviewGatePolicy: ReviewGatePolicy{
			Required:     true,
			ReviewerMode: ReviewerModeReadOnly,
			MaxFixRounds: 2,
		},
		Status:        ReviewGateStatusChangesRequested,
		FixRoundsUsed: 1,
	}
	checkpoint := Checkpoint{
		Type:       CheckpointTypeRecovery,
		Summary:    "已记录恢复 checkpoint。",
		CapturedAt: "2026-03-15T00:00:03Z",
		Stage:      RunPhaseSummaryPublishing,
		ArtifactIDs: []string{
			"art-1",
		},
		BaseSHA: "abc123def456",
		Branch:  "feature/formal-contract",
		Reason:  ptrReason(MustReason(ReasonCheckpointRecoveryCaptured, map[string]any{"artifact_id": "art-1"})),
	}
	actionSummary := ActionSummary{
		HasPendingExternalActions: true,
		PendingCount:              1,
		PendingTypes:              []ActionType{ActionTypeSourceClosure},
	}

	job := Job{
		BaseObject: BaseObject{ID: "job-1", ObjectType: ObjectTypeJob, DomainID: "domain-main", Visibility: VisibilityLevelRestricted, ContractVersion: APIVersionV1, CreatedAt: "2026-03-15T00:00:00Z", UpdatedAt: "2026-03-15T00:00:01Z"},
		ObjectContext: ObjectContext{
			Relations: []ObjectRelation{
				{Type: RelationTypeJobRun, TargetID: "run-1", TargetType: ObjectTypeRun},
				{Type: RelationTypeJobAction, TargetID: "act-1", TargetType: ObjectTypeAction},
			},
			References: []Reference{ref},
			Reasons:    []Reason{MustReason(ReasonJobTargetReferenceMissing, map[string]any{"reference_type": "github_repo"})},
			Decision:   &decision,
			ErrorCode:  ErrorAPIInvalidRequest,
		},
		State:         JobStatusQueued,
		JobType:       JobTypeCodeChange,
		ActionSummary: actionSummary,
	}
	run := Run{
		BaseObject: BaseObject{ID: "run-1", ObjectType: ObjectTypeRun, DomainID: "domain-main", Visibility: VisibilityLevelRestricted, ContractVersion: APIVersionV1, CreatedAt: "2026-03-15T00:00:00Z", UpdatedAt: "2026-03-15T00:00:02Z"},
		ObjectContext: ObjectContext{
			Relations: []ObjectRelation{
				{Type: RelationTypeRunIntervention, TargetID: "int-1", TargetType: ObjectTypeIntervention},
				{Type: RelationTypeRunOutcome, TargetID: "out-1", TargetType: ObjectTypeOutcome},
			},
			Reasons:  []Reason{MustReason(ReasonRunContinuationPending, map[string]any{"checkpoint_id": "chk-1"})},
			Decision: &decision,
		},
		State:             RunStatusContinuationPending,
		Phase:             RunPhaseSummaryPublishing,
		Attempt:           2,
		CandidateDelivery: &candidateDelivery,
		ReviewGate:        &reviewGate,
		Checkpoints:       []Checkpoint{checkpoint},
	}
	intervention := Intervention{
		BaseObject: BaseObject{ID: "int-1", ObjectType: ObjectTypeIntervention, DomainID: "domain-main", Visibility: VisibilityLevelRestricted, ContractVersion: APIVersionV1, CreatedAt: "2026-03-15T00:00:02Z", UpdatedAt: "2026-03-15T00:00:03Z"},
		ObjectContext: ObjectContext{
			Reasons: []Reason{MustReason(ReasonInterventionInputRequired, map[string]any{"field": "resolution"})},
		},
		State:      InterventionStatusOpen,
		TemplateID: "code_change.reviewable_pr_blocked",
		Summary:    "需要人工确认如何处理未生成合格 PR 的情况。",
		RequiredInputs: []InterventionInputField{
			{Field: "resolution", Label: "处理方向", Kind: InterventionInputKindEnum, Required: true, AllowedValues: []string{"revise_scope"}, Description: "选择处理方向。", Visibility: VisibilityLevelRestricted, Sensitivity: SensitiveFieldClassOperationalMetadata},
		},
		AllowedActions: []ControlAction{ControlActionResolveIntervention, ControlActionCancel},
		Resolution: &InterventionResolution{
			Action:         ControlActionResolveIntervention,
			ProvidedInputs: map[string]any{"resolution": "revise_scope"},
			ResolvedAt:     "2026-03-15T00:00:04Z",
			Decision:       &decision,
		},
	}
	outcome := Outcome{
		BaseObject: BaseObject{ID: "out-1", ObjectType: ObjectTypeOutcome, DomainID: "domain-main", Visibility: VisibilityLevelRestricted, ContractVersion: APIVersionV1, CreatedAt: "2026-03-15T00:00:04Z", UpdatedAt: "2026-03-15T00:00:05Z"},
		ObjectContext: ObjectContext{
			Relations: []ObjectRelation{{Type: RelationTypeOutcomeArtifact, TargetID: "art-1", TargetType: ObjectTypeArtifact}},
			Decision:  &decision,
		},
		State:       OutcomeConclusionSucceeded,
		Summary:     "已生成可审查 PR。",
		CompletedAt: "2026-03-15T00:00:05Z",
	}
	artifact := Artifact{
		BaseObject: BaseObject{ID: "art-1", ObjectType: ObjectTypeArtifact, DomainID: "domain-main", Visibility: VisibilityLevelRestricted, ContractVersion: APIVersionV1, CreatedAt: "2026-03-15T00:00:05Z", UpdatedAt: "2026-03-15T00:00:05Z"},
		State:      ArtifactStatusAvailable,
		Kind:       ArtifactKindPullRequest,
		Role:       ArtifactRolePrimary,
		Locator:    "https://github.com/IIwate/symphony-go/pull/123",
	}
	action := Action{
		BaseObject: BaseObject{ID: "act-1", ObjectType: ObjectTypeAction, DomainID: "domain-main", Visibility: VisibilityLevelRestricted, ContractVersion: APIVersionV1, CreatedAt: "2026-03-15T00:00:05Z", UpdatedAt: "2026-03-15T00:00:06Z"},
		ObjectContext: ObjectContext{
			Relations: []ObjectRelation{{Type: RelationTypeActionReference, TargetID: "ref-1", TargetType: ObjectTypeReference}},
			Reasons:   []Reason{MustReason(ReasonActionExternalPending, map[string]any{"action_type": string(ActionTypeSourceClosure)})},
			Decision:  &decision,
		},
		State:   ActionStatusExternalPending,
		Type:    ActionTypeSourceClosure,
		Summary: "等待外部来源关闭完成。",
	}
	instance := Instance{
		BaseObject: BaseObject{ID: "inst-1", ObjectType: ObjectTypeInstance, DomainID: "domain-main", Visibility: VisibilityLevelSummary, ContractVersion: APIVersionV1, CreatedAt: "2026-03-15T00:00:00Z", UpdatedAt: "2026-03-15T00:00:05Z"},
		State:      ServiceModeServing,
		Name:       "symphony",
		Version:    "dev",
		Role:       InstanceRoleLeader,
		StaticCapabilities: StaticCapabilitySet{
			Capabilities: []StaticCapability{{Name: CapabilitySubmitJob, Category: CapabilityCategoryControl, Summary: "支持提交 Job。", Supported: true}},
		},
		AvailableCapabilities: AvailableCapabilitySet{
			Capabilities: []AvailableCapability{{Name: CapabilitySubmitJob, Category: CapabilityCategoryControl, Summary: "当前可提交 Job。", Available: true}},
		},
	}
	capabilities := CapabilityContract{
		Static: StaticCapabilitySet{
			Capabilities: []StaticCapability{{Name: CapabilityLinearSource, Category: CapabilityCategorySource, Summary: "静态支持 Linear 来源。", Supported: true}},
		},
		Available: AvailableCapabilitySet{
			Capabilities: []AvailableCapability{{Name: CapabilityLinearSource, Category: CapabilityCategorySource, Summary: "当前 Linear 来源可用。", Available: true}},
		},
	}

	if err := job.ObjectContext.ValidateForSource(job.ObjectType); err != nil {
		t.Fatalf("job.ObjectContext.ValidateForSource() error = %v", err)
	}
	if err := run.ObjectContext.ValidateForSource(run.ObjectType); err != nil {
		t.Fatalf("run.ObjectContext.ValidateForSource() error = %v", err)
	}
	if err := intervention.ObjectContext.ValidateForSource(intervention.ObjectType); err != nil {
		t.Fatalf("intervention.ObjectContext.ValidateForSource() error = %v", err)
	}
	if err := outcome.ObjectContext.ValidateForSource(outcome.ObjectType); err != nil {
		t.Fatalf("outcome.ObjectContext.ValidateForSource() error = %v", err)
	}
	if err := action.ObjectContext.ValidateForSource(action.ObjectType); err != nil {
		t.Fatalf("action.ObjectContext.ValidateForSource() error = %v", err)
	}
	if err := candidateDelivery.Validate(); err != nil {
		t.Fatalf("candidateDelivery.Validate() error = %v", err)
	}
	if err := reviewGate.Validate(); err != nil {
		t.Fatalf("reviewGate.Validate() error = %v", err)
	}
	if err := checkpoint.Validate(); err != nil {
		t.Fatalf("checkpoint.Validate() error = %v", err)
	}
	if err := actionSummary.Validate(); err != nil {
		t.Fatalf("actionSummary.Validate() error = %v", err)
	}

	assertJSONHasKeys(t, ref, []string{"id", "object_type", "domain_id", "visibility", "contract_version", "created_at", "updated_at", "state", "type", "system", "locator"})
	assertJSONHasKeys(t, MustReason(ReasonCapabilityStaticUnsupported, map[string]any{"capability": CapabilitySubmitJob}), []string{"object_type", "reason_code", "category", "summary", "visibility", "details"})
	assertJSONHasKeys(t, decision, []string{"id", "object_type", "domain_id", "contract_version", "created_at", "updated_at", "visibility", "decision_code", "category", "summary", "recommended_actions", "details"})
	assertJSONHasKeys(t, candidateDelivery, []string{"kind", "reached", "reached_at", "summary", "artifact_ids"})
	assertJSONHasKeys(t, reviewGate, []string{"required", "reviewer_mode", "max_fix_rounds", "status", "fix_rounds_used"})
	assertJSONHasKeys(t, checkpoint, []string{"type", "summary", "captured_at", "stage", "artifact_ids", "base_sha", "branch", "reason"})
	assertJSONHasKeys(t, actionSummary, []string{"has_pending_external_actions", "pending_count", "pending_types"})
	assertJSONHasKeys(t, job, []string{"id", "object_type", "domain_id", "visibility", "contract_version", "created_at", "updated_at", "relations", "references", "reasons", "decision", "error_code", "state", "job_type", "action_summary"})
	assertJSONHasKeys(t, run, []string{"id", "object_type", "domain_id", "visibility", "contract_version", "created_at", "updated_at", "relations", "reasons", "decision", "state", "phase", "attempt", "candidate_delivery", "review_gate", "checkpoints"})
	assertJSONHasKeys(t, intervention, []string{"id", "object_type", "domain_id", "visibility", "contract_version", "created_at", "updated_at", "reasons", "state", "template_id", "summary", "required_inputs", "allowed_actions", "resolution"})
	assertJSONHasKeys(t, outcome, []string{"id", "object_type", "domain_id", "visibility", "contract_version", "created_at", "updated_at", "relations", "decision", "state", "summary", "completed_at"})
	assertJSONHasKeys(t, artifact, []string{"id", "object_type", "domain_id", "visibility", "contract_version", "created_at", "updated_at", "state", "kind", "role"})
	assertJSONHasKeys(t, action, []string{"id", "object_type", "domain_id", "visibility", "contract_version", "created_at", "updated_at", "relations", "reasons", "decision", "state", "type", "summary"})
	assertJSONHasKeys(t, instance, []string{"id", "object_type", "domain_id", "visibility", "contract_version", "created_at", "updated_at", "state", "name", "version", "role", "static_capabilities", "available_capabilities"})
	assertJSONHasKeys(t, capabilities, []string{"static", "available"})
}

func TestObjectContextValidateForSourceRejectsInvalidCoreRelations(t *testing.T) {
	validCases := []struct {
		name   string
		source ObjectType
		ctx    ObjectContext
	}{
		{
			name:   "job run and action relations",
			source: ObjectTypeJob,
			ctx: ObjectContext{
				Relations: []ObjectRelation{
					{Type: RelationTypeJobRun, TargetID: "run-1", TargetType: ObjectTypeRun},
					{Type: RelationTypeJobAction, TargetID: "act-1", TargetType: ObjectTypeAction},
				},
			},
		},
		{
			name:   "run core relations",
			source: ObjectTypeRun,
			ctx: ObjectContext{
				Relations: []ObjectRelation{
					{Type: RelationTypeRunIntervention, TargetID: "int-1", TargetType: ObjectTypeIntervention},
					{Type: RelationTypeRunOutcome, TargetID: "out-1", TargetType: ObjectTypeOutcome},
				},
			},
		},
		{
			name:   "outcome artifact relation",
			source: ObjectTypeOutcome,
			ctx: ObjectContext{
				Relations: []ObjectRelation{{Type: RelationTypeOutcomeArtifact, TargetID: "art-1", TargetType: ObjectTypeArtifact}},
			},
		},
		{
			name:   "intervention without outbound relation",
			source: ObjectTypeIntervention,
			ctx:    ObjectContext{},
		},
		{
			name:   "action reference relation",
			source: ObjectTypeAction,
			ctx: ObjectContext{
				Relations: []ObjectRelation{{Type: RelationTypeActionReference, TargetID: "ref-1", TargetType: ObjectTypeReference}},
			},
		},
	}

	for _, tc := range validCases {
		if err := tc.ctx.ValidateForSource(tc.source); err != nil {
			t.Fatalf("%s: ValidateForSource() error = %v", tc.name, err)
		}
	}

	invalidCases := []struct {
		name   string
		source ObjectType
		ctx    ObjectContext
	}{
		{
			name:   "job cannot point to outcome",
			source: ObjectTypeJob,
			ctx: ObjectContext{
				Relations: []ObjectRelation{{Type: RelationTypeRunOutcome, TargetID: "out-1", TargetType: ObjectTypeOutcome}},
			},
		},
		{
			name:   "run outcome must target outcome type",
			source: ObjectTypeRun,
			ctx: ObjectContext{
				Relations: []ObjectRelation{{Type: RelationTypeRunOutcome, TargetID: "out-1", TargetType: ObjectTypeArtifact}},
			},
		},
		{
			name:   "artifact cannot define outbound relation",
			source: ObjectTypeArtifact,
			ctx: ObjectContext{
				Relations: []ObjectRelation{{Type: RelationTypeOutcomeArtifact, TargetID: "art-1", TargetType: ObjectTypeArtifact}},
			},
		},
		{
			name:   "relation target id required",
			source: ObjectTypeOutcome,
			ctx: ObjectContext{
				Relations: []ObjectRelation{{Type: RelationTypeOutcomeArtifact, TargetID: "", TargetType: ObjectTypeArtifact}},
			},
		},
		{
			name:   "action must target reference",
			source: ObjectTypeAction,
			ctx: ObjectContext{
				Relations: []ObjectRelation{{Type: RelationTypeActionReference, TargetID: "out-1", TargetType: ObjectTypeOutcome}},
			},
		},
	}

	for _, tc := range invalidCases {
		if err := tc.ctx.ValidateForSource(tc.source); err == nil {
			t.Fatalf("%s: ValidateForSource() = nil, want error", tc.name)
		}
	}
}

func TestOfficialJobTypeDefinitionsAreCompleteAndCloneSafe(t *testing.T) {
	definitions := OfficialJobTypes()
	if len(definitions) != 4 {
		t.Fatalf("len(OfficialJobTypes()) = %d, want 4", len(definitions))
	}

	found := map[JobType]JobTypeDefinition{}
	for _, definition := range definitions {
		if err := definition.Validate(); err != nil {
			t.Fatalf("JobTypeDefinition(%q).Validate() error = %v", definition.Type, err)
		}
		found[definition.Type] = definition
	}

	codeChange := found[JobTypeCodeChange]
	if !codeChange.Target.Required || !codeChange.Target.RequiresCodeSpace {
		t.Fatal("code_change must require a formal code-space target")
	}
	if codeChange.CandidateDelivery.Kind != CandidateDeliveryPointReviewablePullRequest {
		t.Fatalf("code_change candidate delivery = %q, want %q", codeChange.CandidateDelivery.Kind, CandidateDeliveryPointReviewablePullRequest)
	}
	if codeChange.BusinessCheckpoint.Type != CheckpointTypeBusiness {
		t.Fatalf("code_change business checkpoint type = %q, want %q", codeChange.BusinessCheckpoint.Type, CheckpointTypeBusiness)
	}
	if !codeChange.ReviewGate.Required || codeChange.ReviewGate.MaxFixRounds != 2 || codeChange.ReviewGate.ReviewerMode != ReviewerModeReadOnly {
		t.Fatal("code_change review gate must require readonly review with max 2 fix rounds")
	}
	if codeChange.DefaultArtifacts[0].Kind != ArtifactKindPullRequest {
		t.Fatalf("code_change primary artifact = %q, want %q", codeChange.DefaultArtifacts[0].Kind, ArtifactKindPullRequest)
	}

	landChange := found[JobTypeLandChange]
	if landChange.CandidateDelivery.Kind != CandidateDeliveryPointTargetPRSnapshot {
		t.Fatalf("land_change candidate delivery = %q, want %q", landChange.CandidateDelivery.Kind, CandidateDeliveryPointTargetPRSnapshot)
	}
	if !landChange.CompletionCriteria[0].RequiresMergedTarget {
		t.Fatal("land_change completion criterion must require merged target PR evidence")
	}
	if landChange.CompletionCriteria[0].RequiredReferenceTypes[0] != ReferenceTypeGitHubPullRequest {
		t.Fatalf("land_change completion reference = %q, want %q", landChange.CompletionCriteria[0].RequiredReferenceTypes[0], ReferenceTypeGitHubPullRequest)
	}
	if landChange.DefaultArtifacts[0].Kind != ArtifactKindLandedChangeRecord {
		t.Fatalf("land_change primary artifact = %q, want %q", landChange.DefaultArtifacts[0].Kind, ArtifactKindLandedChangeRecord)
	}

	analysis := found[JobTypeAnalysis]
	if analysis.CandidateDelivery.Kind != CandidateDeliveryPointAnalysisReportDraft {
		t.Fatalf("analysis candidate delivery = %q, want %q", analysis.CandidateDelivery.Kind, CandidateDeliveryPointAnalysisReportDraft)
	}
	if analysis.DefaultArtifacts[0].Kind != ArtifactKindAnalysisReport {
		t.Fatalf("analysis primary artifact = %q, want %q", analysis.DefaultArtifacts[0].Kind, ArtifactKindAnalysisReport)
	}

	diagnostic := found[JobTypeDiagnostic]
	if diagnostic.CandidateDelivery.Kind != CandidateDeliveryPointDiagnosticReportDraft {
		t.Fatalf("diagnostic candidate delivery = %q, want %q", diagnostic.CandidateDelivery.Kind, CandidateDeliveryPointDiagnosticReportDraft)
	}
	if diagnostic.DefaultArtifacts[0].Kind != ArtifactKindDiagnosticReport {
		t.Fatalf("diagnostic primary artifact = %q, want %q", diagnostic.DefaultArtifacts[0].Kind, ArtifactKindDiagnosticReport)
	}
	if JobType("documentation").IsValid() {
		t.Fatal(`JobType("documentation").IsValid() = true, want false`)
	}

	first, ok := DescribeJobType(JobTypeCodeChange)
	if !ok {
		t.Fatal("DescribeJobType(code_change) = false, want true")
	}
	first.Target.AllowedReferenceTypes[0] = ReferenceTypeReportURL
	first.CandidateDelivery.RequiredArtifactKinds[0] = ArtifactKindAnalysisReport
	first.BusinessCheckpoint.RequiredReferenceTypes[0] = ReferenceTypeReportURL
	first.CompletionCriteria[0].RequiredArtifactKinds[0] = ArtifactKindAnalysisReport
	second, ok := DescribeJobType(JobTypeCodeChange)
	if !ok {
		t.Fatal("DescribeJobType(code_change) second lookup = false, want true")
	}
	if second.Target.AllowedReferenceTypes[0] != ReferenceTypeGitHubRepo {
		t.Fatalf("DescribeJobType(code_change) returned shared slice, got %q", second.Target.AllowedReferenceTypes[0])
	}
	if second.CandidateDelivery.RequiredArtifactKinds[0] != ArtifactKindPullRequest {
		t.Fatalf("DescribeJobType(code_change) returned shared candidate slice, got %q", second.CandidateDelivery.RequiredArtifactKinds[0])
	}
	if second.BusinessCheckpoint.RequiredReferenceTypes[0] != ReferenceTypeGitHubPullRequest {
		t.Fatalf("DescribeJobType(code_change) returned shared checkpoint slice, got %q", second.BusinessCheckpoint.RequiredReferenceTypes[0])
	}
	if second.CompletionCriteria[0].RequiredArtifactKinds[0] != ArtifactKindPullRequest {
		t.Fatalf("DescribeJobType(code_change) returned shared completion slice, got %q", second.CompletionCriteria[0].RequiredArtifactKinds[0])
	}
}
