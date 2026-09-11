package team

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/kjelly/hufu/internal/execution"
)

// CompareCanonicalProjection compares the replay-relevant session surface
// without volatile timestamps, random IDs, or compatibility-only fields. It
// is deliberately a shadow check: EventStore remains safe to adopt gradually
// while a mismatch is made durable as a recovery-required condition.
func CompareCanonicalProjection(live *SessionData, events []RunEvent) error {
	if live == nil {
		return fmt.Errorf("live session projection is nil")
	}
	if _, err := executionPolicySnapshotFromEvents(events); err != nil {
		return err
	}
	replayed := ReduceToSessionData(events)
	if err := compareExecutionPolicySnapshotProjection(live.ExecutionPolicySnapshot, replayed.ExecutionPolicySnapshot); err != nil {
		return err
	}
	if !sameConversationProjection(live.Entries, replayed.Entries) {
		return fmt.Errorf("conversation entries differ")
	}
	if err := compareTaskProjection(live.Tasks, replayed.Tasks); err != nil {
		return err
	}
	if !reflect.DeepEqual(live.CriterionResults, replayed.CriterionResults) || !reflect.DeepEqual(live.CriterionCheckpoints, replayed.CriterionCheckpoints) || live.LastCriterionProgressAt != replayed.LastCriterionProgressAt {
		return fmt.Errorf("criterion projection differs")
	}
	return nil
}

// executionPolicySnapshotFromEvents returns the single immutable policy
// snapshot visible in an event lineage. A current-schema lineage may not
// change its policy after it has been admitted: accepting the last value would
// make a later append silently rewrite the scheduling and execution boundary
// for tasks already recorded in the same lineage.
func executionPolicySnapshotFromEvents(events []RunEvent) (*ExecutionPolicySnapshot, error) {
	var admitted *ExecutionPolicySnapshot
	for _, event := range events {
		if event.SchemaVersion < eventStoreSchemaVersion || EventType(event.Type) != EventExecutionPolicySnapshot {
			continue
		}
		var candidate ExecutionPolicySnapshot
		if err := json.Unmarshal(event.Payload, &candidate); err != nil {
			return nil, fmt.Errorf("execution policy snapshot event %q is invalid: %w", event.ID, err)
		}
		if err := validateExecutionPolicySnapshot(&candidate); err != nil {
			return nil, fmt.Errorf("execution policy snapshot event %q is invalid: %w", event.ID, err)
		}
		if admitted != nil && admitted.ConfigurationHash != candidate.ConfigurationHash {
			return nil, fmt.Errorf("execution policy snapshot events disagree: %s != %s", admitted.ConfigurationHash, candidate.ConfigurationHash)
		}
		admitted = cloneExecutionPolicySnapshot(&candidate)
	}
	return admitted, nil
}

func compareExecutionPolicySnapshotProjection(live, replayed *ExecutionPolicySnapshot) error {
	if live == nil && replayed == nil {
		return nil
	}
	if live == nil {
		return fmt.Errorf("execution policy snapshot missing from checkpoint")
	}
	if replayed == nil {
		return fmt.Errorf("execution policy snapshot missing from event store")
	}
	if err := validateExecutionPolicySnapshot(live); err != nil {
		return fmt.Errorf("checkpoint execution policy snapshot is invalid: %w", err)
	}
	if err := validateExecutionPolicySnapshot(replayed); err != nil {
		return fmt.Errorf("event-store execution policy snapshot is invalid: %w", err)
	}
	if live.ConfigurationHash != replayed.ConfigurationHash {
		return fmt.Errorf("execution policy snapshot differs: checkpoint=%s event_store=%s", live.ConfigurationHash, replayed.ConfigurationHash)
	}
	return nil
}

func sameConversationProjection(left, right []SessionEntry) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Role != right[i].Role || left[i].Content != right[i].Content {
			return false
		}
	}
	return true
}

func compareTaskProjection(left, right []*TodoItem) error {
	if len(left) != len(right) {
		return fmt.Errorf("task count differs: checkpoint=%d event-store=%d", len(left), len(right))
	}
	for i := range left {
		if err := compareSingleTaskProjection(left[i], right[i]); err != nil {
			return fmt.Errorf("task %d mismatch: %w", i, err)
		}
	}
	return nil
}

