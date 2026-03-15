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
	if !RunStatusInterventionRequired.CanTransitionTo(RunStatusRunning) {
		t.Fatal("intervention_required -> running must be allowed")
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
}

func TestReasonDecisionAndErrorDescriptorsAreStructured(t *testing.T) {
	inputDetails := map[string]any{"capability": string(CapabilitySubmitJob)}
	reason := MustReason(ReasonCapabilityCurrentlyUnavailable, inputDetails)
	inputDetails["capability"] = "mutated"

	if reason.ObjectType != ObjectTypeReason {
		t.Fatalf("reason.ObjectType = %q, want %q", reason.ObjectType, ObjectTypeReason)
	}
	if reason.Category != CategoryCapability {
		t.Fatalf("reason.Category = %q, want %q", reason.Category, CategoryCapability)
	}
	if reason.Visibility != VisibilityLevelSummary {
		t.Fatalf("reason.Visibility = %q, want %q", reason.Visibility, VisibilityLevelSummary)
	}
	if reason.Summary == "" {
		t.Fatal("reason.Summary must not be empty")
	}
	if reason.Details["capability"] != string(CapabilitySubmitJob) {
		t.Fatalf("reason.Details[capability] = %v, want %q", reason.Details["capability"], CapabilitySubmitJob)
	}

	decision := MustDecision(DecisionResumeAfterIntervention, map[string]any{"intervention_id": "int-1"})
	if decision.ObjectType != ObjectTypeDecision {
		t.Fatalf("decision.ObjectType = %q, want %q", decision.ObjectType, ObjectTypeDecision)
	}
	if decision.Category != CategoryIntervention {
		t.Fatalf("decision.Category = %q, want %q", decision.Category, CategoryIntervention)
	}
	if len(decision.RecommendedActions) != 2 {
		t.Fatalf("len(decision.RecommendedActions) = %d, want 2", len(decision.RecommendedActions))
	}
	if decision.RecommendedActions[0].Control != ControlActionResolveIntervention {
		t.Fatalf("first recommended control = %q, want %q", decision.RecommendedActions[0].Control, ControlActionResolveIntervention)
	}

	errResp := MustErrorResponse(ErrorCapabilityUnavailable, "submit_job unavailable", map[string]any{"capability": CapabilitySubmitJob})
	if errResp.Category != CategoryCapability {
		t.Fatalf("errResp.Category = %q, want %q", errResp.Category, CategoryCapability)
	}
	if !errResp.Retryable {
		t.Fatal("errResp.Retryable = false, want true")
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

	job := Job{
		BaseObject: BaseObject{ID: "job-1", ObjectType: ObjectTypeJob, DomainID: "domain-main", Visibility: VisibilityLevelRestricted, ContractVersion: APIVersionV1, CreatedAt: "2026-03-15T00:00:00Z", UpdatedAt: "2026-03-15T00:00:01Z"},
		ObjectContext: ObjectContext{
			Relations:  []ObjectRelation{{Type: RelationTypeJobRun, TargetID: "run-1", TargetType: ObjectTypeRun}},
			References: []Reference{ref},
			Reasons:    []Reason{MustReason(ReasonJobTargetReferenceMissing, map[string]any{"reference_type": "github_repo"})},
			Decision:   &decision,
			ErrorCode:  ErrorAPIInvalidRequest,
		},
		State:   JobStatusQueued,
		JobType: JobTypeCodeChange,
	}
	run := Run{
		BaseObject: BaseObject{ID: "run-1", ObjectType: ObjectTypeRun, DomainID: "domain-main", Visibility: VisibilityLevelRestricted, ContractVersion: APIVersionV1, CreatedAt: "2026-03-15T00:00:00Z", UpdatedAt: "2026-03-15T00:00:02Z"},
		ObjectContext: ObjectContext{
			Relations: []ObjectRelation{
				{Type: RelationTypeRunIntervention, TargetID: "int-1", TargetType: ObjectTypeIntervention},
				{Type: RelationTypeRunOutcome, TargetID: "out-1", TargetType: ObjectTypeOutcome},
			},
			Reasons:  []Reason{MustReason(ReasonRunBlockedInterventionRequired, map[string]any{"intervention_id": "int-1"})},
			Decision: &decision,
		},
		State:   RunStatusInterventionRequired,
		Phase:   RunPhaseSummaryBlocked,
		Attempt: 2,
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

	assertJSONHasKeys(t, ref, []string{"id", "object_type", "domain_id", "visibility", "contract_version", "created_at", "updated_at", "state", "type", "system", "locator"})
	assertJSONHasKeys(t, MustReason(ReasonCapabilityStaticUnsupported, map[string]any{"capability": CapabilitySubmitJob}), []string{"object_type", "reason_code", "category", "summary", "visibility", "details"})
	assertJSONHasKeys(t, decision, []string{"id", "object_type", "domain_id", "contract_version", "created_at", "updated_at", "visibility", "decision_code", "category", "summary", "recommended_actions", "details"})
	assertJSONHasKeys(t, job, []string{"id", "object_type", "domain_id", "visibility", "contract_version", "created_at", "updated_at", "relations", "references", "reasons", "decision", "error_code", "state", "job_type"})
	assertJSONHasKeys(t, run, []string{"id", "object_type", "domain_id", "visibility", "contract_version", "created_at", "updated_at", "relations", "reasons", "decision", "state", "phase", "attempt"})
	assertJSONHasKeys(t, intervention, []string{"id", "object_type", "domain_id", "visibility", "contract_version", "created_at", "updated_at", "reasons", "state", "template_id", "summary", "required_inputs", "allowed_actions", "resolution"})
	assertJSONHasKeys(t, outcome, []string{"id", "object_type", "domain_id", "visibility", "contract_version", "created_at", "updated_at", "relations", "decision", "state", "summary", "completed_at"})
	assertJSONHasKeys(t, artifact, []string{"id", "object_type", "domain_id", "visibility", "contract_version", "created_at", "updated_at", "state", "kind", "role"})
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
			name:   "job run relation",
			source: ObjectTypeJob,
			ctx: ObjectContext{
				Relations: []ObjectRelation{{Type: RelationTypeJobRun, TargetID: "run-1", TargetType: ObjectTypeRun}},
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
	if codeChange.DefaultArtifacts[0].Kind != ArtifactKindPullRequest {
		t.Fatalf("code_change primary artifact = %q, want %q", codeChange.DefaultArtifacts[0].Kind, ArtifactKindPullRequest)
	}

	landChange := found[JobTypeLandChange]
	if landChange.DefaultArtifacts[0].Kind != ArtifactKindLandedChangeRecord {
		t.Fatalf("land_change primary artifact = %q, want %q", landChange.DefaultArtifacts[0].Kind, ArtifactKindLandedChangeRecord)
	}

	analysis := found[JobTypeAnalysis]
	if analysis.DefaultArtifacts[0].Kind != ArtifactKindAnalysisReport {
		t.Fatalf("analysis primary artifact = %q, want %q", analysis.DefaultArtifacts[0].Kind, ArtifactKindAnalysisReport)
	}

	diagnostic := found[JobTypeDiagnostic]
	if diagnostic.DefaultArtifacts[0].Kind != ArtifactKindDiagnosticReport {
		t.Fatalf("diagnostic primary artifact = %q, want %q", diagnostic.DefaultArtifacts[0].Kind, ArtifactKindDiagnosticReport)
	}

	first, ok := DescribeJobType(JobTypeCodeChange)
	if !ok {
		t.Fatal("DescribeJobType(code_change) = false, want true")
	}
	first.Target.AllowedReferenceTypes[0] = ReferenceTypeReportURL
	second, ok := DescribeJobType(JobTypeCodeChange)
	if !ok {
		t.Fatal("DescribeJobType(code_change) second lookup = false, want true")
	}
	if second.Target.AllowedReferenceTypes[0] != ReferenceTypeGitHubRepo {
		t.Fatalf("DescribeJobType(code_change) returned shared slice, got %q", second.Target.AllowedReferenceTypes[0])
	}
}
