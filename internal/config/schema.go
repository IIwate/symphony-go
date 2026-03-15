package config

import (
	"fmt"
	"strings"

	"symphony-go/internal/model"
	"symphony-go/internal/model/contract"
	"symphony-go/internal/secret"
)

type PersistenceBackendKind string

const (
	PersistenceBackendKindFile       PersistenceBackendKind = "file"
	PersistenceBackendKindRelational PersistenceBackendKind = "relational"
)

func (k PersistenceBackendKind) IsValid() bool {
	switch k {
	case PersistenceBackendKindFile, PersistenceBackendKindRelational:
		return true
	default:
		return false
	}
}

type PersistenceUsage string

const (
	PersistenceUsageDevelopment PersistenceUsage = "development"
	PersistenceUsageTest        PersistenceUsage = "test"
	PersistenceUsageSingleNode  PersistenceUsage = "single_machine"
	PersistenceUsageProduction  PersistenceUsage = "production"
)

func (u PersistenceUsage) IsValid() bool {
	switch u {
	case PersistenceUsageDevelopment, PersistenceUsageTest, PersistenceUsageSingleNode, PersistenceUsageProduction:
		return true
	default:
		return false
	}
}

type SecretProviderContract struct {
	Name string
	Kind string
}

type SecretContract struct {
	EnvironmentEnabled bool
	ExternalProviders  []SecretProviderContract
}

type ServiceContract struct {
	ContractVersion contract.APIVersion
	InstanceName    string
	ServerHost      string
	ServerPort      *int
	Notifications   NotificationContract
}

type DomainContract struct {
	ID              string
	PollIntervalMS  int
	WorkspaceRoot   string
	BranchNamespace string
	GitAuthorName   string
	GitAuthorEmail  string
}

type SourceAdapterContract struct {
	Name                      string
	Kind                      string
	Endpoint                  string
	APIKeyRef                 secret.Reference
	ProjectSlug               string
	BranchScope               string
	Repo                      string
	ActiveStates              []string
	TerminalStates            []string
	LinearChildrenBlockParent bool
}

type CodexBackendContract struct {
	Command           string
	ApprovalPolicy    string
	ThreadSandbox     string
	TurnSandboxPolicy string
	TurnTimeoutMS     int
	ReadTimeoutMS     int
	StallTimeoutMS    int
}

type ExecutionContract struct {
	BackendKind                string
	Codex                      CodexBackendContract
	MaxConcurrentAgents        int
	MaxTurns                   int
	MaxRetryBackoffMS          int
	RunBudgetTotalMS           int
	RunExecutionBudgetMS       int
	RunReviewFixBudgetMS       int
	MaxConcurrentAgentsByState map[string]int
	HookAfterCreate            *string
	HookBeforeRun              *string
	HookBeforeRunContinuation  *string
	HookAfterRun               *string
	HookBeforeRemove           *string
	HookTimeoutMS              int
}

type JobPolicyContract struct {
	DispatchFlow      string
	SupportedJobTypes []contract.JobType
}

type AuthContract struct {
	Mode                  string
	LeaderRequired        bool
	TransparentForwarding bool
}

type PersistenceContract struct {
	BackendKind         PersistenceBackendKind
	Usage               PersistenceUsage
	FilePath            string
	FlushIntervalMS     int
	FsyncOnCritical     bool
	ArchiveEnabled      bool
	AllowPhysicalDelete bool
}

type NotificationContract struct {
	Defaults NotificationDefaultsContract
	Channels []NotificationChannelContract
}

type NotificationDefaultsContract struct {
	TimeoutMS         int
	RetryCount        int
	RetryDelayMS      int
	QueueSize         int
	CriticalQueueSize int
}

type NotificationChannelContract struct {
	ID            string
	DisplayName   string
	Kind          model.NotificationChannelKind
	Subscriptions model.NotificationSubscriptionConfig
	Delivery      model.NotificationDeliveryConfig
	Webhook       *WebhookNotificationContract
	Slack         *SlackNotificationContract
}

type WebhookNotificationContract struct {
	URLRef     secret.Reference
	HeaderRefs map[string]secret.Reference
}

type SlackNotificationContract struct {
	IncomingWebhookURLRef secret.Reference
}

