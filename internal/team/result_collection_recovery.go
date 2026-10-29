package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"github.com/kjelly/hufu/internal/tools"
)

// ResultCollectionRecoveryDisposition describes only result-stage authority.
type ResultCollectionRecoveryDisposition string

const (
	RecoveryCommitLocal             ResultCollectionRecoveryDisposition = "commit_local"
	RecoveryRepairClaims            ResultCollectionRecoveryDisposition = "repair_claims"
	RecoveryResumeCollection        ResultCollectionRecoveryDisposition = "resume_collection"
	RecoveryBlocked                 ResultCollectionRecoveryDisposition = "blocked"
	resultRecoveryValidatorRevision                                     = 2
)

type ResultCollectionRecoveryCheck struct {
	Code       string `json:"code"`
	Passed     bool   `json:"passed"`
	Diagnostic string `json:"diagnostic,omitempty"`
}

// ResultCollectionRecoveryAssessment is immutable grant evidence. Checkpoint
// contains references, never report/prompt bytes. Its digest pins all consumed
// turns, original response/prepared bytes, policy and source bindings.
type ResultCollectionRecoveryAssessment struct {
	TaskID            string                              `json:"task_id,omitempty"`
	BranchID          string                              `json:"branch_id,omitempty"`
	SourceRunID       string                              `json:"source_run_id,omitempty"`
	JournalHeadID     string                              `json:"journal_head_id"`
	Version           int                                 `json:"version"`
	ValidatorRevision int                                 `json:"validator_revision"`
	Disposition       ResultCollectionRecoveryDisposition `json:"disposition"`
	Checkpoint        *ResultCollectionCheckpoint         `json:"checkpoint"`
	CheckpointSHA256  string                              `json:"checkpoint_sha256"`
	OriginalReason    RepairFailureReason                 `json:"original_reason"`
	TranscriptRef     string                              `json:"transcript_ref"`
	Checks            []ResultCollectionRecoveryCheck     `json:"checks"`
	TurnsUsed         int                                 `json:"turns_used"`
	RemainingTurns    int                                 `json:"remaining_turns"`
	AdditionalTurns   int                                 `json:"additional_turns"`
	AcceptedTasks     *int                                `json:"accepted_tasks,omitempty"`
	ContinuesWorkflow bool                                `json:"continues_workflow"`
	Fingerprint       string                              `json:"fingerprint"`
}

func recoveryAssessmentFingerprint(a *ResultCollectionRecoveryAssessment) (string, error) {
	copy := *a
	copy.Fingerprint = ""
	return collectionDigest(copy)
}

func cloneRecoveryAssessment(a *ResultCollectionRecoveryAssessment) *ResultCollectionRecoveryAssessment {
	if a == nil {
		return nil
	}
	copy := *a
	copy.Checkpoint = cloneResultCollectionCheckpoint(a.Checkpoint)
	if a.AcceptedTasks != nil {
		copy.AcceptedTasks = new(*a.AcceptedTasks)
	}
	copy.Checks = slices.Clone(a.Checks)
	return &copy
}

func validateCollectionRecoveryTarget(item *TodoItem) error {
	if item == nil || item.Status != TaskBlocked || item.FailureEvent == nil || item.FailureEvent.FailureClass != FailureProtocol {
		return fmt.Errorf("recovery requires an existing blocked protocol task")
	}
	if item.CheckpointPause || item.RecoveryState == RecoveryStateUnknown || item.RecoveryState == RecoveryStatePartial {
		return fmt.Errorf("result-only recovery requires reconciliation of paused or uncertain side effects")
	}
	if item.DecisionProfile != "" && item.DecisionProfile != "off" {
		return fmt.Errorf("result-only recovery does not support an active decision discipline; reconcile its governing decision first")
	}
	receipt := item.ExecutionReceipt
	if item.WorkerWorkspace.isolated() || receipt == nil || receipt.ResultCollection == nil || receipt.ResultCollection.FanoutLeaf || receipt.ExecutionWorldID != "" || len(item.ExecutionTopology) > 1 {
		return fmt.Errorf("recovery requires a shared local collection without model fanout")
	}
	if receipt.RepairProvenance == nil || !receipt.RepairProvenance.Attempted || receipt.RepairProvenance.Success || receipt.Attempt != receipt.ResultCollection.Attempt || (receipt.OccurrenceAttempt > 0 && receipt.OccurrenceAttempt != item.Retries+1) {
		return fmt.Errorf("recovery requires the original failed collection receipt")
	}
	return validateResultCollectionCheckpoint(receipt.ResultCollection)
}

