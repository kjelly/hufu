package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"slices"
)

// LegacySubmissionRecovery binds a derived collection to a rejected tool
// submission. It never represents successful worker execution or a repair grant.
// Only contract_recovery_created can introduce this proof into canonical replay.
type LegacySubmissionRecovery struct {
	Version                 int                 `json:"version"`
	SourceBranch            string              `json:"source_branch"`
	SourcePolicy            string              `json:"source_policy"`
	SourceTaskSHA256        string              `json:"source_task_sha256"`
	OriginalTaskInputSHA256 string              `json:"original_task_input_sha256"`
	EvidenceSHA256          string              `json:"evidence_sha256"`
	ReceiptSHA256           string              `json:"receipt_sha256"`
	Candidate               *ResultCandidate    `json:"candidate"`
	ArgumentsSHA256         string              `json:"arguments_sha256"`
	OriginalExecution       ExecutionContract   `json:"original_execution"`
	Execution               ExecutionContract   `json:"execution"`
	AttemptsUsed            int                 `json:"attempts_used"`
	WorkspaceSHA256         string              `json:"workspace_sha256"`
	Work                    ResultCollectionRef `json:"work"`
}

func cloneLegacySubmission(p *LegacySubmissionRecovery) *LegacySubmissionRecovery {
	if p == nil {
		return nil
	}
	out := *p
	out.Candidate = cloneResultCandidate(p.Candidate)
	out.OriginalExecution = cloneExecutionContract(p.OriginalExecution)
	out.Execution = cloneExecutionContract(p.Execution)
	return &out
}

func legacySubmissionTarget(item *TodoItem) bool {
	if item == nil || item.Status != TaskBlocked || item.FailureEvent == nil || item.FailureEvent.FailureClass != FailureExecution {
		return false
	}
	if !legacySubmissionScope(item) {
		return false
	}
	candidate, receipt := item.ResultCandidate, item.ExecutionReceipt
	return item.ResultContract != nil && candidate != nil && !candidate.Prepared && !candidate.Truncated && len(candidate.Issues) > 0 &&
		receipt != nil && receipt.ResultCollection == nil && receipt.RepairProvenance == nil
}

func legacySubmissionScope(item *TodoItem) bool {
	e := item.Execution
	return item.SideEffect == SideEffectNone && !item.WorkerWorkspace.isolated() && item.Action == nil && len(item.ExecutionTopology) <= 1 &&
		item.DependencyResultView == nil && item.HistoricalResultBinding == nil && item.WorksetBinding == nil && item.MediaBinding == nil && item.MediaInputDigest == "" &&
		slices.Equal(e.ToolSequence, []string{submitResultToolName}) && len(e.Steps) == 0 && len(e.ToolInputSequence) == 0 &&
		len(e.ToolInputCanonicalSequence) == 0 && len(e.ToolInputTransformSequence) == 0 && !item.CheckpointPause && item.RecoveryState == "" &&
		(item.DecisionProfile == "" || item.DecisionProfile == DecisionProfileOff)
}

// HasLegacySubmissionCandidate only selects a read-only preview. It does not
// authenticate evidence, authorize a grant or permit any recovery mutation.
func HasLegacySubmissionCandidate(item *TodoItem) bool { return legacySubmissionTarget(item) }

func legacyWorkBinding(p *LegacySubmissionRecovery) (string, error) {
	copy := *p
	copy.Work = ResultCollectionRef{}
	return collectionDigest(copy)
}