type WorkflowContract struct {
	Service      ServiceContract
	Domain       DomainContract
	Source       SourceAdapterContract
	Execution    ExecutionContract
	JobPolicy    JobPolicyContract
	Auth         AuthContract
	Persistence  PersistenceContract
	Secrets      SecretContract
	Capabilities contract.CapabilityContract
}

func ParseWorkflowContract(def *model.WorkflowDefinition) (WorkflowContract, error) {
	if def == nil {
		return WorkflowContract{}, model.NewWorkflowError(model.ErrWorkflowParseError, "workflow definition is nil", nil)
	}

	configMap := def.Config
	if configMap == nil {
		configMap = map[string]any{}
	}

	serviceMap := getMap(configMap, "service")
	domainMap := getMap(configMap, "domain")
	sourceMap := getMap(configMap, "source_adapter")
	executionMap := getMap(configMap, "execution")
	jobPolicyMap := getMap(configMap, "job_policy")
	authMap := getMap(configMap, "auth")
	persistenceMap := getMap(configMap, "persistence")
	secretsMap := getMap(configMap, "secrets")

	contractConfig := WorkflowContract{
		Service: ServiceContract{
			ContractVersion: contract.APIVersion(getString(serviceMap, "contract_version", string(contract.APIVersionV1))),
			InstanceName:    strings.TrimSpace(getString(serviceMap, "instance_name", "symphony")),
			ServerHost:      "127.0.0.1",
		},
		Domain: DomainContract{
			ID:              strings.TrimSpace(getString(domainMap, "id", "default")),
			WorkspaceRoot:   expandHomePath(strings.TrimSpace(getString(getMap(domainMap, "workspace"), "root", ""))),
			BranchNamespace: strings.TrimSpace(getString(getMap(domainMap, "workspace"), "branch_namespace", "")),
			GitAuthorName:   strings.TrimSpace(getString(getMap(getMap(domainMap, "workspace"), "git"), "author_name", "")),
			GitAuthorEmail:  strings.TrimSpace(getString(getMap(getMap(domainMap, "workspace"), "git"), "author_email", "")),
		},
		Source: SourceAdapterContract{
			Name:                      strings.TrimSpace(getString(sourceMap, "name", "")),
			Kind:                      model.NormalizeState(getString(sourceMap, "kind", "")),
			Endpoint:                  strings.TrimSpace(getString(sourceMap, "endpoint", "https://api.linear.app/graphql")),
			ProjectSlug:               strings.TrimSpace(getString(sourceMap, "project_slug", "")),
			BranchScope:               slugifyScopeValue(getString(sourceMap, "branch_scope", "")),
			Repo:                      strings.TrimSpace(getString(sourceMap, "repo", "")),
			ActiveStates:              []string{"Todo", "In Progress"},
			TerminalStates:            []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"},
			LinearChildrenBlockParent: true,
		},
		Execution: ExecutionContract{
			BackendKind:                model.NormalizeState(getString(getMap(executionMap, "backend"), "kind", "codex")),
			MaxConcurrentAgents:        10,
			MaxTurns:                   20,
			MaxRetryBackoffMS:          300000,
			RunBudgetTotalMS:           3600000,
			RunExecutionBudgetMS:       3600000,
			RunReviewFixBudgetMS:       0,
			MaxConcurrentAgentsByState: map[string]int{},
			HookTimeoutMS:              60000,
			Codex: CodexBackendContract{
				Command:           strings.TrimSpace(getString(getMap(getMap(executionMap, "backend"), "codex"), "command", "codex app-server")),
				ApprovalPolicy:    strings.TrimSpace(getString(getMap(getMap(executionMap, "backend"), "codex"), "approval_policy", "never")),
				ThreadSandbox:     strings.TrimSpace(getString(getMap(getMap(executionMap, "backend"), "codex"), "thread_sandbox", "workspace-write")),
				TurnSandboxPolicy: stringifyValue(getMap(getMap(getMap(executionMap, "backend"), "codex"), "turn_sandbox_policy")),
				TurnTimeoutMS:     3600000,
				ReadTimeoutMS:     5000,
				StallTimeoutMS:    300000,
			},
		},
		JobPolicy: JobPolicyContract{
			DispatchFlow:      strings.TrimSpace(getString(jobPolicyMap, "dispatch_flow", "implement")),
			SupportedJobTypes: parseJobTypes(jobPolicyMap),
		},
		Auth: AuthContract{
			Mode:                  strings.TrimSpace(getString(authMap, "mode", "none")),
			LeaderRequired:        true,
			TransparentForwarding: false,
		},
		Persistence: PersistenceContract{
			BackendKind:         PersistenceBackendKind(model.NormalizeState(getString(getMap(persistenceMap, "backend"), "kind", "file"))),
			Usage:               PersistenceUsage(model.NormalizeState(getString(getMap(persistenceMap, "backend"), "usage", string(PersistenceUsageDevelopment)))),
			FilePath:            expandHomePath(strings.TrimSpace(getString(getMap(persistenceMap, "file"), "path", "./local/runtime-ledger.json"))),
			FlushIntervalMS:     1000,
			FsyncOnCritical:     true,
			ArchiveEnabled:      false,
			AllowPhysicalDelete: false,
		},
		Secrets: SecretContract{
			EnvironmentEnabled: true,
		},
	}
	notificationsContract, err := parseNotificationsContract(getMap(serviceMap, "notifications"))
	if err != nil {
		return WorkflowContract{}, err
	}
	contractConfig.Service.Notifications = notificationsContract
	if host := strings.TrimSpace(getString(getMap(serviceMap, "server"), "host", "")); host != "" {
		contractConfig.Service.ServerHost = host
	}
	contractConfig.Secrets.ExternalProviders = parseExternalProviders(getMap(secretsMap, "providers")["external"])

	if port, ok := getInt(getMap(serviceMap, "server"), "port"); ok && port >= 0 {
		contractConfig.Service.ServerPort = &port
	}
	if interval, ok := getInt(getMap(domainMap, "polling"), "interval_ms"); ok && interval > 0 {
		contractConfig.Domain.PollIntervalMS = interval
	} else {
		contractConfig.Domain.PollIntervalMS = 30000
	}

	if childrenBlockParent, ok := getBool(getMap(sourceMap, "linear"), "children_block_parent"); ok {
		contractConfig.Source.LinearChildrenBlockParent = childrenBlockParent
	}
	if values, ok := getStringSlice(sourceMap, "active_states"); ok && len(values) > 0 {
		contractConfig.Source.ActiveStates = values
	}
	if values, ok := getStringSlice(sourceMap, "terminal_states"); ok && len(values) > 0 {
		contractConfig.Source.TerminalStates = values
	}
	apiKeyRef, err := parseSecretReference(getMap(getMap(sourceMap, "credentials"), "api_key_ref"), "source_adapter.credentials.api_key_ref")
	if err != nil {
		return WorkflowContract{}, err
	}
	contractConfig.Source.APIKeyRef = apiKeyRef

	executionAgent := getMap(executionMap, "agent")
	if value, ok := getInt(executionAgent, "max_concurrent_agents"); ok && value > 0 {
		contractConfig.Execution.MaxConcurrentAgents = value
	}
	if value, ok := getInt(executionAgent, "max_turns"); ok && value > 0 {
		contractConfig.Execution.MaxTurns = value
	}
	if value, ok := getInt(executionAgent, "max_retry_backoff_ms"); ok && value > 0 {
		contractConfig.Execution.MaxRetryBackoffMS = value
	}
	if budgets := getMap(executionAgent, "run_budget_ms"); len(budgets) > 0 {
		if value, ok := getInt(budgets, "total"); ok && value > 0 {
			contractConfig.Execution.RunBudgetTotalMS = value
		}
		if value, ok := getInt(budgets, "execution"); ok && value > 0 {
			contractConfig.Execution.RunExecutionBudgetMS = value
		}
		if value, ok := getInt(budgets, "review_fix"); ok && value >= 0 {
			contractConfig.Execution.RunReviewFixBudgetMS = value
		}
	}
	if byState := getMap(executionAgent, "max_concurrent_agents_by_state"); len(byState) > 0 {
		contractConfig.Execution.MaxConcurrentAgentsByState = normalizePositiveIntMap(byState)
	}
	executionHooks := getMap(executionMap, "hooks")
	if value, ok := getOptionalString(executionHooks, "after_create"); ok {
		contractConfig.Execution.HookAfterCreate = stringPointer(value)
	}
	if value, ok := getOptionalString(executionHooks, "before_run"); ok {
		contractConfig.Execution.HookBeforeRun = stringPointer(value)
	}
	if value, ok := getOptionalString(executionHooks, "before_run_continuation"); ok {
		contractConfig.Execution.HookBeforeRunContinuation = stringPointer(value)
	}
	if value, ok := getOptionalString(executionHooks, "after_run"); ok {
		contractConfig.Execution.HookAfterRun = stringPointer(value)
	}
	if value, ok := getOptionalString(executionHooks, "before_remove"); ok {
		contractConfig.Execution.HookBeforeRemove = stringPointer(value)
	}
	if value, ok := getInt(executionHooks, "timeout_ms"); ok && value > 0 {
		contractConfig.Execution.HookTimeoutMS = value
	}
	codexMap := getMap(getMap(executionMap, "backend"), "codex")
	if value, ok := getInt(codexMap, "turn_timeout_ms"); ok && value > 0 {
		contractConfig.Execution.Codex.TurnTimeoutMS = value
		if !hasKey(getMap(executionAgent, "run_budget_ms"), "execution") {
			contractConfig.Execution.RunExecutionBudgetMS = value
		}
		if !hasKey(getMap(executionAgent, "run_budget_ms"), "total") {
			contractConfig.Execution.RunBudgetTotalMS = value + contractConfig.Execution.RunReviewFixBudgetMS
		}
	}
	if value, ok := getInt(codexMap, "read_timeout_ms"); ok && value > 0 {
		contractConfig.Execution.Codex.ReadTimeoutMS = value
	}
	if value, ok := getInt(codexMap, "stall_timeout_ms"); ok && value >= 0 {
		contractConfig.Execution.Codex.StallTimeoutMS = value
	}
	if policy, exists := codexMap["turn_sandbox_policy"]; exists && policy != nil {
		contractConfig.Execution.Codex.TurnSandboxPolicy = stringifyValue(policy)
	}
	if contractConfig.Execution.Codex.TurnSandboxPolicy == "" {
		contractConfig.Execution.Codex.TurnSandboxPolicy = `{"type":"workspaceWrite"}`
	}

	if leaderRequired, ok := getBool(authMap, "leader_required"); ok {
		contractConfig.Auth.LeaderRequired = leaderRequired
	}
	if transparentForwarding, ok := getBool(authMap, "transparent_forwarding"); ok {
		contractConfig.Auth.TransparentForwarding = transparentForwarding
	}

	if envEnabled, ok := getBool(getMap(getMap(secretsMap, "providers"), "env"), "enabled"); ok {
		contractConfig.Secrets.EnvironmentEnabled = envEnabled
	}
	if flushInterval, ok := getInt(getMap(persistenceMap, "file"), "flush_interval_ms"); ok {
		contractConfig.Persistence.FlushIntervalMS = flushInterval
	}
	if fsyncOnCritical, ok := getBool(getMap(persistenceMap, "file"), "fsync_on_critical"); ok {
		contractConfig.Persistence.FsyncOnCritical = fsyncOnCritical
	}
	if archiveEnabled, ok := getBool(getMap(persistenceMap, "archive"), "enabled"); ok {
		contractConfig.Persistence.ArchiveEnabled = archiveEnabled
	}
	if allowPhysicalDelete, ok := getBool(getMap(persistenceMap, "retention"), "allow_physical_delete"); ok {
		contractConfig.Persistence.AllowPhysicalDelete = allowPhysicalDelete
	}

	contractConfig.Capabilities = buildCapabilityContract(contractConfig)
	if err := validateWorkflowContract(contractConfig); err != nil {
		return WorkflowContract{}, err
	}
	return contractConfig, nil
}