// AssessResultCollectionRecovery performs no transitions, reservations, writes,
// tool calls, provider construction, or decision inference. Both explicit
// repair and the read-only CLI projection use this boundary.
func (c *Coordinator) AssessResultCollectionRecovery(ctx context.Context, taskID string) (*ResultCollectionRecoveryAssessment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	item := c.todoItemByID(taskID)
	a := &ResultCollectionRecoveryAssessment{TaskID: taskID, BranchID: c.activeBranchID(), Version: 1, ValidatorRevision: resultRecoveryValidatorRevision, Disposition: RecoveryBlocked, Checks: []ResultCollectionRecoveryCheck{}, ContinuesWorkflow: true}
	if item != nil && item.ExecutionReceipt != nil {
		a.SourceRunID = item.ExecutionReceipt.RunID
	}
	check := func(code string, err error) bool {
		entry := ResultCollectionRecoveryCheck{Code: code, Passed: err == nil}
		if err != nil {
			entry.Diagnostic = schemaRepairDiagnostic(err.Error())
		}
		a.Checks = append(a.Checks, entry)
		return err == nil
	}
	finish := func() (*ResultCollectionRecoveryAssessment, error) {
		if a.JournalHeadID != "" {
			latest, err := c.EventJournal().ReadEvents(ctx)
			if err != nil || len(latest) == 0 || latest[len(latest)-1].ID != a.JournalHeadID {
				a.Disposition = RecoveryBlocked
				a.AdditionalTurns = 0
				check("stable_journal", ErrEventHeadChanged)
			}
		}
		if a.Disposition == RecoveryBlocked {
			a.ContinuesWorkflow = false
		}
		var err error
		a.Fingerprint, err = recoveryAssessmentFingerprint(a)
		return a, err
	}
	if !check("target", validateCollectionRecoveryTarget(item)) {
		return finish()
	}
	cp := currentResultCollection(item)
	a.Checkpoint = cp
	a.CheckpointSHA256, _ = collectionDigest(cp)
	a.TranscriptRef = item.ExecutionReceipt.TranscriptRef
	a.OriginalReason = cp.FailureReason
	if a.OriginalReason == "" {
		a.OriginalReason = item.ExecutionReceipt.RepairProvenance.FailureReason
	}
	a.TurnsUsed, a.RemainingTurns = cp.TurnsUsed, max(0, cp.TurnLimit-cp.TurnsUsed)
	for _, other := range UnresolvedTaskReferences(c.taskTracker.TodoList().Items()) {
		if other.ID != taskID {
			a.ContinuesWorkflow = false
			break
		}
	}
	if !c.assessRecoveryLineage(ctx, item, a, check) {
		return finish()
	}
	work, err := c.loadCollectionWork(ctx, item, cp)
	if !check("work_sources_policy", err) {
		return finish()
	}
	if work.Receipt.FinishedAt.IsZero() {
		check("execution_ended", fmt.Errorf("work receipt is unfinished"))
		return finish()
	}
	// No ordinary worker can be active once a blocked protocol task has sealed
	// work. Refuse a locally held lease as well as an unfinished work receipt.
	if _, active := c.activeTaskResultOccurrence(taskID); active {
		check("execution_ended", errTaskResultStale)
		return finish()
	}
	_, compiled, err := c.boundResultContract(taskID)
	if !check("contract", err) {
		return finish()
	}
	plans, err := c.fixedCollectionPlans(taskID)
	if err == nil {
		_, err = c.resolveDecisionCollectionPlans(ctx, taskID, plans)
	}
	if !check("settled_decisions", err) {
		return finish()
	}
	for _, blob := range []struct {
		kind string
		ref  *ResultCollectionRef
	}{{"request", cp.Request}, {"response", cp.Response}, {"claims", cp.Prepared}, {"accepted", cp.Accepted}} {
		if blob.ref != nil {
			_, err = c.readCollectionBlob(ctx, cp, blob.kind, blob.ref)
			if !check(blob.kind+"_integrity", err) {
				return finish()
			}
		}
	}
	evidence, err := c.restoreTranscriptEvidence(ctx, &work.Receipt, cp.SourceRunID)
	if !check("transcript", err) {
		return finish()
	}
	if !check("terminal_context", validateRecoveryTerminalContext(c.session.Workspace, cp)) {
		return finish()
	}
	ctx = context.WithValue(ctx, resultCollectionEvidenceKey{}, evidence)
	ctx = context.WithValue(ctx, tools.AgentNameKey, cp.Agent)
	ctx = context.WithValue(ctx, todoIDKey{}, taskID)
	ctx = context.WithValue(ctx, executionAttemptKey{}, cp.Attempt)
	ctx = withSubmitResultRuntimeIdentity(ctx, submitResultRuntimeIdentity{RunID: cp.SourceRunID, TaskID: taskID, Attempt: cp.Attempt, Agent: cp.Agent, OccurrenceRevision: cp.SubmissionRevision, DispatchID: cp.SubmissionDispatchID})
	ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: cp.SourceRunID, TaskID: taskID, Attempt: cp.Attempt, AgentName: cp.Agent, AgentRole: "worker", ModelExecutionID: cp.ModelExecutionID})
	manifest, err := collectionContextManifest(item, cp, &work.Receipt)
	if !check("context_manifest", err) {
		return finish()
	}
	if item.DependencyResultView != nil {
		if !check("dependency_view", c.validateSelectedRepairContext(ctx, item, manifest)) {
			return finish()
		}
	}
	if manifest != nil {
		ctx = withInvocationMetadata(ctx, invocationMetadataFromManifest(manifest))
	}
	if denial := c.checkpointDenial(taskID); denial != "" {
		check("checkpoint", errors.New(denial))
		return finish()
	}
	if work.Receipt.ArtifactScope != nil {
		ctx = context.WithValue(ctx, artifactAccessScopeKey, cloneArtifactAccessScope(work.Receipt.ArtifactScope))
	}
	if cp.State == ResultCollectionCommitted {
		result, err := c.acceptedCollectionResult(ctx, item, cp)
		if err == nil {
			accepted := cloneTodoItem(item)
			accepted.TypedResult = result
			err = c.validateAcceptedDependencyPayload(accepted)
		}
		if check("accepted_result", err) {
			a.Disposition = RecoveryCommitLocal
		}
		return finish()
	}
	c.assessRecoveryClaims(ctx, item, a, work, compiled, check)
	return finish()
}

