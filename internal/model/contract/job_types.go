package contract

var officialJobTypeDefinitions = []JobTypeDefinition{
	{
		Type:    JobTypeCodeChange,
		Summary: "产出可审查的代码变更 PR，不负责推动最终落地。",
		Target: TargetReferencePolicy{
			Required:          true,
			RequiresCodeSpace: true,
			AllowedReferenceTypes: []ReferenceType{
				ReferenceTypeGitHubRepo,
				ReferenceTypeGitBranch,
			},
		},
		CandidateDelivery: CandidateDeliveryPoint{
			Kind:    CandidateDeliveryPointReviewablePullRequest,
			Summary: "候选交付点必须是已创建的 draft/open PR。",
			RequiredArtifactKinds: []ArtifactKind{
				ArtifactKindPullRequest,
			},
			RequiredReferenceTypes: []ReferenceType{
				ReferenceTypeGitHubPullRequest,
			},
		},
		BusinessCheckpoint: BusinessCheckpointRule{
			Type:                  CheckpointTypeBusiness,
			CandidateDeliveryKind: CandidateDeliveryPointReviewablePullRequest,
			Summary:               "业务 checkpoint 必须是远端 draft/open PR；本地 commit 或仅本地分支不算正式业务 checkpoint。",
			RequiredArtifactKinds: []ArtifactKind{
				ArtifactKindPullRequest,
			},
			RequiredReferenceTypes: []ReferenceType{
				ReferenceTypeGitHubPullRequest,
			},
			RequiresRemotePublication: true,
		},
		ReviewGate: ReviewGatePolicy{
			Required:     true,
			ReviewerMode: ReviewerModeReadOnly,
			MaxFixRounds: 2,
		},
		CompletionCriteria: []CompletionCriterion{
			{
				Code:               "reviewable_pr_created",
				Summary:            "存在可审查的 PR 产物且最终 Outcome 为 succeeded。",
				AcceptableOutcomes: []OutcomeConclusion{OutcomeConclusionSucceeded},
				RequiredArtifactKinds: []ArtifactKind{
					ArtifactKindPullRequest,
				},
			},
		},
		DefaultArtifacts: []ArtifactExpectation{
			{Role: ArtifactRolePrimary, Kind: ArtifactKindPullRequest, Required: true, Summary: "主产物为可审查 PR"},
			{Role: ArtifactRoleSupporting, Kind: ArtifactKindGitBranch, Required: false, Summary: "辅助产物可为工作分支"},
			{Role: ArtifactRoleSupporting, Kind: ArtifactKindPatchBundle, Required: false, Summary: "辅助产物可为补丁包"},
		},
		InterventionTemplates: []InterventionTemplate{
			{
				TemplateID:     "code_change.reviewable_pr_blocked",
				Summary:        "当代码变更无法形成合格 PR 时，请求人工给出结构化处理方向。",
				AllowedActions: []ControlAction{ControlActionResolveIntervention, ControlActionRetry, ControlActionCancel, ControlActionTerminate},
				RequiredInputs: []InterventionInputField{
					{Field: "resolution", Label: "处理方向", Kind: InterventionInputKindEnum, Required: true, AllowedValues: []string{"revise_scope", "retry_publish", "cancel_job"}, Description: "选择继续修订、重试发布 PR 或取消任务。", Visibility: VisibilityLevelRestricted, Sensitivity: SensitiveFieldClassOperationalMetadata},
					{Field: "target_branch", Label: "目标分支", Kind: InterventionInputKindString, Required: false, Description: "必要时提供应发布到的目标分支。", Visibility: VisibilityLevelRestricted, Sensitivity: SensitiveFieldClassExternalReference},
					{Field: "review_constraints", Label: "审查约束", Kind: InterventionInputKindStringList, Required: false, Description: "补充必须满足的审查或分支约束。", Visibility: VisibilityLevelRestricted, Sensitivity: SensitiveFieldClassHumanInput},
				},
				AllowsSupplementalText: true,
			},
		},
	},
	{
		Type:    JobTypeLandChange,
		Summary: "推动已经存在的变更完成正式落地，职责与 code_change 分离。",
		Target: TargetReferencePolicy{
			Required:          true,
			RequiresCodeSpace: true,
			AllowedReferenceTypes: []ReferenceType{
				ReferenceTypeGitHubRepo,
				ReferenceTypeGitBranch,
				ReferenceTypeGitHubPullRequest,
			},
		},
		CandidateDelivery: CandidateDeliveryPoint{
			Kind:    CandidateDeliveryPointTargetPRSnapshot,
			Summary: "候选交付点必须是可推进的目标 PR 快照。",
			RequiredReferenceTypes: []ReferenceType{
				ReferenceTypeGitHubPullRequest,
			},
		},
		BusinessCheckpoint: BusinessCheckpointRule{
			Type:                  CheckpointTypeBusiness,
			CandidateDeliveryKind: CandidateDeliveryPointTargetPRSnapshot,
			Summary:               "业务 checkpoint 必须记录可推进的目标 PR 快照，不以本地临时提交代替。",
			RequiredReferenceTypes: []ReferenceType{
				ReferenceTypeGitHubPullRequest,
			},
			RequiresRemotePublication: true,
		},
		ReviewGate: ReviewGatePolicy{
			Required:     true,
			ReviewerMode: ReviewerModeReadOnly,
			MaxFixRounds: 2,
		},
		CompletionCriteria: []CompletionCriterion{
			{
				Code:               "landed_change_recorded",
				Summary:            "正式完成点是目标 PR 已 merged，且存在 merge result 与 landed change record 正式产物。",
				AcceptableOutcomes: []OutcomeConclusion{OutcomeConclusionSucceeded},
				RequiredArtifactKinds: []ArtifactKind{
					ArtifactKindMergeResult,
					ArtifactKindLandedChangeRecord,
				},
				RequiredReferenceTypes: []ReferenceType{
					ReferenceTypeGitHubPullRequest,
				},
				RequiresMergedTarget: true,
			},
		},
		DefaultArtifacts: []ArtifactExpectation{
			{Role: ArtifactRolePrimary, Kind: ArtifactKindLandedChangeRecord, Required: true, Summary: "主产物为 landed change record"},
			{Role: ArtifactRoleSupporting, Kind: ArtifactKindMergeResult, Required: true, Summary: "必需产物包含 merge result 引用，用于证明目标 PR 已 merged"},
		},
		InterventionTemplates: []InterventionTemplate{
			{
				TemplateID:     "land_change.merge_blocked",
				Summary:        "当落地被分支策略、合并冲突或发布窗口阻塞时，请求人工给出处理方案。",
				AllowedActions: []ControlAction{ControlActionResolveIntervention, ControlActionRetry, ControlActionCancel, ControlActionTerminate},
				RequiredInputs: []InterventionInputField{
					{Field: "resolution", Label: "处理方向", Kind: InterventionInputKindEnum, Required: true, AllowedValues: []string{"retry_land", "change_strategy", "cancel_job"}, Description: "选择重试落地、修改策略或取消任务。", Visibility: VisibilityLevelRestricted, Sensitivity: SensitiveFieldClassOperationalMetadata},
					{Field: "merge_strategy", Label: "合并策略", Kind: InterventionInputKindEnum, Required: false, AllowedValues: []string{"merge", "squash", "rebase"}, Description: "必要时指定正式合并策略。", Visibility: VisibilityLevelRestricted, Sensitivity: SensitiveFieldClassOperationalMetadata},
					{Field: "release_constraints", Label: "发布约束", Kind: InterventionInputKindStringList, Required: false, Description: "补充发布窗口、冻结期或审批约束。", Visibility: VisibilityLevelRestricted, Sensitivity: SensitiveFieldClassHumanInput},
				},
				AllowsSupplementalText: true,
			},
		},
	},
	{
		Type:    JobTypeAnalysis,
		Summary: "产出结构化分析结论，documentation 作为 analysis 的结果路径存在，不单独升成一级 Job type。",
		Target: TargetReferencePolicy{
			Required:          false,
			RequiresCodeSpace: false,
			AllowedReferenceTypes: []ReferenceType{
				ReferenceTypeLinearIssue,
				ReferenceTypeGitHubRepo,
				ReferenceTypeReportURL,
			},
		},
		CandidateDelivery: CandidateDeliveryPoint{
			Kind:    CandidateDeliveryPointAnalysisReportDraft,
			Summary: "候选交付点必须是正式分析报告草案。",
			RequiredArtifactKinds: []ArtifactKind{
				ArtifactKindAnalysisReport,
			},
		},
		BusinessCheckpoint: BusinessCheckpointRule{
			Type:                  CheckpointTypeBusiness,
			CandidateDeliveryKind: CandidateDeliveryPointAnalysisReportDraft,
			Summary:               "业务 checkpoint 必须记录正式分析报告草案。",
			RequiredArtifactKinds: []ArtifactKind{
				ArtifactKindAnalysisReport,
			},
			RequiresRemotePublication: false,
		},
		ReviewGate: ReviewGatePolicy{
			Required:     true,
			ReviewerMode: ReviewerModeReadOnly,
			MaxFixRounds: 2,
		},
		CompletionCriteria: []CompletionCriterion{
			{
				Code:               "analysis_report_recorded",
				Summary:            "存在分析报告产物且最终 Outcome 为 succeeded。",
				AcceptableOutcomes: []OutcomeConclusion{OutcomeConclusionSucceeded},
				RequiredArtifactKinds: []ArtifactKind{
					ArtifactKindAnalysisReport,
				},
			},
		},
		DefaultArtifacts: []ArtifactExpectation{
			{Role: ArtifactRolePrimary, Kind: ArtifactKindAnalysisReport, Required: true, Summary: "主产物为 Analysis Report"},
		},
		InterventionTemplates: []InterventionTemplate{
			{
				TemplateID:     "analysis.scope_clarification",
				Summary:        "当分析范围、验收口径或关注点不清晰时，请求人工补充结构化范围信息。",
				AllowedActions: []ControlAction{ControlActionResolveIntervention, ControlActionResume, ControlActionCancel, ControlActionTerminate},
				RequiredInputs: []InterventionInputField{
					{Field: "resolution", Label: "处理方向", Kind: InterventionInputKindEnum, Required: true, AllowedValues: []string{"continue", "narrow_scope", "cancel_job"}, Description: "选择继续分析、收窄范围或取消任务。", Visibility: VisibilityLevelRestricted, Sensitivity: SensitiveFieldClassOperationalMetadata},
					{Field: "focus_areas", Label: "关注点", Kind: InterventionInputKindStringList, Required: false, Description: "列出优先关注的分析维度。", Visibility: VisibilityLevelRestricted, Sensitivity: SensitiveFieldClassHumanInput},
					{Field: "acceptance_questions", Label: "验收问题", Kind: InterventionInputKindStringList, Required: false, Description: "列出必须回答的关键问题。", Visibility: VisibilityLevelRestricted, Sensitivity: SensitiveFieldClassHumanInput},
				},
				AllowsSupplementalText: true,
			},
		},
	},
	{
		Type:    JobTypeDiagnostic,
		Summary: "产出结构化诊断结论，主产物为 Diagnostic Report，必要时附带 Evidence Bundle。",
		Target: TargetReferencePolicy{
			Required:          false,
			RequiresCodeSpace: false,
			AllowedReferenceTypes: []ReferenceType{
				ReferenceTypeLinearIssue,
				ReferenceTypeGitHubRepo,
				ReferenceTypeReportURL,
			},
		},
		CandidateDelivery: CandidateDeliveryPoint{
			Kind:    CandidateDeliveryPointDiagnosticReportDraft,
			Summary: "候选交付点必须是正式诊断报告草案。",
			RequiredArtifactKinds: []ArtifactKind{
				ArtifactKindDiagnosticReport,
			},
		},
		BusinessCheckpoint: BusinessCheckpointRule{
			Type:                  CheckpointTypeBusiness,
			CandidateDeliveryKind: CandidateDeliveryPointDiagnosticReportDraft,
			Summary:               "业务 checkpoint 必须记录正式诊断报告草案。",
			RequiredArtifactKinds: []ArtifactKind{
				ArtifactKindDiagnosticReport,
			},
			RequiresRemotePublication: false,
		},
		ReviewGate: ReviewGatePolicy{
			Required:     true,
			ReviewerMode: ReviewerModeReadOnly,
			MaxFixRounds: 2,
		},
		CompletionCriteria: []CompletionCriterion{
			{
				Code:               "diagnostic_report_recorded",
				Summary:            "存在诊断报告产物且最终 Outcome 为 succeeded。",
				AcceptableOutcomes: []OutcomeConclusion{OutcomeConclusionSucceeded},
				RequiredArtifactKinds: []ArtifactKind{
					ArtifactKindDiagnosticReport,
				},
			},
		},
		DefaultArtifacts: []ArtifactExpectation{
			{Role: ArtifactRolePrimary, Kind: ArtifactKindDiagnosticReport, Required: true, Summary: "主产物为 Diagnostic Report"},
			{Role: ArtifactRoleSupporting, Kind: ArtifactKindEvidenceBundle, Required: false, Summary: "必要时附 Evidence Bundle"},
		},
		InterventionTemplates: []InterventionTemplate{
			{
				TemplateID:     "diagnostic.environment_clarification",
				Summary:        "当诊断缺少复现条件、日志范围或环境边界时，请求人工补充结构化上下文。",
				AllowedActions: []ControlAction{ControlActionResolveIntervention, ControlActionResume, ControlActionCancel, ControlActionTerminate},
				RequiredInputs: []InterventionInputField{
					{Field: "resolution", Label: "处理方向", Kind: InterventionInputKindEnum, Required: true, AllowedValues: []string{"continue", "request_logs", "cancel_job"}, Description: "选择继续诊断、补充日志或取消任务。", Visibility: VisibilityLevelRestricted, Sensitivity: SensitiveFieldClassOperationalMetadata},
					{Field: "reproduction_steps", Label: "复现步骤", Kind: InterventionInputKindStringList, Required: false, Description: "列出最关键的复现步骤。", Visibility: VisibilityLevelRestricted, Sensitivity: SensitiveFieldClassHumanInput},
					{Field: "suspected_scope", Label: "怀疑范围", Kind: InterventionInputKindStringList, Required: false, Description: "列出优先排查的服务、模块或依赖。", Visibility: VisibilityLevelRestricted, Sensitivity: SensitiveFieldClassHumanInput},
				},
				AllowsSupplementalText: true,
			},
		},
	},
}

