package team

type PrimaryEvidenceTaskSourceV1 struct {
	TaskID                 string `json:"task_id"`
	OccurrenceRevision     uint32 `json:"occurrence_revision"`
	Attempt                uint32 `json:"attempt"`
	ProducerExecutionRunID string `json:"producer_execution_run_id"`
	ResultEventID          string `json:"result_event_id"`
}

type PrimaryEvidenceSourceV1 struct {
	SourceKind           string                       `json:"source_kind"`
	Task                 *PrimaryEvidenceTaskSourceV1 `json:"task"`
	SourceArtifact       DecisionArtifactRef          `json:"source_artifact"`
	JSONPointer          string                       `json:"json_pointer"`
	SourceEventID        string                       `json:"source_event_id"`
	ProducerAgentID      *string                      `json:"producer_agent_id"`
	ModelIdentity        *string                      `json:"model_identity"`
	ProviderIdentity     *string                      `json:"provider_identity"`
	DerivedParentItemIDs []string                     `json:"derived_parent_item_ids"`
	ProvenanceAuthority  string                       `json:"provenance_authority"`
}

type PrimaryEvidenceVerificationV1 struct {
	State         string                `json:"state"`
	Scope         string                `json:"scope"`
	AssertionRefs []DecisionArtifactRef `json:"assertion_refs"`
}

type PrimaryEvidenceIndependenceV1 struct {
	GroupID             string `json:"group_id"`
	KnownRoot           bool   `json:"known_root"`
	CountsTowardMinimum bool   `json:"counts_toward_minimum"`
}

type PrimaryEvidenceLineRangeV1 struct {
	Start uint64 `json:"start"`
	End   uint64 `json:"end"`
}

type PrimaryEvidenceExtractionV1 struct {
	Algorithm          string                       `json:"algorithm"`
	Truncated          bool                         `json:"truncated"`
	SelectedLineRanges []PrimaryEvidenceLineRangeV1 `json:"selected_line_ranges"`
	SourceBytes        uint64                       `json:"source_bytes"`
	ViewBytes          uint64                       `json:"view_bytes"`
	RemovedLineCount   uint64                       `json:"removed_line_count"`
}

type PrimaryEvidenceItemV1 struct {
	ItemID           string                        `json:"item_id"`
	Source           PrimaryEvidenceSourceV1       `json:"source"`
	ViewRef          DecisionArtifactRef           `json:"view_ref"`
	ContentFormat    string                        `json:"content_format"`
	Content          string                        `json:"content"`
	Verification     PrimaryEvidenceVerificationV1 `json:"verification"`
	MandatoryReasons []string                      `json:"mandatory_reasons"`
	EpistemicStatus  string                        `json:"epistemic_status"`
	Independence     PrimaryEvidenceIndependenceV1 `json:"independence"`
	Extraction       PrimaryEvidenceExtractionV1   `json:"extraction"`
}

type PrimaryEvidenceScopeV1 struct {
	ExecutionRunIDs        []string              `json:"execution_run_ids"`
	ImportedEvidenceRefs   []DecisionArtifactRef `json:"imported_evidence_refs"`
	ExcludesPrimaryLineage bool                  `json:"excludes_primary_lineage"`
}

type PrimaryEvidenceExcludedReasonCountsV1 struct {
	Duplicate        uint64 `json:"duplicate"`
	OutOfScope       uint64 `json:"out_of_scope"`
	Superseded       uint64 `json:"superseded"`
	RedactedUnusable uint64 `json:"redacted_unusable"`
	OptionalOversize uint64 `json:"optional_oversize"`
	Budget           uint64 `json:"budget"`
	UnsupportedMedia uint64 `json:"unsupported_media"`
}

type PrimaryEvidenceCoverageV1 struct {
	InventoryRef              DecisionArtifactRef                   `json:"inventory_ref"`
	InventoryCount            uint64                                `json:"inventory_count"`
	SelectedCount             uint64                                `json:"selected_count"`
	ExcludedCount             uint64                                `json:"excluded_count"`
	ExcludedReasonCounts      PrimaryEvidenceExcludedReasonCountsV1 `json:"excluded_reason_counts"`
	MandatorySelectedCount    uint64                                `json:"mandatory_selected_count"`
	KnownIndependentGroups    uint64                                `json:"known_independent_groups"`
	UnresolvedProvenanceCount uint64                                `json:"unresolved_provenance_count"`
}