func validateLegacySubmissionSource(item *TodoItem, p *LegacySubmissionRecovery, plan *ContractRecoveryPlan) error {
	if !legacySubmissionTarget(item) || p == nil || plan == nil || p.SourceBranch != plan.SourceBranch || p.SourcePolicy != plan.SourcePolicy ||
		p.Version != 1 || p.SourceTaskSHA256 == "" || p.Candidate == nil || p.Candidate.BranchID != p.SourceBranch || !sameCanonicalEvidence(p.Candidate, item.ResultCandidate) || !sameCanonicalEvidence(p.OriginalExecution, item.Execution) {
		return fmt.Errorf("invalid legacy submission source")
	}
	expected := cloneExecutionContract(item.Execution)
	expected.ToolSequence = nil
	receiptHash, _ := collectionDigest(item.ExecutionReceipt)
	taskHash, _ := collectionDigest(item)
	inputHash, _ := decisionTaskInputDigest(item)
	if !sameCanonicalEvidence(expected, p.Execution) || p.SourceTaskSHA256 != taskHash || p.OriginalTaskInputSHA256 != inputHash || p.ReceiptSHA256 != receiptHash ||
		!validCollectionDigest(p.EvidenceSHA256) || !validCollectionDigest(p.ArgumentsSHA256) || !validCollectionDigest(p.WorkspaceSHA256) || !validCollectionRef(&p.Work) ||
		item.ExecutionReceipt.StepBudget == nil || p.AttemptsUsed != item.ExecutionReceipt.StepBudget.Used || p.AttemptsUsed < 1 {
		return fmt.Errorf("legacy submission binding changed")
	}
	return nil
}