func parseNotificationsContract(source map[string]any) (NotificationContract, error) {
	contractConfig := NotificationContract{
		Defaults: NotificationDefaultsContract{
			TimeoutMS:         5000,
			RetryCount:        2,
			RetryDelayMS:      1000,
			QueueSize:         128,
			CriticalQueueSize: 32,
		},
	}
	defaults := getMap(source, "defaults")
	if value, ok := getInt(defaults, "timeout_ms"); ok {
		contractConfig.Defaults.TimeoutMS = value
	}
	if value, ok := getInt(defaults, "retry_count"); ok {
		contractConfig.Defaults.RetryCount = value
	}
	if value, ok := getInt(defaults, "retry_delay_ms"); ok {
		contractConfig.Defaults.RetryDelayMS = value
	}
	if value, ok := getInt(defaults, "queue_size"); ok {
		contractConfig.Defaults.QueueSize = value
	}
	if value, ok := getInt(defaults, "critical_queue_size"); ok {
		contractConfig.Defaults.CriticalQueueSize = value
	}

	channelMaps := getMapSlice(source, "channels")
	if len(channelMaps) == 0 {
		return contractConfig, nil
	}

	contractConfig.Channels = make([]NotificationChannelContract, 0, len(channelMaps))
	for _, channel := range channelMaps {
		item := NotificationChannelContract{
			ID:          strings.TrimSpace(getString(channel, "id", "")),
			DisplayName: strings.TrimSpace(getString(channel, "display_name", "")),
			Kind:        model.NotificationChannelKind(model.NormalizeState(getString(channel, "kind", ""))),
			Delivery: model.NotificationDeliveryConfig{
				TimeoutMS:         contractConfig.Defaults.TimeoutMS,
				RetryCount:        contractConfig.Defaults.RetryCount,
				RetryDelayMS:      contractConfig.Defaults.RetryDelayMS,
				QueueSize:         contractConfig.Defaults.QueueSize,
				CriticalQueueSize: contractConfig.Defaults.CriticalQueueSize,
			},
		}
		if item.DisplayName == "" {
			item.DisplayName = item.ID
		}
		subscriptions := getMap(channel, "subscriptions")
		if families, ok := getStringSlice(subscriptions, "families"); ok {
			item.Subscriptions.Families = make([]model.RuntimeEventFamily, 0, len(families))
			for _, family := range families {
				item.Subscriptions.Families = append(item.Subscriptions.Families, model.RuntimeEventFamily(model.NormalizeState(family)))
			}
		}
		if events, ok := getStringSlice(subscriptions, "types"); ok {
			item.Subscriptions.Types = make([]model.NotificationEventType, 0, len(events))
			for _, eventName := range events {
				item.Subscriptions.Types = append(item.Subscriptions.Types, model.NotificationEventType(strings.ToLower(strings.TrimSpace(eventName))))
			}
		}
		delivery := getMap(channel, "delivery")
		if value, ok := getInt(delivery, "timeout_ms"); ok {
			item.Delivery.TimeoutMS = value
		}
		if value, ok := getInt(delivery, "retry_count"); ok {
			item.Delivery.RetryCount = value
		}
		if value, ok := getInt(delivery, "retry_delay_ms"); ok {
			item.Delivery.RetryDelayMS = value
		}
		if value, ok := getInt(delivery, "queue_size"); ok {
			item.Delivery.QueueSize = value
		}
		if value, ok := getInt(delivery, "critical_queue_size"); ok {
			item.Delivery.CriticalQueueSize = value
		}

		switch item.Kind {
		case model.NotificationChannelKindWebhook:
			webhook := getMap(channel, "webhook")
			if len(webhook) == 0 {
				break
			}
			urlRef, err := parseSecretReference(getMap(webhook, "url_ref"), fmt.Sprintf("service.notifications.channels[%d].webhook.url_ref", len(contractConfig.Channels)))
			if err != nil {
				return NotificationContract{}, err
			}
			headers := map[string]secret.Reference{}
			for key, value := range getMap(webhook, "header_refs") {
				ref, refErr := parseSecretReference(valueAsMap(value), fmt.Sprintf("service.notifications.channels[%d].webhook.header_refs.%s", len(contractConfig.Channels), key))
				if refErr != nil {
					return NotificationContract{}, refErr
				}
				headers[key] = ref
			}
			item.Webhook = &WebhookNotificationContract{
				URLRef:     urlRef,
				HeaderRefs: headers,
			}
		case model.NotificationChannelKindSlack:
			slack := getMap(channel, "slack")
			if len(slack) == 0 {
				break
			}
			urlRef, err := parseSecretReference(getMap(slack, "incoming_webhook_url_ref"), fmt.Sprintf("service.notifications.channels[%d].slack.incoming_webhook_url_ref", len(contractConfig.Channels)))
			if err != nil {
				return NotificationContract{}, err
			}
			item.Slack = &SlackNotificationContract{IncomingWebhookURLRef: urlRef}
		}
		contractConfig.Channels = append(contractConfig.Channels, item)
	}
	return contractConfig, nil
}