type canonicalVerifyResult struct {
	Command        string            `json:"command,omitempty"`
	WorkDir        string            `json:"work_dir,omitempty"`
	ExitCode       int               `json:"exit_code"`
	Stdout         string            `json:"stdout,omitempty"`
	Stderr         string            `json:"stderr,omitempty"`
	TimedOut       bool              `json:"timed_out,omitempty"`
	WeakWarning    bool              `json:"weak_warning,omitempty"`
	WeakReason     string            `json:"weak_reason,omitempty"`
	Overturned     bool              `json:"overturned,omitempty"`
	OverturnReason string            `json:"overturn_reason,omitempty"`
	Fingerprint    string            `json:"fingerprint,omitempty"`
	Spec           *VerificationSpec `json:"spec,omitempty"`
}

func toCanonicalVerifyResult(vr *VerificationResult) *canonicalVerifyResult {
	if vr == nil {
		return nil
	}
	return &canonicalVerifyResult{
		Command:        vr.Command,
		WorkDir:        vr.WorkDir,
		ExitCode:       vr.ExitCode,
		Stdout:         vr.Stdout,
		Stderr:         vr.Stderr,
		TimedOut:       vr.TimedOut,
		WeakWarning:    vr.WeakWarning,
		WeakReason:     vr.WeakReason,
		Overturned:     vr.Overturned,
		OverturnReason: vr.OverturnReason,
		Fingerprint:    vr.Fingerprint,
		Spec:           cloneVerificationSpecPtr(vr.Spec),
	}
}

type canonicalReceipt struct {
	RunID            string                     `json:"run_id,omitempty"`
	TaskID           string                     `json:"task_id,omitempty"`
	Attempt          int                        `json:"attempt"`
	ExitCode         *int                       `json:"exit_code,omitempty"`
	ProducerID       string                     `json:"producer_id,omitempty"`
	TranscriptRef    string                     `json:"transcript_ref,omitempty"`
	SubmittedResult  *TaskResult                `json:"submitted_result,omitempty"`
	RepairProvenance *RepairProvenance          `json:"repair_provenance,omitempty"`
	VerifyResult     *canonicalVerifyResult     `json:"verify_result,omitempty"`
	StepBudget       *StepBudgetUsage           `json:"step_budget,omitempty"`
	ToolDispositions []ToolExecutionDisposition `json:"tool_dispositions,omitempty"`
	HandoffState     ResultHandoffState         `json:"handoff_state,omitempty"`
	MemoryManifest   *MemoryInjectionManifest   `json:"memory_manifest,omitempty"`
	ContextManifest  *ContextInjectionManifest  `json:"context_manifest,omitempty"`
}

func toCanonicalReceipts(receipts []ExecutionReceipt, single *ExecutionReceipt) []canonicalReceipt {
	var merged []ExecutionReceipt
	if len(receipts) > 0 {
		merged = append(merged, receipts...)
	}
	if single != nil {
		merged = appendExecutionReceipt(merged, *single)
	}
	if len(merged) == 0 {
		return nil
	}
	out := make([]canonicalReceipt, 0, len(merged))
	for _, r := range merged {
		out = append(out, canonicalReceipt{
			RunID:            r.RunID,
			TaskID:           r.TaskID,
			Attempt:          r.Attempt,
			ExitCode:         r.ExitCode,
			ProducerID:       r.ProducerID,
			TranscriptRef:    r.TranscriptRef,
			SubmittedResult:  r.SubmittedResult,
			RepairProvenance: r.RepairProvenance,
			VerifyResult:     toCanonicalVerifyResult(r.VerifyResult),
			StepBudget:       r.StepBudget,
			ToolDispositions: append([]ToolExecutionDisposition(nil), r.ToolDispositions...),
			HandoffState:     r.HandoffState,
			MemoryManifest:   r.MemoryManifest,
			ContextManifest:  r.ContextManifest,
		})
	}
	return out
}

func normalizeStringSlice(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return append([]string(nil), s...)
}