func (c *Coordinator) assessLegacySubmission(ctx context.Context, source *ResumeSource, item *TodoItem, plan *ContractRecoveryPlan) (*LegacySubmissionRecovery, error) {
	store, err := OpenEventStoreReadOnly(c.session.Workspace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()
	restore := c.installResumeValidationSource(source, store)
	defer restore()
	if !legacySubmissionTarget(item) {
		return nil, fmt.Errorf("legacy recovery requires a rejected submission-only read-only task")
	}
	r := item.ExecutionReceipt
	if err := validateLegacySubmissionReceipt(item); err != nil {
		return nil, err
	}
	snapshot := contractRecoveryInputSnapshot(item, source.Session)
	if snapshot == nil || ValidateRunInputSnapshot(snapshot) != nil || snapshot.SnapshotHash != item.RunInputSnapshotHash {
		return nil, fmt.Errorf("legacy recovery frozen inputs are unavailable")
	}
	if _, active := c.activeTaskResultOccurrence(item.ID); active {
		return nil, errTaskResultStale
	}
	if err := validateRecoveryTerminalContext(c.session.Workspace, &ResultCollectionCheckpoint{TaskID: item.ID, SourceRunID: r.RunID}); err != nil {
		return nil, err
	}
	if err := c.validateResumeTranscript(ctx, r); err != nil {
		return nil, err
	}
	transcript, err := c.restoreTranscriptEvidence(ctx, r, r.RunID)
	if err != nil {
		return nil, err
	}
	value, err := c.loadLegacyCandidate(ctx, item, item.ResultCandidate, source.BranchID)
	if err != nil {
		return nil, err
	}
	for field := range value {
		if field != "status" && field != "summary" && field != "structured_payload" {
			return nil, fmt.Errorf("unsupported legacy submission field %q", field)
		}
	}
	if payload, ok := value["structured_payload"].(map[string]any); !ok || len(payload) == 0 {
		return nil, fmt.Errorf("legacy recovery requires a complete object candidate")
	}
	status, _ := value["status"].(string)
	if !taskResultStatusIsSuccessful(status) {
		return nil, fmt.Errorf("legacy candidate is not a completion claim")
	}
	args, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	if err := validateLegacySubmissionTranscript(item, transcript, args); err != nil {
		return nil, err
	}
	if err := c.validateSelectedRepairContext(ctx, item, r.ContextManifest); err != nil {
		return nil, err
	}
	for _, id := range resultDependencyIDs(item) {
		original := todoItemByID(source.Session.Tasks, id)
		if original == nil {
			return nil, fmt.Errorf("legacy source %s missing", id)
		}
		if err := c.validateContractRecoverySource(ctx, original, source.Session); err != nil {
			return nil, err
		}
	}
	p := &LegacySubmissionRecovery{Version: 1, SourceBranch: source.BranchID, SourcePolicy: plan.SourcePolicy, Candidate: cloneResultCandidate(item.ResultCandidate),
		OriginalExecution: cloneExecutionContract(item.Execution), Execution: cloneExecutionContract(item.Execution), AttemptsUsed: r.StepBudget.Used}
	p.Execution.ToolSequence = nil
	p.SourceTaskSHA256, _ = collectionDigest(item)
	p.OriginalTaskInputSHA256, _ = decisionTaskInputDigest(item)
	p.ReceiptSHA256, _ = collectionDigest(r)
	p.EvidenceSHA256, err = c.resultCollectionEvidenceHash(item)
	if err != nil {
		return nil, err
	}
	p.ArgumentsSHA256 = legacyBytesHash(args)
	p.WorkspaceSHA256, err = c.collectionWorkspaceIdentity(r, c.artifactStoreRootPath())
	if err != nil {
		return nil, err
	}
	body, err := c.legacySubmissionWork(item, p, args)
	if err != nil {
		return nil, err
	}
	p.Work = ResultCollectionRef{ArtifactID: "sha256-" + legacyBytesHash(body), SHA256: legacyBytesHash(body), Bytes: int64(len(body))}
	return p, validateLegacySubmissionSource(item, p, plan)
}

func validateLegacySubmissionReceipt(item *TodoItem) error {
	r := item.ExecutionReceipt
	if r.FinishedAt.IsZero() || r.ExitCode == nil || *r.ExitCode != 1 || r.ModelExecutionID == "" || r.StepBudget == nil || r.StepBudget.Used < 1 || r.StepBudget.Used > r.StepBudget.Limit ||
		r.RunInputSnapshotID != item.RunInputSnapshotID || r.RunInputSnapshotHash != item.RunInputSnapshotHash || r.ExecutionTarget != item.ExecutionTarget ||
		r.SubmittedResult == nil || r.SubmittedResult.Status != TaskResultStatusBlocked {
		return fmt.Errorf("legacy recovery requires a settled blocked submission receipt")
	}
	return nil
}

func legacyBytesHash(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func validateLegacySubmissionTranscript(item *TodoItem, t *taskTranscript, args []byte) error {
	calls := map[string]string{}
	results := map[string]bool{}
	candidateSeen := false
	for _, r := range t.evidenceRecords() {
		switch r.Event {
		case "tool_call":
			if r.Tool != submitResultToolName || r.ToolCallID == "" || calls[r.ToolCallID] != "" {
				return fmt.Errorf("legacy transcript contains work or duplicate calls")
			}
			calls[r.ToolCallID] = r.Input
		case "tool_result":
			if r.Tool != submitResultToolName || calls[r.ToolCallID] == "" || results[r.ToolCallID] {
				return fmt.Errorf("legacy transcript call/result mismatch")
			}
			results[r.ToolCallID] = true
			value, err := decodeResultContractJSON([]byte(calls[r.ToolCallID]))
			if err != nil {
				return err
			}
			normalized, _ := json.Marshal(value, json.Deterministic(true))
			if string(normalized) == string(args) && r.Error && r.ToolCallID == item.FailureEvent.FailedStepID {
				candidateSeen = true
			}
		}
	}
	if !candidateSeen || len(calls) != len(results) || len(calls) != item.ExecutionReceipt.StepBudget.Used {
		return fmt.Errorf("legacy transcript mismatch: rejected=%t calls=%d results=%d used=%d failed_step=%s", candidateSeen, len(calls), len(results), item.ExecutionReceipt.StepBudget.Used, item.FailureEvent.FailedStepID)
	}
	for _, d := range item.ExecutionReceipt.ToolDispositions {
		if d.ToolName != submitResultToolName || d.Executed || d.Kind != "policy_denied" || d.ReasonCode != "task_sequence_denied" || calls[d.ToolCallID] == "" ||
			d.RunID != item.ExecutionReceipt.RunID || d.TodoID != item.ID || d.Attempt != item.ExecutionReceipt.Attempt {
			return fmt.Errorf("legacy receipt contains an unrelated policy denial")
		}
	}
	return nil
}

func (c *Coordinator) legacySubmissionWork(item *TodoItem, p *LegacySubmissionRecovery, args []byte) ([]byte, error) {
	binding, err := legacyWorkBinding(p)
	if err != nil {
		return nil, err
	}
	workspace, err := filepath.Abs(c.artifactStoreRootPath())
	if err != nil {
		return nil, err
	}
	def := c.session.Agents[normalizedName(item.Agent)]
	if def == nil {
		return nil, fmt.Errorf("legacy worker missing")
	}
	sources, err := c.resultCandidateEvidenceSources(item)
	if err != nil {
		return nil, err
	}
	results := make([]*TaskResult, 0, len(sources))
	for _, source := range sources {
		results = append(results, cloneTaskResult(source.TypedResult))
	}
	input, err := json.Marshal(struct {
		Goal        string        `json:"goal"`
		Constraints string        `json:"constraints"`
		Sources     []*TaskResult `json:"accepted_sources"`
	}{item.Goal, item.Constraints, results}, json.Deterministic(true), collectionDurationFormat)
	if err != nil {
		return nil, err
	}
	work := resultCollectionWork{Version: resultCollectionVersion, BindingSHA256: binding, Receipt: cloneExecutionReceipt(item.ExecutionReceipt),
		Workspace: workspace, Report: args, Input: input, Generation: def.Generation, GenerationCaptured: true, TeamGeneration: c.session.Config.Generation, TimeoutSeconds: def.Timeout}
	return json.Marshal(work, json.Deterministic(true), collectionDurationFormat)
}

func (c *Coordinator) persistLegacySubmissionWork(ctx context.Context, source *ResumeSource, plan *ContractRecoveryPlan) error {
	store, err := OpenEventStoreReadOnly(c.session.Workspace)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	restore := c.installResumeValidationSource(source, store)
	defer restore()
	for _, patch := range plan.Tasks {
		if p := patch.LegacySubmission; p != nil {
			item := todoItemByID(source.Session.Tasks, patch.ID)
			value, err := c.loadLegacyCandidate(ctx, item, p.Candidate, p.SourceBranch)
			if err != nil {
				return err
			}
			args, err := json.Marshal(value, json.Deterministic(true))
			if err != nil {
				return err
			}
			body, err := c.legacySubmissionWork(item, p, args)
			if err != nil {
				return err
			}
			cp := &ResultCollectionCheckpoint{SourceRunID: item.ExecutionReceipt.RunID, TaskID: item.ID, Agent: item.Agent, Attempt: item.ExecutionReceipt.Attempt}
			ref, err := c.putCollectionBytes(ctx, cp, "work", body, resultCollectionWorkMaxBytes)
			if err != nil {
				return err
			}
			if ref != p.Work {
				return errResultCollectionStale
			}
		}
	}
	return nil
}

func projectLegacySubmission(item *TodoItem, patch ContractRecoveryTask, plan *ContractRecoveryPlan, branch string) {
	p := cloneLegacySubmission(patch.LegacySubmission)
	r := cloneExecutionReceipt(item.ExecutionReceipt)
	item.Execution = cloneExecutionContract(p.Execution)
	digest, _ := decisionTaskInputDigest(item)
	cp := &ResultCollectionCheckpoint{LegacySubmission: p, Version: resultCollectionVersion, Revision: 1, State: ResultCollectionBlocked,
		BranchID: branch, SourceRunID: r.RunID, TaskID: item.ID, Agent: item.Agent, Attempt: r.Attempt, ModelExecutionID: r.ModelExecutionID, ExecutionTarget: r.ExecutionTarget,
		WorkOccurrenceRevision: item.OccurrenceRevision, SubmissionRevision: item.OccurrenceRevision, WorkDispatchID: "legacy-submission:" + p.Candidate.SHA256, SubmissionDispatchID: "legacy-submission:" + p.Candidate.SHA256,
		PolicySHA256: plan.Policy.ConfigurationHash, TaskContractSHA256: digest, InputsID: item.RunInputSnapshotID, InputsSHA256: item.RunInputSnapshotHash,
		EvidenceSHA256: p.EvidenceSHA256, EvidenceRevision: 1, WorkspaceIdentitySHA256: p.WorkspaceSHA256, CollectorSchemaRevision: resultCollectionVersion,
		TurnsUsed: p.AttemptsUsed, TurnLimit: max(resultCollectionDefaultTurns, p.AttemptsUsed), Work: p.Work, FailureReason: RepairFailureInvalidSchema}
	// This is a derived submission receipt, not a rewrite of the original work.
	// The original remains in the immutable capsule and the parent's events.
	r.ResultCollection = cp
	r.RepairProvenance = &RepairProvenance{Attempted: true, RepairAttempts: p.AttemptsUsed, FailureReason: RepairFailureInvalidSchema}
	item.ExecutionReceipts = appendExecutionReceipt(item.ExecutionReceipts, r)
	item.ExecutionReceipt = &r
	item.TypedResult, item.ResultCandidate, item.VerifyResult = nil, nil, nil
	item.FailureEvent = &FailureEventPayload{TaskID: item.ID, Phase: "protocol", FailureClass: FailureProtocol, RetryDisposition: NeedsHuman}
	item.Status = TaskBlocked
	item.Detail = "legacy submission recovered for result-only repair; worker replay forbidden"
}

func validateLegacyCollection(cp *ResultCollectionCheckpoint) error {
	p := cp.LegacySubmission
	if p == nil {
		return nil
	}
	if p.Version != 1 || cp.ContractUpgrade != nil || p.Candidate == nil || !validCollectionDigest(p.SourceTaskSHA256) || !validCollectionDigest(p.ReceiptSHA256) ||
		!validCollectionDigest(p.EvidenceSHA256) || !validCollectionDigest(p.OriginalTaskInputSHA256) || !validCollectionDigest(p.ArgumentsSHA256) || !validCollectionDigest(p.SourcePolicy) ||
		p.Candidate.BranchID != p.SourceBranch || !validateLegacyExecutionChange(p.OriginalExecution, p.Execution) ||
		cp.BranchID == p.SourceBranch || cp.PolicySHA256 == p.SourcePolicy || cp.Work != p.Work || cp.WorkspaceIdentitySHA256 != p.WorkspaceSHA256 ||
		cp.FanoutLeaf || cp.InputsID != p.Candidate.InputsID || cp.InputsSHA256 != p.Candidate.InputsHash || cp.SourceRunID != p.Candidate.SourceRunID ||
		cp.TaskID != p.Candidate.TaskID || cp.Agent != p.Candidate.Agent || cp.Attempt != p.Candidate.Attempt || p.AttemptsUsed < 1 || cp.TurnsUsed < p.AttemptsUsed {
		return fmt.Errorf("invalid legacy collection proof")
	}
	return nil
}

func (c *Coordinator) validateLegacyWork(ctx context.Context, item *TodoItem, cp *ResultCollectionCheckpoint, work *resultCollectionWork) error {
	p := cp.LegacySubmission
	receiptHash, _ := collectionDigest(work.Receipt)
	if receiptHash != p.ReceiptSHA256 || legacyBytesHash(work.Report) != p.ArgumentsSHA256 || cp.Work != p.Work {
		return errResultCollectionStale
	}
	evidence, err := c.resultCollectionEvidenceHash(item)
	if err != nil || evidence != p.EvidenceSHA256 {
		return errResultCollectionStale
	}
	// Recheck the original immutable CAS wrapper, rather than trusting copied text.
	original := cloneTodoItem(item)
	original.ExecutionReceipt = new(cloneExecutionReceipt(&work.Receipt))
	original.Execution = cloneExecutionContract(p.OriginalExecution)
	original.ResultCandidate = cloneResultCandidate(p.Candidate)
	value, err := c.loadLegacyCandidate(ctx, original, p.Candidate, p.SourceBranch)
	if err != nil {
		return err
	}
	args, err := json.Marshal(value, json.Deterministic(true))
	if err != nil || string(args) != string(work.Report) {
		return errResultCollectionStale
	}
	body, err := c.legacySubmissionWork(original, p, args)
	if err != nil || legacyBytesHash(body) != p.Work.SHA256 {
		return errResultCollectionStale
	}
	return nil
}