func parseExternalProviders(source any) []SecretProviderContract {
	var items []map[string]any
	switch typed := source.(type) {
	case []map[string]any:
		items = typed
	case []any:
		items = make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			items = append(items, valueAsMap(item))
		}
	}
	result := make([]SecretProviderContract, 0, len(items))
	for _, item := range items {
		result = append(result, SecretProviderContract{
			Name: strings.TrimSpace(getString(item, "name", "")),
			Kind: strings.TrimSpace(getString(item, "kind", "")),
		})
	}
	return result
}

func valueAsMap(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		return typed
	case map[any]any:
		result := make(map[string]any, len(typed))
		for key, nested := range typed {
			result[fmt.Sprint(key)] = nested
		}
		return result
	default:
		return map[string]any{}
	}
}

func hasKey(source map[string]any, key string) bool {
	if len(source) == 0 {
		return false
	}
	_, ok := source[key]
	return ok
}

func parseJobTypes(source map[string]any) []contract.JobType {
	values, ok := getStringSlice(source, "supported_types")
	if !ok || len(values) == 0 {
		return []contract.JobType{
			contract.JobTypeCodeChange,
			contract.JobTypeLandChange,
			contract.JobTypeAnalysis,
			contract.JobTypeDiagnostic,
		}
	}
	result := make([]contract.JobType, 0, len(values))
	for _, value := range values {
		result = append(result, contract.JobType(model.NormalizeState(value)))
	}
	return result
}

