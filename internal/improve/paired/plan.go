package paired

// ExperimentPlan is committed before the first provider call. Unlike an
// offline preview, every runtime-sensitive input is resolved and artifact
// backed. Raw prompts and results remain outside journal event payloads.
type ExperimentPlan struct {
	Kind                    string                   `json:"kind"`
	SchemaVersion           int                      `json:"schema_version"`
	ExperimentID            string                   `json:"experiment_id"`
	ParentWorkspaceID       string                   `json:"parent_workspace_id"`
	ParentBranchID          string                   `json:"parent_branch_id"`
	Mode                    string                   `json:"mode"`
	ExecutionMode           string                   `json:"execution_mode"`
	Profile                 string                   `json:"profile"`
	BinaryDigest            string                   `json:"binary_digest"`
	SourceRevision          string                   `json:"source_revision,omitempty"`
	Config                  ArtifactRef              `json:"config_ref"`
	Fixture                 ArtifactRef              `json:"fixture_ref"`
	Inputs                  []ArtifactRef            `json:"input_refs"`
	BaselineSnapshot        ArtifactRef              `json:"baseline_snapshot_ref"`
	ComparisonContextDigest string                   `json:"comparison_context_digest"`
	PolicyProjectionVersion int                      `json:"policy_projection_version"`
	Models                  map[string]ResolvedModel `json:"resolved_models"`
	Limits                  Limits                   `json:"limits"`
	Objective               Objective                `json:"objective"`
	Slots                   []PairSlot               `json:"slots"`
}

type ResolvedModel struct {
	Backend      string            `json:"backend"`
	Model        string            `json:"model"`
	Generation   map[string]string `json:"generation"`
	PolicyDigest string            `json:"policy_digest"`
}