func normalizeFingerprints(fps []FailureFingerprint) []FailureFingerprint {
	if len(fps) == 0 {
		return nil
	}
	return append([]FailureFingerprint(nil), fps...)
}

func normalizeMemoryManifests(mms []MemoryInjectionManifest) []MemoryInjectionManifest {
	if len(mms) == 0 {
		return nil
	}
	return append([]MemoryInjectionManifest(nil), mms...)
}

func normalizeContextManifests(manifests []ContextInjectionManifest) []ContextInjectionManifest {
	if len(manifests) == 0 {
		return nil
	}
	return append([]ContextInjectionManifest(nil), manifests...)
}

type canonicalTaskShadow struct {
	ID                  string                      `json:"id"`
	Phase               string                      `json:"phase,omitempty"`
	Action              *Action                     `json:"action,omitempty"`
	PlanTaskID          string                      `json:"plan_task_id,omitempty"`
	PlanFirst           bool                        `json:"plan_first,omitzero"`
	PlanID              string                      `json:"plan_id,omitempty"`
	ContractID          string                      `json:"contract_id,omitempty"`
	ContractHash        string                      `json:"contract_hash,omitempty"`
	ContractRevision    int                         `json:"contract_revision,omitempty"`
	Agent               string                      `json:"agent"`
	Desc                string                      `json:"desc"`
	Goal                string                      `json:"goal,omitempty"`
	Constraints         string                      `json:"constraints,omitempty"`
	Status              string                      `json:"status"`
	Detail              string                      `json:"detail,omitempty"`
	Output              string                      `json:"output,omitempty"`
	Model               string                      `json:"model,omitempty"`
	ModelTopology       []string                    `json:"model_topology,omitempty"`
	ExecutionTarget     execution.ExecutionTarget   `json:"execution_target,omitzero"`
	ExecutionTopology   []execution.ExecutionTarget `json:"execution_topology,omitempty"`
	Sidecar             bool                        `json:"sidecar,omitempty"`
	Summarize           bool                        `json:"summarize,omitempty"`
	OutputMode          string                      `json:"output_mode,omitempty"`
	ContextFiles        []string                    `json:"context_files,omitempty"`
	Requires            []string                    `json:"requires,omitempty"`
	Skills              []string                    `json:"skills,omitempty"`
	InjectedSkills      []string                    `json:"injected_skills,omitempty"`
	LoadedSkills        []string                    `json:"loaded_skills,omitempty"`
	Source              string                      `json:"source,omitempty"`
	ParentID            string                      `json:"parent_id,omitempty"`
	DependsOn           []string                    `json:"depends_on,omitempty"`
	MaxRetries          int                         `json:"max_retries,omitempty"`
	Retries             int                         `json:"retries,omitempty"`
	OnFailure           string                      `json:"on_failure,omitempty"`
	Escalate            bool                        `json:"escalate,omitempty"`
	AdversarialVerify   int                         `json:"adversarial_verify,omitempty"`
	Verify              string                      `json:"verify,omitempty"`
	VerifyMode          string                      `json:"verify_mode,omitempty"`
	VerifySpec          *VerificationSpec           `json:"verify_spec,omitempty"`
	VerifyResult        *canonicalVerifyResult      `json:"verify_result,omitempty"`
	WorksetBinding      *WorksetBinding             `json:"workset_binding,omitempty"`
	WorksetReceipt      *WorksetExpansionReceipt    `json:"workset_receipt,omitempty"`
	ExecutionReceipts   []canonicalReceipt          `json:"execution_receipts,omitempty"`
	FailureEvent        *FailureEventPayload        `json:"failure_event,omitempty"`
	FailureFingerprints []FailureFingerprint        `json:"failure_fingerprints,omitempty"`
	SideEffect          string                      `json:"side_effect,omitempty"`
	Recovery            string                      `json:"recovery,omitempty"`
	ReconcileTool       string                      `json:"reconcile_tool,omitempty"`
	RecoveryHypothesis  *RecoveryHypothesis         `json:"recovery_hypothesis,omitempty"`
	RecoveryState       string                      `json:"recovery_state,omitempty"`
	TypedResult         *TaskResult                 `json:"typed_result,omitempty"`
	Resolution          *TaskResolution             `json:"resolution,omitempty"`
	Kind                string                      `json:"kind,omitempty"`
	Advances            []string                    `json:"advances,omitempty"`
	ExpectedStateChange string                      `json:"expected_state_change,omitempty"`
	Progress            string                      `json:"progress,omitempty"`
	ProgressCriteria    []string                    `json:"progress_criteria,omitempty"`
	Execution           ExecutionContract           `json:"execution,omitempty"`
	Optional            bool                        `json:"optional,omitempty"`
	ResourceClaims      []string                    `json:"resource_claims,omitempty"`
	Resources           []ResourceClaim             `json:"resources,omitempty"`
	DecisionProfile     string                      `json:"decision_profile,omitempty"`
	DecisionOptions     []DecisionOption            `json:"decision_options,omitempty"`
	DecisionAssumptions []DecisionAssumption        `json:"decision_assumptions,omitempty"`
	DecisionFacts       map[string]any              `json:"decision_facts,omitempty"`
	DecisionArtifacts   []ArtifactRef               `json:"decision_artifacts,omitempty"`
	DecisionBaseRates   []BaseRateEvidence          `json:"decision_base_rates,omitempty"`
	DecisionProvenance  []EvidenceProvenance        `json:"decision_provenance,omitempty"`
	MemoryManifests     []MemoryInjectionManifest   `json:"memory_manifests,omitempty"`
	ContextManifests    []ContextInjectionManifest  `json:"context_manifests,omitempty"`
	SubagentProvider    string                      `json:"subagent_provider,omitempty"`
	ProviderBinding     *ProviderBinding            `json:"provider_binding,omitempty"`
	BackendBinding      *BackendBinding             `json:"backend_binding,omitempty"`
}

