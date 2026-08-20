package team

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// CompareCanonicalProjection compares the replay-relevant session surface
// without volatile timestamps, random IDs, or compatibility-only fields. It
// is deliberately a shadow check: EventStore remains safe to adopt gradually
// while a mismatch is made durable as a recovery-required condition.
func CompareCanonicalProjection(live *SessionData, events []RunEvent) error {
	if live == nil {
		return fmt.Errorf("live session projection is nil")
	}
	replayed := ReduceToSessionData(events)
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
	ID                  string                     `json:"id"`
	Phase               string                     `json:"phase,omitempty"`
	PlanTaskID          string                     `json:"plan_task_id,omitempty"`
	ContractID          string                     `json:"contract_id,omitempty"`
	ContractHash        string                     `json:"contract_hash,omitempty"`
	ContractRevision    int                        `json:"contract_revision,omitempty"`
	Agent               string                     `json:"agent"`
	Desc                string                     `json:"desc"`
	Status              string                     `json:"status"`
	Detail              string                     `json:"detail,omitempty"`
	Output              string                     `json:"output,omitempty"`
	Model               string                     `json:"model,omitempty"`
	Skills              []string                   `json:"skills,omitempty"`
	InjectedSkills      []string                   `json:"injected_skills,omitempty"`
	LoadedSkills        []string                   `json:"loaded_skills,omitempty"`
	Source              string                     `json:"source,omitempty"`
	ParentID            string                     `json:"parent_id,omitempty"`
	DependsOn           []string                   `json:"depends_on,omitempty"`
	MaxRetries          int                        `json:"max_retries,omitempty"`
	Retries             int                        `json:"retries,omitempty"`
	OnFailure           string                     `json:"on_failure,omitempty"`
	Verify              string                     `json:"verify,omitempty"`
	VerifyMode          string                     `json:"verify_mode,omitempty"`
	VerifySpec          *VerificationSpec          `json:"verify_spec,omitempty"`
	VerifyResult        *canonicalVerifyResult     `json:"verify_result,omitempty"`
	ExecutionReceipts   []canonicalReceipt         `json:"execution_receipts,omitempty"`
	FailureEvent        *FailureEventPayload       `json:"failure_event,omitempty"`
	FailureFingerprints []FailureFingerprint       `json:"failure_fingerprints,omitempty"`
	SideEffect          string                     `json:"side_effect,omitempty"`
	Recovery            string                     `json:"recovery,omitempty"`
	ReconcileTool       string                     `json:"reconcile_tool,omitempty"`
	RecoveryState       string                     `json:"recovery_state,omitempty"`
	TypedResult         *TaskResult                `json:"typed_result,omitempty"`
	Resolution          *TaskResolution            `json:"resolution,omitempty"`
	Kind                string                     `json:"kind,omitempty"`
	Advances            []string                   `json:"advances,omitempty"`
	ExpectedStateChange string                     `json:"expected_state_change,omitempty"`
	Progress            string                     `json:"progress,omitempty"`
	ProgressCriteria    []string                   `json:"progress_criteria,omitempty"`
	Execution           ExecutionContract          `json:"execution,omitempty"`
	MemoryManifests     []MemoryInjectionManifest  `json:"memory_manifests,omitempty"`
	ContextManifests    []ContextInjectionManifest `json:"context_manifests,omitempty"`
}

func toCanonicalTaskShadow(item *TodoItem) canonicalTaskShadow {
	if item == nil {
		return canonicalTaskShadow{}
	}
	return canonicalTaskShadow{
		ID:                  item.ID,
		Phase:               string(item.Phase),
		PlanTaskID:          item.PlanTaskID,
		ContractID:          item.ContractID,
		ContractHash:        item.ContractHash,
		ContractRevision:    item.ContractRevision,
		Agent:               strings.ToLower(strings.TrimSpace(item.Agent)),
		Desc:                item.Desc,
		Status:              string(item.Status),
		Detail:              item.Detail,
		Output:              item.Output,
		Model:               strings.TrimSpace(item.Model),
		Skills:              normalizeStringSlice(item.Skills),
		InjectedSkills:      normalizeStringSlice(item.InjectedSkills),
		LoadedSkills:        normalizeStringSlice(item.LoadedSkills),
		Source:              item.Source,
		ParentID:            item.ParentID,
		DependsOn:           normalizeStringSlice(item.DependsOn),
		MaxRetries:          item.MaxRetries,
		Retries:             item.Retries,
		OnFailure:           item.OnFailure,
		Verify:              item.Verify,
		VerifyMode:          item.VerifyMode,
		VerifySpec:          cloneVerificationSpecPtr(item.VerifySpec),
		VerifyResult:        toCanonicalVerifyResult(item.VerifyResult),
		ExecutionReceipts:   toCanonicalReceipts(item.ExecutionReceipts, item.ExecutionReceipt),
		FailureEvent:        RedactedFailureEvent(item.FailureEvent),
		FailureFingerprints: normalizeFingerprints(item.FailureFingerprints),
		SideEffect:          string(item.SideEffect),
		Recovery:            string(item.Recovery),
		ReconcileTool:       item.ReconcileTool,
		RecoveryState:       item.RecoveryState,
		TypedResult:         item.TypedResult,
		Resolution:          item.Resolution,
		Kind:                string(item.Kind),
		Advances:            normalizeStringSlice(item.Advances),
		ExpectedStateChange: item.ExpectedStateChange,
		Progress:            string(item.Progress),
		ProgressCriteria:    normalizeStringSlice(item.ProgressCriteria),
		Execution:           item.Execution,
		MemoryManifests:     normalizeMemoryManifests(item.MemoryManifests),
		ContextManifests:    normalizeContextManifests(item.ContextManifests),
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
		case EventUserMessageAdded, EventAssistantMessageAdded, EventTaskCreated, EventTaskStarted, EventTaskVerifying, EventTaskCompleted, EventTaskFailed, EventTaskBlocked, EventTaskSkipped, EventTaskProtocolIncomplete:
			return true
		}
	}
	return false
}