func parseSecretReference(source map[string]any, fieldPath string) (secret.Reference, error) {
	if len(source) == 0 {
		return secret.Reference{}, model.NewWorkflowError(model.ErrWorkflowParseError, fieldPath+" is required", nil)
	}
	ref := secret.Reference{
		Kind: secret.ReferenceKind(model.NormalizeState(getString(source, "kind", ""))),
		Name: strings.TrimSpace(getString(source, "name", "")),
	}
	switch ref.Kind {
	case secret.ReferenceKindEnv:
		if err := ref.Validate(); err != nil {
			return secret.Reference{}, model.NewWorkflowError(model.ErrWorkflowParseError, fieldPath+": "+err.Error(), nil)
		}
		return ref, nil
	case secret.ReferenceKindProvider:
		ref.Provider = &secret.ProviderReference{
			Name:     strings.TrimSpace(getString(source, "provider", "")),
			SecretID: strings.TrimSpace(getString(source, "secret_id", "")),
		}
		if version, ok := getOptionalString(source, "version"); ok {
			ref.Provider.Version = stringPointer(version)
		}
		if err := ref.Validate(); err != nil {
			return secret.Reference{}, model.NewWorkflowError(model.ErrWorkflowParseError, fieldPath+": "+err.Error(), nil)
		}
		return ref, nil
	default:
		return secret.Reference{}, model.NewWorkflowError(model.ErrWorkflowParseError, fieldPath+" kind is unsupported", nil)
	}
}