func toCanonicalTaskShadow(item *TodoItem) canonicalTaskShadow {
	if item == nil {
		return canonicalTaskShadow{}
	}
	// A fully admitted target is the canonical worker identity. The legacy
	// provider marker remains readable on live/history projections for
	// compatibility, but canonical task-transition events intentionally omit it
	// so it must not make an otherwise identical checkpoint look divergent.
	subagentProvider := strings.TrimSpace(item.SubagentProvider)
	model := strings.TrimSpace(item.Model)
	modelTopology := normalizeStringSlice(item.ModelTopology)
	providerBinding := cloneProviderBinding(item.ProviderBinding)
	if !item.ExecutionTarget.IsZero() {
		subagentProvider = ""
		model = ""
		modelTopology = nil
		providerBinding = nil
	}
	return canonicalTaskShadow{
		ID:                  item.ID,
		Phase:               string(item.Phase),
		Action:              cloneActionPtr(item.Action),
		PlanTaskID:          item.PlanTaskID,
		PlanFirst:           item.PlanFirst,
		PlanID:              item.PlanID,
		ContractID:          item.ContractID,
		ContractHash:        item.ContractHash,
		ContractRevision:    item.ContractRevision,
		Agent:               strings.ToLower(strings.TrimSpace(item.Agent)),
		Desc:                item.Desc,
		Goal:                item.Goal,
		Constraints:         item.Constraints,
		Status:              string(item.Status),
		Detail:              item.Detail,
		Output:              item.Output,
		Model:               model,
		ModelTopology:       modelTopology,
		ExecutionTarget:     item.ExecutionTarget,
		ExecutionTopology:   cloneExecutionTopology(item.ExecutionTopology),
		Sidecar:             item.Sidecar,
		Summarize:           item.Summarize,
		OutputMode:          item.OutputMode,
		ContextFiles:        normalizeStringSlice(item.ContextFiles),
		Requires:            normalizeStringSlice(item.Requires),
		Skills:              normalizeStringSlice(item.Skills),
		InjectedSkills:      normalizeStringSlice(item.InjectedSkills),
		LoadedSkills:        normalizeStringSlice(item.LoadedSkills),
		Source:              item.Source,
		ParentID:            item.ParentID,
		DependsOn:           normalizeStringSlice(item.DependsOn),
		MaxRetries:          item.MaxRetries,
		Retries:             item.Retries,
		OnFailure:           item.OnFailure,
		Escalate:            item.Escalate,
		AdversarialVerify:   item.AdversarialVerify,
		Verify:              item.Verify,
		VerifyMode:          item.VerifyMode,
		VerifySpec:          cloneVerificationSpecPtr(item.VerifySpec),
		VerifyResult:        toCanonicalVerifyResult(item.VerifyResult),
		WorksetBinding:      cloneWorksetBinding(item.WorksetBinding),
		WorksetReceipt:      cloneWorksetReceipt(item.WorksetReceipt),
		ExecutionReceipts:   toCanonicalReceipts(item.ExecutionReceipts, item.ExecutionReceipt),
		FailureEvent:        RedactedFailureEvent(item.FailureEvent),
		FailureFingerprints: normalizeFingerprints(item.FailureFingerprints),
		SideEffect:          string(item.SideEffect),
		Recovery:            string(item.Recovery),
		ReconcileTool:       item.ReconcileTool,
		RecoveryHypothesis:  cloneRecoveryHypothesis(item.RecoveryHypothesis),
		RecoveryState:       item.RecoveryState,
		TypedResult:         item.TypedResult,
		Resolution:          item.Resolution,
		Kind:                string(item.Kind),
		Advances:            normalizeStringSlice(item.Advances),
		ExpectedStateChange: item.ExpectedStateChange,
		Progress:            string(item.Progress),
		ProgressCriteria:    normalizeStringSlice(item.ProgressCriteria),
		Execution:           cloneExecutionContract(item.Execution),
		Optional:            item.Optional,
		ResourceClaims:      normalizeStringSlice(item.ResourceClaims),
		Resources:           append([]ResourceClaim(nil), item.Resources...),
		DecisionProfile:     item.DecisionProfile,
		DecisionOptions:     append([]DecisionOption(nil), item.DecisionOptions...),
		DecisionAssumptions: cloneDecisionAssumptions(item.DecisionAssumptions),
		DecisionFacts:       cloneDecisionFacts(item.DecisionFacts),
		DecisionArtifacts:   append([]ArtifactRef(nil), item.DecisionArtifacts...),
		DecisionBaseRates:   cloneBaseRateEvidence(item.DecisionBaseRates),
		DecisionProvenance:  cloneEvidenceProvenance(item.DecisionProvenance),
		MemoryManifests:     normalizeMemoryManifests(item.MemoryManifests),
		ContextManifests:    normalizeContextManifests(item.ContextManifests),
		SubagentProvider:    subagentProvider,
		ProviderBinding:     providerBinding,
		BackendBinding:      cloneBackendBinding(item.BackendBinding),
	}
}