type PrimaryEvidenceBudgetV1 struct {
	UsedViewBytes         uint64  `json:"used_view_bytes"`
	UsedTokens            uint64  `json:"used_tokens"`
	ReservedFramingTokens uint64  `json:"reserved_framing_tokens"`
	CountMethod           string  `json:"count_method"`
	TokenizerID           *string `json:"tokenizer_id"`
	TokenizerRevision     *string `json:"tokenizer_revision"`
	MaxContextInputTokens uint64  `json:"max_context_input_tokens"`
}

type PrimaryDecisionEvidenceV1 struct {
	SchemaVersion         int                       `json:"schema_version"`
	Kind                  string                    `json:"kind"`
	LogicalRunID          string                    `json:"logical_run_id"`
	BranchID              string                    `json:"branch_id"`
	Generation            uint32                    `json:"generation"`
	RequirementDigest     string                    `json:"requirement_digest"`
	SupportCursor         DecisionSupportCursor     `json:"support_cursor"`
	SupportRevisionDigest string                    `json:"support_revision_digest"`
	SourceIndexDigest     string                    `json:"source_index_digest"`
	CollectorVersion      string                    `json:"collector_version"`
	NormalizerVersion     string                    `json:"normalizer_version"`
	RedactionPolicyRef    DecisionArtifactRef       `json:"redaction_policy_ref"`
	LimitsRef             DecisionArtifactRef       `json:"limits_ref"`
	Scope                 PrimaryEvidenceScopeV1    `json:"scope"`
	Items                 []PrimaryEvidenceItemV1   `json:"items"`
	Coverage              PrimaryEvidenceCoverageV1 `json:"coverage"`
	Budget                PrimaryEvidenceBudgetV1   `json:"budget"`
	BaseRateItemIDs       []string                  `json:"base_rate_item_ids"`
	AssumptionItemIDs     []string                  `json:"assumption_item_ids"`
}

type PrimaryEvidenceInventoryEntryV1 struct {
	ItemID           string              `json:"item_id"`
	SourceRef        DecisionArtifactRef `json:"source_ref"`
	SourceEventID    string              `json:"source_event_id"`
	MandatoryReasons []string            `json:"mandatory_reasons"`
	Selection        string              `json:"selection"`
	Reason           string              `json:"reason"`
}

type PrimaryEvidenceCandidate struct {
	Source           PrimaryEvidenceSourceV1
	ContentFormat    string
	Content          string
	Verification     PrimaryEvidenceVerificationV1
	MandatoryReasons []string
	EpistemicStatus  string
	RootIdentities   []string
	SourceEventOrder uint64
	BaseRate         bool
	Assumption       bool
}

type PrimaryEvidenceLimits struct {
	MaxCandidates            uint32
	MaxAggregateSourceBytes  uint64
	MaxItems                 uint32
	MaxViewBytes             uint64
	MaxItemViewBytes         uint64
	MaxContextInputTokens    uint64
	ReservedFramingTokens    uint64
	PromptFramingBytes       uint64
	KnownIndependenceMinimum uint32
	BaseRateMinimum          uint32
}

type PrimaryEvidenceCollectionRequest struct {
	LogicalRunID          string
	BranchID              string
	Generation            uint32
	RequirementDigest     string
	SupportCursor         DecisionSupportCursor
	SupportRevisionDigest string
	RedactionPolicyRef    DecisionArtifactRef
	LimitsRef             DecisionArtifactRef
	ExecutionRunIDs       []string
	ImportedEvidenceRefs  []DecisionArtifactRef
	Candidates            []PrimaryEvidenceCandidate
	Limits                PrimaryEvidenceLimits
	Store                 ArtifactStore
}

// PrimaryEvidenceSourceRecord is a runtime-owned description of one value in
// an immutable artifact. The adapter resolves SourceArtifact through the
// ArtifactStore and derives Content; callers cannot inject unverified bytes.
type PrimaryEvidenceSourceRecord struct {
	SourceKind           string
	Task                 *PrimaryEvidenceTaskSourceV1
	SourceArtifact       ArtifactRef
	JSONPointer          string
	SourceEventID        string
	ProducerAgentID      *string
	ModelIdentity        *string
	ProviderIdentity     *string
	DerivedParentItemIDs []string
	ProvenanceAuthority  string
	Verification         PrimaryEvidenceVerificationV1
	MandatoryReasons     []string
	EpistemicStatus      string
	SourceEventOrder     uint64
	BaseRate             bool
	Assumption           bool
}

// PrimaryEvidencePacketInput contains the trusted decision request fields
// that are deliberately not inferred from collected evidence.
type PrimaryEvidencePacketInput struct {
	ID                 string
	Question           string
	Options            []DecisionOption
	Criteria           []DecisionCriterion
	RequestContractRef string
}