func validateWorkflowContract(cfg WorkflowContract) error {
	if cfg.Service.ContractVersion == "" {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "service.contract_version is required", nil)
	}
	if cfg.Service.InstanceName == "" {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "service.instance_name is required", nil)
	}
	if cfg.Domain.ID == "" {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "domain.id is required", nil)
	}
	if cfg.Domain.PollIntervalMS <= 0 {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "domain.polling.interval_ms must be > 0", nil)
	}
	if strings.TrimSpace(cfg.Domain.WorkspaceRoot) == "" {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "domain.workspace.root is required", nil)
	}
	if cfg.Source.Name == "" {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "source_adapter.name is required", nil)
	}
	if cfg.Source.Kind == "" {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "source_adapter.kind is required", nil)
	}
	if err := cfg.Source.APIKeyRef.Validate(); err != nil {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "source_adapter.credentials.api_key_ref: "+err.Error(), nil)
	}
	if cfg.Source.Kind == "linear" && strings.TrimSpace(cfg.Source.ProjectSlug) == "" {
		return model.NewTrackerError(model.ErrMissingTrackerProjectSlug, "source_adapter.project_slug is required", nil)
	}
	if cfg.Source.Kind == "linear" && strings.TrimSpace(cfg.Source.BranchScope) == "" {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "source_adapter.branch_scope is required for linear source", nil)
	}
	if cfg.Execution.BackendKind != "codex" {
		return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("execution.backend.kind %q is unsupported", cfg.Execution.BackendKind), nil)
	}
	if strings.TrimSpace(cfg.Execution.Codex.Command) == "" {
		return model.NewWorkflowError(model.ErrInvalidCodexCommand, "execution.backend.codex.command is required", nil)
	}
	if cfg.Execution.RunExecutionBudgetMS <= 0 {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "execution.agent.run_budget_ms.execution must be > 0", nil)
	}
	if cfg.Execution.RunReviewFixBudgetMS < 0 {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "execution.agent.run_budget_ms.review_fix must be >= 0", nil)
	}
	if cfg.Execution.RunBudgetTotalMS <= 0 {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "execution.agent.run_budget_ms.total must be > 0", nil)
	}
	if cfg.Execution.RunBudgetTotalMS < cfg.Execution.RunExecutionBudgetMS+cfg.Execution.RunReviewFixBudgetMS {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "execution.agent.run_budget_ms.total must be >= execution + review_fix", nil)
	}
	if cfg.JobPolicy.DispatchFlow == "" {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "job_policy.dispatch_flow is required", nil)
	}
	for _, jobType := range cfg.JobPolicy.SupportedJobTypes {
		if !jobType.IsValid() {
			return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("job_policy.supported_types contains unsupported job type %q", jobType), nil)
		}
	}
	if cfg.Auth.TransparentForwarding {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "auth.transparent_forwarding must stay false", nil)
	}
	if !cfg.Persistence.BackendKind.IsValid() {
		return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("persistence.backend.kind %q is unsupported", cfg.Persistence.BackendKind), nil)
	}
	if !cfg.Persistence.Usage.IsValid() {
		return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("persistence.backend.usage %q is unsupported", cfg.Persistence.Usage), nil)
	}
	if cfg.Persistence.BackendKind == PersistenceBackendKindFile && cfg.Persistence.Usage == PersistenceUsageProduction {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "persistence.backend.kind file is limited to development/test/single_machine usage", nil)
	}
	if cfg.Persistence.BackendKind == PersistenceBackendKindFile && strings.TrimSpace(cfg.Persistence.FilePath) == "" {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "persistence.file.path is required for file backend", nil)
	}
	if cfg.Persistence.BackendKind == PersistenceBackendKindFile && cfg.Persistence.FlushIntervalMS <= 0 {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "persistence.file.flush_interval_ms must be > 0", nil)
	}
	if cfg.Persistence.AllowPhysicalDelete {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "persistence.retention.allow_physical_delete must stay false", nil)
	}
	if !cfg.Secrets.EnvironmentEnabled {
		if cfg.Source.APIKeyRef.Kind == secret.ReferenceKindEnv {
			return model.NewWorkflowError(model.ErrWorkflowParseError, "secrets.providers.env.enabled must stay true when env references are used", nil)
		}
	}
	providers := map[string]struct{}{}
	for _, provider := range cfg.Secrets.ExternalProviders {
		if provider.Name == "" {
			return model.NewWorkflowError(model.ErrWorkflowParseError, "secrets.providers.external.name is required", nil)
		}
		providers[provider.Name] = struct{}{}
	}
	if cfg.Source.APIKeyRef.Kind == secret.ReferenceKindProvider {
		if _, ok := providers[cfg.Source.APIKeyRef.Provider.Name]; !ok {
			return model.NewWorkflowError(model.ErrWorkflowParseError, "source_adapter.credentials.api_key_ref.provider is not declared in secrets.providers.external", nil)
		}
	}
	return validateNotificationContract(cfg, providers)
}