func validateRecoveryAssessment(a *ResultCollectionRecoveryAssessment) error {
	if a == nil || a.Version != 1 || (a.ValidatorRevision != 1 && a.ValidatorRevision != 2) || a.Checkpoint == nil || a.JournalHeadID == "" || !validArtifactID(a.TranscriptRef) {
		return errResultCollectionStale
	}
	if (a.TaskID != "" && a.TaskID != a.Checkpoint.TaskID) || (a.BranchID != "" && a.BranchID != a.Checkpoint.BranchID) || (a.SourceRunID != "" && a.SourceRunID != a.Checkpoint.SourceRunID) {
		return errResultCollectionStale
	}
	if err := validateResultCollectionCheckpoint(a.Checkpoint); err != nil {
		return err
	}
	digest, err := collectionDigest(a.Checkpoint)
	if err != nil || digest != a.CheckpointSHA256 {
		return errResultCollectionStale
	}
	hash, err := recoveryAssessmentFingerprint(a)
	if err != nil || hash != a.Fingerprint || a.TurnsUsed != a.Checkpoint.TurnsUsed || a.RemainingTurns != max(0, a.Checkpoint.TurnLimit-a.TurnsUsed) {
		return errResultCollectionStale
	}
	switch a.Disposition {
	case RecoveryCommitLocal:
		if a.AdditionalTurns != 0 {
			return errResultCollectionStale
		}
	case RecoveryRepairClaims:
		if a.AdditionalTurns != 2 {
			return errResultCollectionStale
		}
	case RecoveryResumeCollection:
		if a.AdditionalTurns != 0 && a.AdditionalTurns != 2 {
			return errResultCollectionStale
		}
	default:
		return errResultCollectionBlocked
	}
	return validateRecoveryProofChecks(a)
}

