// Package executioncompat owns pure, dependency-light compatibility types for
// inspecting and materializing historical durable execution identity.
package executioncompat

const InspectionReportSchemaVersion = 1

// Feature identifies one historical execution-identity surface. A subject can
// report more than one feature while retaining exactly one classification.
type Feature string

const (
	FeatureLocalAlias            Feature = "local_alias"
	FeatureProviderShadowFields  Feature = "provider_shadow_fields"
	FeatureLegacyProviderBinding Feature = "legacy_provider_binding"
	FeatureProviderSessionEvent  Feature = "provider_session_event"
	FeatureLegacyReceiptProvider Feature = "legacy_receipt_provider"
	FeatureLegacyPolicyRoute     Feature = "legacy_policy_route"
	FeatureLegacyResumeMigration Feature = "legacy_resume_migration"
)

// InspectionReport is the stable, content-free result of a workspace
// compatibility inventory. It intentionally contains no model, provider URL,
// credential, prompt, output, or workspace path.
type InspectionReport struct {
	SchemaVersion                 int       `json:"schema_version"`
	Scope                         string    `json:"scope"`
	LegacyLocalAliasEvents        int       `json:"legacy_local_alias_events"`
	LegacyProviderShadowEvents    int       `json:"legacy_provider_shadow_events"`
	LegacyExecutionEventProviders int       `json:"legacy_execution_event_providers"`
	LegacyProviderBindings        int       `json:"legacy_provider_bindings"`
	LegacyProviderSessionEvents   int       `json:"legacy_provider_session_events"`
	LegacyReceiptProviders        int       `json:"legacy_receipt_providers"`
	LegacyPolicyRoutes            int       `json:"legacy_policy_routes"`
	CanonicalTasks                int       `json:"canonical_tasks"`
	MigratedTasks                 int       `json:"migrated_tasks"`
	MigratableTasks               int       `json:"migratable_tasks"`
	AmbiguousTasks                int       `json:"ambiguous_tasks"`
	UnmigratableTasks             int       `json:"unmigratable_tasks"`
	CanonicalPolicySnapshots      int       `json:"canonical_policy_snapshots"`
	MigratedPolicySnapshots       int       `json:"migrated_policy_snapshots"`
	MigratablePolicySnapshots     int       `json:"migratable_policy_snapshots"`
	AmbiguousPolicySnapshots      int       `json:"ambiguous_policy_snapshots"`
	UnmigratablePolicySnapshots   int       `json:"unmigratable_policy_snapshots"`
	Findings                      []Finding `json:"findings"`
}

// Finding is a deterministic, content-free compatibility subject result.
type Finding struct {
	SubjectKind      string         `json:"subject_kind"`
	BranchID         string         `json:"branch_id"`
	TaskID           string         `json:"task_id,omitempty"`
	RunID            string         `json:"run_id,omitempty"`
	SourceEventID    string         `json:"source_event_id,omitempty"`
	Classification   Classification `json:"classification"`
	Features         []Feature      `json:"features"`
	ReasonCode       string         `json:"reason_code,omitempty"`
	EvidenceEventIDs []string       `json:"evidence_event_ids,omitempty"`
}

// Target is a dependency-light durable execution target wire value.
type Target struct {
	Backend string `json:"backend,omitempty"`
	Model   string `json:"model,omitempty"`
}

// Receipt is the compatibility-relevant subset of a durable execution
// receipt. It is intentionally separate from team.ExecutionReceipt.
type Receipt struct {
	Backend          string `json:"backend,omitempty"`
	SubagentProvider string `json:"subagent_provider,omitempty"`
}

// TaskInput is all immutable execution identity evidence for one occurrence.
// It contains raw workspace evidence only; it never consults current config.
type TaskInput struct {
	Target           Target
	Topology         []Target
	Model            string
	SubagentProvider string
	ProviderBinding  string
	BackendBinding   string
	Receipts         []Receipt
}

// PolicyRoute is the compatibility-relevant subset of a policy route.
type PolicyRoute struct {
	Model          string `json:"model"`
	Backend        string `json:"backend"`
	ProviderKey    string `json:"provider_key,omitempty"`
	LegacyProvider string `json:"legacy_provider,omitempty"`
}

// PolicyInput is raw durable evidence for one execution-policy snapshot.
type PolicyInput struct {
	Version int           `json:"version"`
	Routes  []PolicyRoute `json:"model_routes"`
}

// Classification is the deterministic compatibility state of one task or
// policy subject. Feature counts may overlap; classifications may not.
type Classification string

const (
	ClassificationCanonical     Classification = "canonical"
	ClassificationMigrated      Classification = "migrated"
	ClassificationMigratable    Classification = "migratable"
	ClassificationAmbiguous     Classification = "ambiguous"
	ClassificationUnmigratable  Classification = "unmigratable"
	ClassificationNotApplicable Classification = "not_applicable"
)