func validateNotificationContract(cfg WorkflowContract, providers map[string]struct{}) error {
	for index, channel := range cfg.Service.Notifications.Channels {
		switch channel.Kind {
		case model.NotificationChannelKindWebhook:
			if channel.Webhook == nil {
				return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].webhook.url_ref is required", index), nil)
			}
			if err := channel.Webhook.URLRef.Validate(); err != nil {
				return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].webhook.url_ref: %v", index, err), nil)
			}
			if channel.Webhook.URLRef.Kind == secret.ReferenceKindProvider {
				if _, ok := providers[channel.Webhook.URLRef.Provider.Name]; !ok {
					return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].webhook.url_ref.provider is not declared", index), nil)
				}
			}
			for key, ref := range channel.Webhook.HeaderRefs {
				if err := ref.Validate(); err != nil {
					return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].webhook.header_refs.%s: %v", index, key, err), nil)
				}
				if ref.Kind == secret.ReferenceKindProvider {
					if _, ok := providers[ref.Provider.Name]; !ok {
						return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].webhook.header_refs.%s.provider is not declared", index, key), nil)
					}
				}
			}
		case model.NotificationChannelKindSlack:
			if channel.Slack == nil {
				return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].slack.incoming_webhook_url_ref is required", index), nil)
			}
			if err := channel.Slack.IncomingWebhookURLRef.Validate(); err != nil {
				return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].slack.incoming_webhook_url_ref: %v", index, err), nil)
			}
			if channel.Slack.IncomingWebhookURLRef.Kind == secret.ReferenceKindProvider {
				if _, ok := providers[channel.Slack.IncomingWebhookURLRef.Provider.Name]; !ok {
					return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].slack.incoming_webhook_url_ref.provider is not declared", index), nil)
				}
			}
		}
	}
	return nil
}