// AssessResumeResultCollection uses a checked source projection and a read-only
// journal. It never activates the branch or restores a live occurrence lease.
// The receiver must be a fresh dry-run coordinator, not an executing runtime.
func (c *Coordinator) AssessResumeResultCollection(ctx context.Context, source *ResumeSource, taskID string) (*ResultCollectionRecoveryAssessment, error) {
	if c.eventStore != nil || c.executionRunID != "" || source == nil || source.Session == nil {
		return nil, fmt.Errorf("recovery preview requires a fresh read-only coordinator and source")
	}
	if err := c.prepareRecoveryValidation(); err != nil {
		return nil, err
	}
	store, err := OpenEventStoreReadOnly(c.session.Workspace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()
	c.eventStore = store
	c.eventJournal = eventStoreJournal{store: store}
	c.recoveryBranch = source.BranchID
	defer func() { c.eventStore = nil; c.eventJournal = nil; c.recoveryBranch = "" }()
	c.taskTracker = NewTaskTracker()
	c.taskTracker.TodoList().AddReserved(source.Session.Tasks)
	a, err := c.AssessResultCollectionRecovery(ctx, taskID)
	if err != nil {
		return nil, err
	}
	// Public resume requires a canonical frozen input snapshot even for a team
	// with no declared inputs. Match that admission barrier in its preview.
	if !slices.ContainsFunc(source.Session.RunInputSnapshots, func(snapshot RunInputSnapshot) bool {
		return a.Checkpoint != nil && snapshot.ID == a.Checkpoint.InputsID && snapshot.SnapshotHash == a.Checkpoint.InputsSHA256 && snapshot.Team == c.session.Config.Name && ValidateRunInputSnapshot(&snapshot) == nil
	}) {
		a.Disposition, a.AdditionalTurns, a.ContinuesWorkflow = RecoveryBlocked, 0, false
		a.Checks = append(a.Checks, ResultCollectionRecoveryCheck{Code: "resume_frozen_inputs", Diagnostic: "selected task has no matching canonical frozen input binding; automatic rebinding of historical work is unsupported, and supplying new inputs cannot repair it"})
		a.Fingerprint, err = recoveryAssessmentFingerprint(a)
	}
	return a, err
}

// Historical grants validate against their saved revision and checkpoint,
// never by running today's evaluator on today's source results.
func validateCollectionRecoveryGrantEvent(event RunEvent, grant *ResultRepairGrant, receipt *ExecutionReceipt) error {
	a := grant.Recovery
	if err := validateRecoveryAssessment(a); err != nil {
		return err
	}
	cp := a.Checkpoint
	if err := validateCollectionRecoveryGrantIdentity(event, grant, receipt); err != nil {
		return err
	}

	next := receipt.ResultCollection
	before, err := collectionBindingDigest(cp)
	if err != nil {
		return err
	}
	after, err := collectionBindingDigest(next)
	if err != nil || before != after || next.Work != cp.Work || next.Revision != cp.Revision+1 || next.TurnsUsed != cp.TurnsUsed || next.TurnLimit != max(cp.TurnLimit, grant.AfterAttempts+grant.AdditionalAttempts) || next.SubmissionRevision <= cp.SubmissionRevision || next.SubmissionDispatchID == cp.SubmissionDispatchID {
		return errResultCollectionStale
	}
	if err := validateResultCollectionCheckpoint(next); err != nil {
		return err
	}
	switch a.Disposition {
	case RecoveryRepairClaims:
		if next.State != ResultCollectionReceived || next.Prepared != nil || next.Response == nil || cp.Response == nil || *next.Response != *cp.Response || next.FailureReason != RepairFailureInvalidSchema {
			return errResultCollectionStale
		}
	case RecoveryCommitLocal:
		expected := ResultCollectionReceived
		if cp.State == ResultCollectionCommitted {
			expected = ResultCollectionCommitted
		} else if cp.Prepared != nil {
			expected = ResultCollectionPrepared
		}
		if next.State != expected {
			return errResultCollectionStale
		}
	case RecoveryResumeCollection:
		if next.State != cp.State {
			return errResultCollectionStale
		}
	}
	return nil
}

func validateCollectionRecoveryReplay(events []RunEvent) error {
	checkpoints := make(map[string]*ResultCollectionCheckpoint)
	for _, event := range events {
		if event.Type == string(EventTaskResultCollectionCheckpointed) {
			var payload resultCollectionEvent
			if err := json.Unmarshal(event.Payload, &payload, collectionDurationFormat); err != nil {
				return err
			}
			checkpoints[event.TaskID] = payload.Receipt.ResultCollection
			continue
		}
		if event.TaskID == "" {
			continue
		}
		var payload struct {
			Grant   *ResultRepairGrant `json:"result_repair_grant"`
			Receipt *ExecutionReceipt  `json:"execution_receipt"`
		}
		if err := json.Unmarshal(event.Payload, &payload, collectionDurationFormat); err != nil {
			continue
		}
		if payload.Grant != nil && payload.Grant.Recovery != nil {
			if err := validateCollectionRecoveryGrantEvent(event, payload.Grant, payload.Receipt); err != nil {
				return fmt.Errorf("recovery grant at %s: %w", event.ID, err)
			}
			prior := checkpoints[event.TaskID]
			digest, err := collectionDigest(prior)
			if err != nil || prior == nil || digest != payload.Grant.Recovery.CheckpointSHA256 {
				return fmt.Errorf("recovery grant at %s has stale prior checkpoint", event.ID)
			}
		}
		if payload.Receipt != nil && payload.Receipt.ResultCollection != nil {
			checkpoints[event.TaskID] = payload.Receipt.ResultCollection
		}
	}
	return nil
}

func validateStoredRecoveryGrant(grant ResultRepairGrant, checkpoint *ResultCollectionCheckpoint) error {
	a := grant.Recovery
	if err := validateRecoveryAssessment(a); err != nil {
		return err
	}
	if grant.AfterAttempts != a.TurnsUsed || grant.AdditionalAttempts != a.AdditionalTurns || grant.TranscriptRef != a.TranscriptRef {
		return errResultCollectionStale
	}
	original, err := collectionBindingDigest(a.Checkpoint)
	if err != nil {
		return err
	}
	current, err := collectionBindingDigest(checkpoint)
	if err != nil || current != original || checkpoint.Work != a.Checkpoint.Work {
		return errResultCollectionStale
	}
	return nil
}

func (c *Coordinator) assessRecoveryLineage(ctx context.Context, item *TodoItem, a *ResultCollectionRecoveryAssessment, check func(string, error) bool) bool {
	cp := a.Checkpoint
	if !check("lineage", c.EventJournal().VerifyHashChain(ctx)) {
		return false
	}
	allEvents, headErr := c.EventJournal().ReadEvents(ctx)
	if !check("journal_head", headErr) {
		return false
	}
	if len(allEvents) == 0 {
		check("journal_head", fmt.Errorf("canonical journal is empty"))
		return false
	}
	a.JournalHeadID = allEvents[len(allEvents)-1].ID
	if cp.BranchID != c.activeBranchID() {
		check("branch", errResultCollectionStale)
		return false
	}
	// Verify frozen inputs from canonical lineage, rather than trusting the
	// matching ID/hash strings on the task and checkpoint alone.
	events, lineageErr := c.readActiveLineageEvents(ctx)
	if !check("lineage_projection", lineageErr) {
		return false
	}
	replayed, replayErr := ReplayTodoList(events)
	if !check("canonical_replay", replayErr) {
		return false
	}
	canonical := todoItemByID(replayed, item.ID)
	if !check("canonical_target", validateCollectionRecoveryTarget(canonical)) {
		return false
	}
	canonicalCheckpoint := currentResultCollection(canonical)
	canonicalDigest, _ := collectionDigest(canonicalCheckpoint)
	canonicalContract, _ := decisionTaskInputDigest(canonical)
	// Compare the frozen execution and source contract, not display-only fields
	// or separately refreshed failure telemetry on the live Todo projection.
	if canonical == nil || canonical.Status != item.Status || canonicalCheckpoint == nil {
		check("canonical_task", errResultCollectionStale)
		return false
	}
	if canonicalDigest != a.CheckpointSHA256 {
		check("canonical_checkpoint", errResultCollectionStale)
		return false
	}
	if canonicalContract != cp.TaskContractSHA256 {
		check("canonical_contract", errResultCollectionStale)
		return false
	}
	if canonical.ExecutionReceipt.TranscriptRef != a.TranscriptRef {
		check("canonical_transcript", errResultCollectionStale)
		return false
	}
	if canonical.ExecutionReceipt.RepairProvenance == nil || !sameCanonicalEvidence(canonical.ExecutionReceipt.RepairProvenance.OperatorGrants, item.ExecutionReceipt.RepairProvenance.OperatorGrants) {
		check("canonical_grants", errResultCollectionStale)
		return false
	}
	if cp.InputsID == "" && len(c.session.RunInputDefinitions) > 0 {
		check("frozen_inputs", fmt.Errorf("checkpoint has no original frozen input binding; an existing run snapshot alone does not authenticate this task's sealed work, and automatic historical rebinding is unsupported"))
		return false
	}
	if cp.InputsID != "" {
		var snapshot *RunInputSnapshot
		session := ReduceToSessionData(events)
		for _, candidate := range session.RunInputSnapshots {
			if candidate.ID == cp.InputsID && candidate.SnapshotHash == cp.InputsSHA256 {
				snapshot = new(candidate)
				break
			}
		}
		if snapshot == nil || snapshot.Team != c.session.Config.Name {
			check("frozen_inputs", fmt.Errorf("original input snapshot is unavailable"))
			return false
		}
		if !check("frozen_inputs", ValidateRunInputSnapshot(snapshot)) {
			return false
		}
		if !c.assessRecoveryAcceptedSources(ctx, snapshot, a, check) {
			return false
		}
	} else if !c.assessRecoveryAcceptedSources(ctx, nil, a, check) {
		return false
	}
	return true
}

func (c *Coordinator) assessRecoveryClaims(ctx context.Context, item *TodoItem, a *ResultCollectionRecoveryAssessment, work *resultCollectionWork, compiled *CompiledResultContract, check func(string, error) bool) {
	cp, taskID := a.Checkpoint, item.ID
	ref, kind := cp.Prepared, "claims"
	if ref == nil {
		ref, kind = cp.Response, "response"
	}
	if ref == nil {
		switch a.OriginalReason {
		case RepairFailureCommit, repairFailureCollectionUnavailable, repairFailureCollectionBudget, repairFailurePreparation, RepairFailureResultMissing:
			a.Disposition = RecoveryResumeCollection
			if a.OriginalReason != RepairFailureCommit {
				a.AdditionalTurns = 2
			}
		default:
			check("claims_unavailable", fmt.Errorf("no claims to reclassify failure %q", a.OriginalReason))
		}
		return
	}
	raw, err := c.readCollectionBlob(ctx, cp, kind, ref)
	if !check("claims_integrity", err) {
		return
	}
	resolved, err := c.resolveResultEvidenceKeys(ctx, taskID, string(raw))
	if !check("evidence_keys", err) {
		return
	}
	if !c.validateRecoveryIndependentClaims(ctx, cp, work, compiled, resolved, check) {
		return
	}
	input, err := c.decodeCollectionClaims(taskID, []byte(resolved))
	if err == nil {
		admission := localResultAdmission{coordinator: c, todoID: taskID}
		var res *TaskResult
		res, err = admission.validateClaims(ctx, input, localResultSubmissionContract(c, taskID), func() RepairFailureReason { return RepairFailureInvalidSchema }, false)
		if err == nil {
			err = c.attestInvariantClaims(taskID, cp.Attempt, cp.ModelExecutionID, input.InvariantAssessments, res)
			if _, mismatch := errors.AsType[*invariantFindingSeverityMismatch](err); mismatch {
				err = rejectLocalResult(RepairFailureInvalidSchema, err)
			}
		}
		if err == nil {
			contract := localResultSubmissionContract(c, taskID)
			err = contract.validateFinalizableResult(res)
			if err != nil && collectionProjectionFormatFailure(contract, res) {
				err = rejectLocalResult(RepairFailureInvalidSchema, err)
			}
		}
	}
	if err == nil {
		a.Disposition = RecoveryCommitLocal
	} else {
		check("canonical_claims", err)
		reason := localResultRejectionReason(err)
		if reason == RepairFailureInvalidSchema || reason == RepairFailureResultMissing {
			a.Disposition, a.AdditionalTurns = RecoveryRepairClaims, 2
		}
	}
}

func validateCollectionRecoveryGrantIdentity(event RunEvent, grant *ResultRepairGrant, receipt *ExecutionReceipt) error {
	a := grant.Recovery
	cp := a.Checkpoint
	if event.Type != string(EventTaskProtocolIncomplete) || grant.RunID != event.RunID || grant.BranchID != event.BranchID || cp.TaskID != event.TaskID || cp.BranchID != event.BranchID || grant.Model != "" || grant.AfterAttempts != a.TurnsUsed || grant.AdditionalAttempts != a.AdditionalTurns || grant.TranscriptRef != a.TranscriptRef || receipt == nil || receipt.ResultCollection == nil {
		return errResultCollectionStale
	}
	if receipt.RunID != cp.SourceRunID || receipt.TaskID != cp.TaskID || receipt.Attempt != cp.Attempt || receipt.ModelExecutionID != cp.ModelExecutionID || receipt.ExecutionTarget != cp.ExecutionTarget || receipt.TranscriptRef != a.TranscriptRef || receipt.RunInputSnapshotID != cp.InputsID || receipt.RunInputSnapshotHash != cp.InputsSHA256 {
		return errResultCollectionStale
	}
	return nil
}