func OfficialJobTypes() []JobTypeDefinition {
	result := make([]JobTypeDefinition, len(officialJobTypeDefinitions))
	for i, definition := range officialJobTypeDefinitions {
		result[i] = cloneJobTypeDefinition(definition)
	}
	return result
}

func DescribeJobType(jobType JobType) (JobTypeDefinition, bool) {
	for _, definition := range officialJobTypeDefinitions {
		if definition.Type == jobType {
			return cloneJobTypeDefinition(definition), true
		}
	}
	return JobTypeDefinition{}, false
}

func cloneJobTypeDefinition(definition JobTypeDefinition) JobTypeDefinition {
	clone := definition
	clone.Target.AllowedReferenceTypes = append([]ReferenceType(nil), definition.Target.AllowedReferenceTypes...)
	clone.CandidateDelivery = CandidateDeliveryPoint{
		Kind:                   definition.CandidateDelivery.Kind,
		Summary:                definition.CandidateDelivery.Summary,
		RequiredArtifactKinds:  append([]ArtifactKind(nil), definition.CandidateDelivery.RequiredArtifactKinds...),
		RequiredReferenceTypes: append([]ReferenceType(nil), definition.CandidateDelivery.RequiredReferenceTypes...),
	}
	clone.BusinessCheckpoint = BusinessCheckpointRule{
		Type:                      definition.BusinessCheckpoint.Type,
		CandidateDeliveryKind:     definition.BusinessCheckpoint.CandidateDeliveryKind,
		Summary:                   definition.BusinessCheckpoint.Summary,
		RequiredArtifactKinds:     append([]ArtifactKind(nil), definition.BusinessCheckpoint.RequiredArtifactKinds...),
		RequiredReferenceTypes:    append([]ReferenceType(nil), definition.BusinessCheckpoint.RequiredReferenceTypes...),
		RequiresRemotePublication: definition.BusinessCheckpoint.RequiresRemotePublication,
	}
	clone.ReviewGate = ReviewGatePolicy{
		Required:     definition.ReviewGate.Required,
		ReviewerMode: definition.ReviewGate.ReviewerMode,
		MaxFixRounds: definition.ReviewGate.MaxFixRounds,
	}
	clone.CompletionCriteria = make([]CompletionCriterion, len(definition.CompletionCriteria))
	for i, criterion := range definition.CompletionCriteria {
		clone.CompletionCriteria[i] = CompletionCriterion{
			Code:                   criterion.Code,
			Summary:                criterion.Summary,
			AcceptableOutcomes:     append([]OutcomeConclusion(nil), criterion.AcceptableOutcomes...),
			RequiredArtifactKinds:  append([]ArtifactKind(nil), criterion.RequiredArtifactKinds...),
			RequiredReferenceTypes: append([]ReferenceType(nil), criterion.RequiredReferenceTypes...),
			RequiresMergedTarget:   criterion.RequiresMergedTarget,
		}
	}
	clone.DefaultArtifacts = append([]ArtifactExpectation(nil), definition.DefaultArtifacts...)
	clone.InterventionTemplates = make([]InterventionTemplate, len(definition.InterventionTemplates))
	for i, template := range definition.InterventionTemplates {
		clone.InterventionTemplates[i] = InterventionTemplate{
			TemplateID:             template.TemplateID,
			Summary:                template.Summary,
			AllowedActions:         append([]ControlAction(nil), template.AllowedActions...),
			RequiredInputs:         cloneInterventionInputs(template.RequiredInputs),
			AllowsSupplementalText: template.AllowsSupplementalText,
		}
	}
	return clone
}

func cloneInterventionInputs(fields []InterventionInputField) []InterventionInputField {
	if len(fields) == 0 {
		return nil
	}
	clone := make([]InterventionInputField, len(fields))
	for i, field := range fields {
		clone[i] = InterventionInputField{
			Field:         field.Field,
			Label:         field.Label,
			Kind:          field.Kind,
			Required:      field.Required,
			AllowedValues: append([]string(nil), field.AllowedValues...),
			Description:   field.Description,
			Visibility:    field.Visibility,
			Sensitivity:   field.Sensitivity,
		}
	}
	return clone
}