func compareSingleTaskProjection(left, right *TodoItem) error {
	if left == nil || right == nil {
		if left != right {
			return fmt.Errorf("nil mismatch")
		}
		return nil
	}
	lShadow := toCanonicalTaskShadow(left)
	rShadow := toCanonicalTaskShadow(right)
	lJSON, err := json.Marshal(lShadow)
	if err != nil {
		return fmt.Errorf("marshal canonical checkpoint task %s: %w", left.ID, err)
	}
	rJSON, err := json.Marshal(rShadow)
	if err != nil {
		return fmt.Errorf("marshal canonical event-store task %s: %w", right.ID, err)
	}
	if !bytes.Equal(lJSON, rJSON) {
		return fmt.Errorf("task %s canonical parity mismatch: checkpoint=%s vs event_store=%s", left.ID, string(lJSON), string(rJSON))
	}
	return nil
}

func hasCurrentCanonicalProjectionEvents(events []RunEvent) bool {
	for _, event := range events {
		if event.SchemaVersion < eventStoreSchemaVersion {
			continue
		}
		switch EventType(event.Type) {
		case EventExecutionPolicySnapshot, EventUserMessageAdded, EventAssistantMessageAdded, EventTaskCreated, EventTaskStarted, EventTaskVerifying, EventTaskCompleted, EventTaskFailed, EventTaskBlocked, EventTaskSkipped, EventTaskProtocolIncomplete:
			return true
		}
	}
	return false
}