func buildCapabilityContract(cfg WorkflowContract) contract.CapabilityContract {
	static := []contract.StaticCapability{
		{Name: contract.CapabilitySubmitJob, Category: contract.CapabilityCategoryControl, Summary: "支持异步提交正式 Job。", Supported: true},
		{Name: contract.CapabilityStreamEvents, Category: contract.CapabilityCategoryProtocol, Summary: "支持 HTTP/SSE 正式事件流。", Supported: true},
		{Name: contract.CapabilityQueryObjects, Category: contract.CapabilityCategoryQuery, Summary: "支持正式对象查询。", Supported: true},
		{Name: contract.CapabilityServiceRefresh, Category: contract.CapabilityCategoryControl, Summary: "支持服务级 refresh 控制。", Supported: true},
		{Name: contract.CapabilityCodexExecutor, Category: contract.CapabilityCategoryExecutor, Summary: "执行后端为 Codex。", Supported: cfg.Execution.BackendKind == "codex"},
		{Name: contract.CapabilityFileLedger, Category: contract.CapabilityCategoryStorage, Summary: "文件型对象账本仅用于开发/测试/单机实验。", Supported: cfg.Persistence.BackendKind == PersistenceBackendKindFile},
		{Name: contract.CapabilityRelationalLedger, Category: contract.CapabilityCategoryStorage, Summary: "事务型数据库是生产级官方后端方向。", Supported: cfg.Persistence.BackendKind == PersistenceBackendKindRelational},
		{Name: contract.CapabilityIdentityAuth, Category: contract.CapabilityCategorySecurity, Summary: "认证/授权边界进入正式配置模型。", Supported: true},
		{Name: contract.CapabilityDomainAccessControl, Category: contract.CapabilityCategorySecurity, Summary: "写操作必须遵守 leader 与调度域边界。", Supported: cfg.Auth.LeaderRequired},
		{Name: contract.CapabilityActionAuthorization, Category: contract.CapabilityCategorySecurity, Summary: "平台级 Action 受正式授权边界约束。", Supported: true},
	}
	switch cfg.Source.Kind {
	case "linear":
		static = append(static, contract.StaticCapability{Name: contract.CapabilityLinearSource, Category: contract.CapabilityCategorySource, Summary: "支持 Linear 来源适配器。", Supported: true})
	case "direct":
		static = append(static, contract.StaticCapability{Name: contract.CapabilityDirectJobSource, Category: contract.CapabilityCategorySource, Summary: "支持 direct job 来源。", Supported: true})
	}
	available := make([]contract.AvailableCapability, 0, len(static))
	for _, item := range static {
		available = append(available, contract.AvailableCapability{
			Name:      item.Name,
			Category:  item.Category,
			Summary:   item.Summary,
			Available: item.Supported,
		})
	}
	return contract.CapabilityContract{
		Static:    contract.StaticCapabilitySet{Capabilities: static},
		Available: contract.AvailableCapabilitySet{Capabilities: available},
	}
}

func defaultStringSlice(values []string, fallback []string) []string {
	if len(values) == 0 {
		return append([]string(nil), fallback...)
	}
	return values
}
