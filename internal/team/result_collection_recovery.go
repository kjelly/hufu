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
	RecoveryCommitLocal                    ResultCollectionRecoveryDisposition = "commit_local"
	RecoveryRepairClaims                   ResultCollectionRecoveryDisposition = "repair_claims"
	RecoveryResumeCollection               ResultCollectionRecoveryDisposition = "resume_collection"
	RecoveryBlocked                        ResultCollectionRecoveryDisposition = "blocked"
	RecoverySelectReferences               ResultCollectionRecoveryDisposition = "select_references"
	resultRecoveryStrandedVerifierRevision                                     = 18
	resultRecoveryBoundedSchemaRevision                                        = 19
	resultRecoveryValidatorRevision                                            = resultRecoveryBoundedSchemaRevision
	collectionSchemaBudgetCheck                                                = "collection_schema_budget"
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
	TaskID                string                              `json:"task_id,omitempty"`
	BranchID              string                              `json:"branch_id,omitempty"`
	SourceRunID           string                              `json:"source_run_id,omitempty"`
	JournalHeadID         string                              `json:"journal_head_id"`
	Version               int                                 `json:"version"`
	ValidatorRevision     int                                 `json:"validator_revision"`
	Disposition           ResultCollectionRecoveryDisposition `json:"disposition"`
	Checkpoint            *ResultCollectionCheckpoint         `json:"checkpoint"`
	CheckpointSHA256      string                              `json:"checkpoint_sha256"`
	OriginalReason        RepairFailureReason                 `json:"original_reason"`
	TranscriptRef         string                              `json:"transcript_ref"`
	Checks                []ResultCollectionRecoveryCheck     `json:"checks"`
	TurnsUsed             int                                 `json:"turns_used"`
	RemainingTurns        int                                 `json:"remaining_turns"`
	AdditionalTurns       int                                 `json:"additional_turns"`
	MaxOutputTokens       int                                 `json:"collector_output_limit,omitempty"`
	AcceptedTasks         *int                                `json:"accepted_tasks,omitempty"`
	ContinuesWorkflow     bool                                `json:"continues_workflow"`
	DeferredDispatch      []string                            `json:"deferred_dispatch,omitempty"`
	ReferenceSelections   []AcceptedReferenceSelection        `json:"reference_selections,omitempty"`
	ReferenceChoices      []AcceptedReferenceChoice           `json:"reference_choices,omitempty"`
	ReferenceClaimsSHA256 string                              `json:"reference_claims_sha256,omitempty"`
	Fingerprint           string                              `json:"fingerprint"`
}

func (a *ResultCollectionRecoveryAssessment) requirePreparedLocalCommit(check func(string, error) bool) {
	if a.Disposition == RecoveryBlocked {
		return
	}
	if a.Disposition != RecoveryCommitLocal || a.AdditionalTurns != 0 {
		check("prepared_local_commit", fmt.Errorf("failed workflow permits only a local commit of the saved claims"))
		a.Disposition, a.AdditionalTurns = RecoveryBlocked, 0
	} else {
		check("prepared_local_commit", nil)
	}
	a.ContinuesWorkflow = false
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
	copy.DeferredDispatch = slices.Clone(a.DeferredDispatch)
	copy.ReferenceSelections = slices.Clone(a.ReferenceSelections)
	copy.ReferenceChoices = slices.Clone(a.ReferenceChoices)
	return &copy
}

// HasCancelledCollectionWork identifies a candidate for read-only assessment.
// It never authorizes recovery; AssessResultCollectionRecovery verifies sources.
func HasCancelledCollectionWork(item *TodoItem) bool {
	if item == nil || item.Status != TaskError || item.FailureEvent == nil || item.FailureEvent.FailureClass != FailureCancelled {
		return false
	}
	r := item.ExecutionReceipt
	if r == nil || currentResultCollection(item) == nil || !collectionWorkCompleted(r, true) {
		return false
	}
	cp := r.ResultCollection
	return cp.State != ResultCollectionBlocked && (cp.FailureReason == "" || cp.FailureReason == RepairFailureInvalidSchema)
}

func collectionRepairGrants(receipt *ExecutionReceipt) []ResultRepairGrant {
	if receipt == nil || receipt.RepairProvenance == nil {
		return nil
	}
	return receipt.RepairProvenance.OperatorGrants
}

func validateCollectionRecoveryTarget(item *TodoItem) error {
	cancelled := HasCancelledCollectionWork(item)
	if !cancelled && (item == nil || item.Status != TaskBlocked || item.FailureEvent == nil || item.FailureEvent.FailureClass != FailureProtocol) {
		return fmt.Errorf("recovery requires a blocked protocol task or cancelled collection with completed work")
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
	if (!cancelled && (receipt.RepairProvenance == nil || !receipt.RepairProvenance.Attempted)) || (receipt.RepairProvenance != nil && receipt.RepairProvenance.Success) || receipt.Attempt != receipt.ResultCollection.Attempt || (receipt.OccurrenceAttempt > 0 && receipt.OccurrenceAttempt != item.Retries+1) {
		return fmt.Errorf("recovery requires the original failed collection receipt")
	}
	return validateResultCollectionCheckpoint(receipt.ResultCollection)
}

// AssessResultCollectionRecovery performs no transitions, reservations, writes,
// tool calls, provider construction, or decision inference. Both explicit
// repair and the read-only CLI projection use this boundary.
func (c *Coordinator) AssessResultCollectionRecovery(ctx context.Context, taskID string) (*ResultCollectionRecoveryAssessment, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	progress := c.beginRuntimeValidation("result_recovery", taskID)
	defer progress.close()
	item := c.todoItemByID(taskID)
	a := &ResultCollectionRecoveryAssessment{TaskID: taskID, BranchID: c.activeBranchID(), Version: 1, ValidatorRevision: resultRecoveryValidatorRevision, Disposition: RecoveryBlocked, Checks: []ResultCollectionRecoveryCheck{}, ContinuesWorkflow: true}
	if item != nil && item.ExecutionReceipt != nil {
		a.SourceRunID = item.ExecutionReceipt.RunID
	}
	var localCommitOnly bool
	check := func(code string, err error) bool {
		progress.checked(code)
		entry := ResultCollectionRecoveryCheck{Code: code, Passed: err == nil}
		if err != nil {
			entry.Diagnostic = schemaRepairDiagnostic(err.Error())
		}
		a.Checks = append(a.Checks, entry)
		return err == nil
	}
	finish := func() (*ResultCollectionRecoveryAssessment, error) {
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		if a.JournalHeadID != "" {
			latest, err := c.EventJournal().ReadEvents(ctx)
			if err != nil || len(latest) == 0 || latest[len(latest)-1].ID != a.JournalHeadID {
				a.Disposition = RecoveryBlocked
				a.AdditionalTurns = 0
				check("stable_journal", ErrEventHeadChanged)
			}
		}
		if localCommitOnly {
			a.requirePreparedLocalCommit(check)
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
	if a.OriginalReason == "" && item.ExecutionReceipt.RepairProvenance != nil {
		a.OriginalReason = item.ExecutionReceipt.RepairProvenance.FailureReason
	}
	if a.OriginalReason == "" && HasCancelledCollectionWork(item) {
		a.OriginalReason = repairFailureCollectionUnavailable
	}
	a.TurnsUsed, a.RemainingTurns = cp.TurnsUsed, max(0, cp.TurnLimit-cp.TurnsUsed)
	for _, other := range UnresolvedTaskReferences(c.taskTracker.TodoList().Items()) {
		if other.ID != taskID {
			a.ContinuesWorkflow = false
			break
		}
	}
	if !c.assessRecoveryLineage(ctx, item, a, check, validateCollectionRecoveryTarget) {
		return finish()
	}
	workflow := c.collectionWorkflowSnapshot()
	localCommitOnly, workflowErr := resultRepairLocalCommitOnly(workflow, taskID, true)
	if !check("workflow_recovery", workflowErr) {
		return finish()
	}
	if workflow.WorkflowState == PhaseFailed && !localCommitOnly {
		for _, peer := range workflow.Tasks {
			if collectionDispatchDeferred(peer, item) {
				a.DeferredDispatch = append(a.DeferredDispatch, peer.ID)
			}
		}
	}
	c.assessResultCollectionWork(ctx, item, a, check)
	return finish()
}

func (c *Coordinator) assessResultCollectionWork(ctx context.Context, item *TodoItem, a *ResultCollectionRecoveryAssessment, check func(string, error) bool) {
	cp, taskID := a.Checkpoint, item.ID
	work, err := c.loadCollectionWork(ctx, item, cp)
	if !check("work_sources_policy", err) {
		return
	}
	a.MaxOutputTokens, _ = ctx.Value(resultRepairMaxOutputKey{}).(int)
	if a.MaxOutputTokens != 0 && !check("collector_output_limit", c.validateCollectionOutputIncrease(item, work, a.MaxOutputTokens)) {
		return
	}
	if !collectionWorkCompleted(&work.Receipt, HasCancelledCollectionWork(item)) {
		check("execution_ended", fmt.Errorf("work receipt is unfinished or cancelled work did not complete successfully"))
		return
	}
	// No ordinary worker can be active once a blocked protocol task has sealed
	// work. Refuse a locally held lease as well as an unfinished work receipt.
	if _, active := c.activeTaskResultOccurrence(taskID); active {
		check("execution_ended", errTaskResultStale)
		return
	}
	check("execution_ended", nil)
	_, compiled, err := c.boundResultContract(taskID)
	if !check("contract", err) {
		return
	}
	omissions, ok := c.assessRecoveryBlobIntegrity(ctx, cp, compiled, check)
	if !ok {
		return
	}
	plans, err := c.fixedCollectionPlans(taskID)
	if err == nil {
		_, err = c.resolveDecisionPlans(ctx, taskID, plans, nil, omissions.unsettledDecisions)
	}
	if !check("settled_decisions", err) {
		return
	}
	evidence, err := c.restoreTranscriptEvidence(ctx, &work.Receipt, cp.SourceRunID)
	if !check("transcript", err) {
		return
	}
	if !check("terminal_context", validateRecoveryTerminalContext(c.session.Workspace, cp)) {
		return
	}
	ctx = context.WithValue(ctx, resultCollectionEvidenceKey{}, evidence)
	ctx = context.WithValue(ctx, tools.AgentNameKey, cp.Agent)
	ctx = context.WithValue(ctx, todoIDKey{}, taskID)
	ctx = context.WithValue(ctx, executionAttemptKey{}, cp.Attempt)
	ctx = withSubmitResultRuntimeIdentity(ctx, submitResultRuntimeIdentity{RunID: cp.SourceRunID, TaskID: taskID, Attempt: cp.Attempt, Agent: cp.Agent, OccurrenceRevision: cp.SubmissionRevision, DispatchID: cp.SubmissionDispatchID})
	ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: cp.SourceRunID, TaskID: taskID, Attempt: cp.Attempt, AgentName: cp.Agent, AgentRole: "worker", ModelExecutionID: cp.ModelExecutionID})
	manifest, err := collectionContextManifest(item, cp, &work.Receipt)
	if !check("context_manifest", err) {
		return
	}
	if item.DependencyResultView != nil {
		if !check("dependency_view", c.validateSelectedRepairContext(ctx, item, manifest)) {
			return
		}
	}
	if manifest != nil {
		ctx = withInvocationMetadata(ctx, invocationMetadataFromManifest(manifest))
	}
	if denial := c.checkpointDenial(taskID); denial != "" {
		check("checkpoint", errors.New(denial))
		return
	}
	ctx = c.withCollectedWorkArtifactScope(ctx, work)
	if cp.State == ResultCollectionCommitted && a.MaxOutputTokens != 0 {
		check("collector_output_limit", fmt.Errorf("a committed collection has no model-based repair"))
		return
	}
	if cp.State == ResultCollectionCommitted {
		result, err := c.acceptedCollectionResult(ctx, item, cp)
		if err == nil {
			accepted := cloneTodoItem(item)
			accepted.TypedResult = result
			err = c.validateAcceptedDependencyPayloadWithContext(ctx, accepted)
		}
		if check("accepted_result", err) {
			a.Disposition = RecoveryCommitLocal
		}
		return
	}
	// Use the archived work revision and observation namespace, without
	// activating the current submission lease or creating any artifact data.
	ctx = context.WithValue(ctx, resultCollectionAdmissionKey{}, cp)
	if !check("required_artifact_reads", c.validateRequiredArtifactReadSubmission(ctx, taskID)) {
		return
	}
	c.assessRecoveryClaims(ctx, item, a, work, compiled, check)
	receipt := collectionReceipt(item, cp)
	boundCollectionSchemaRecovery(a, receipt, check)
	if (a.AdditionalTurns > 0 || a.Disposition == RecoveryRepairClaims) && hasSourceFieldRecollectionGrant(receipt, cp) {
		check("collection_turn_grant", fmt.Errorf("the sealed work has already received its source-field recollection budget"))
		a.Disposition, a.AdditionalTurns = RecoveryBlocked, 0
		return
	}
	if a.MaxOutputTokens != 0 && a.Disposition != RecoveryBlocked && !recoveryCanUseCollectorTurn(a) {
		check("collector_output_limit", fmt.Errorf("increasing the limit requires a model-based collection repair"))
		a.Disposition = RecoveryBlocked
	}
}

func boundCollectionSchemaRecovery(a *ResultCollectionRecoveryAssessment, receipt *ExecutionReceipt, check func(string, error) bool) {
	cp := a.Checkpoint
	// A granted correction can lose its response before the ordinary format
	// checkpoint records it. The saved grant still proves what was reserved.
	consumedGrant := slices.ContainsFunc(collectionRepairGrants(receipt), func(grant ResultRepairGrant) bool {
		return grant.Recovery != nil && grant.Recovery.Disposition == RecoveryRepairClaims &&
			grant.Recovery.Checkpoint != nil && grant.Recovery.Checkpoint.Work == cp.Work && cp.TurnsUsed > grant.AfterAttempts
	})
	if !isCollectionSchemaRecovery(a) && (a.Disposition != RecoveryResumeCollection || !consumedGrant) {
		return
	}
	// A correction reserved before an interruption has already consumed its
	// turn even if neither a response nor prepared claims survived.
	a.AdditionalTurns = 0
	if consumedGrant || !hasOriginalCollectionSchemaTurn(a) {
		check(collectionSchemaBudgetCheck, errors.New("the sealed work has no available original schema correction"))
		a.Disposition = RecoveryBlocked
	} else {
		check(collectionSchemaBudgetCheck, nil)
	}
}

func isCollectionSchemaRecovery(a *ResultCollectionRecoveryAssessment) bool {
	return a.Disposition == RecoveryRepairClaims || a.Disposition == RecoveryResumeCollection && a.Checkpoint.LastRejectedTurn > 0
}

func recoveryCanUseCollectorTurn(a *ResultCollectionRecoveryAssessment) bool {
	return a.AdditionalTurns > 0 || a.ValidatorRevision >= resultRecoveryBoundedSchemaRevision && usesRemainingCollectionSchemaTurn(a, collectionSchemaBudgetCheck)
}

func collectionWorkCompleted(receipt *ExecutionReceipt, cancelled bool) bool {
	return receipt != nil && !receipt.FinishedAt.IsZero() && (!cancelled ||
		(receipt.ExitCode != nil && (*receipt.ExitCode == 0 || *receipt.ExitCode == 1 && receipt.WorkStopReason == workStopToolLoop)))
}

func validateRecoveryAssessment(a *ResultCollectionRecoveryAssessment) error {
	if a == nil || a.Version != 1 || a.ValidatorRevision < 1 || a.ValidatorRevision > resultRecoveryValidatorRevision || a.Checkpoint == nil || a.JournalHeadID == "" || !validArtifactID(a.TranscriptRef) {
		return errResultCollectionStale
	}
	if (a.TaskID != "" && a.TaskID != a.Checkpoint.TaskID) || (a.BranchID != "" && a.BranchID != a.Checkpoint.BranchID) || (a.SourceRunID != "" && a.SourceRunID != a.Checkpoint.SourceRunID) {
		return errResultCollectionStale
	}
	if err := validateResultCollectionCheckpoint(a.Checkpoint); err != nil {
		return err
	}
	if !supportsCollectionOriginRevision(a.Checkpoint, a.ValidatorRevision) {
		return errResultCollectionStale
	}
	digest, err := collectionDigest(a.Checkpoint)
	if err != nil || digest != a.CheckpointSHA256 {
		return errResultCollectionStale
	}
	hash, err := recoveryAssessmentFingerprint(a)
	if err != nil || hash != a.Fingerprint || a.TurnsUsed != a.Checkpoint.TurnsUsed || a.RemainingTurns != max(0, a.Checkpoint.TurnLimit-a.TurnsUsed) {
		return errResultCollectionStale
	}
	if a.MaxOutputTokens != 0 && (a.ValidatorRevision < 15 || a.MaxOutputTokens < 1 ||
		a.MaxOutputTokens > maxResultRepairOutputTokens || !recoveryCanUseCollectorTurn(a)) {
		return errResultCollectionStale
	}
	if err := validateRecoveryDisposition(a); err != nil {
		return err
	}
	return validateRecoveryProofChecks(a)
}

func validateRecoveryDisposition(a *ResultCollectionRecoveryAssessment) error {
	if a.ValidatorRevision >= resultRecoveryBoundedSchemaRevision && isCollectionSchemaRecovery(a) &&
		!usesRemainingCollectionSchemaTurn(a, collectionSchemaBudgetCheck) {
		return errResultCollectionStale
	}
	switch a.Disposition {
	case RecoverySelectReferences:
		if !validAcceptedReferenceAssessment(a) {
			return errResultCollectionStale
		}
	case RecoveryCommitLocal:
		if a.AdditionalTurns != 0 {
			return errResultCollectionStale
		}
	case RecoveryRepairClaims:
		if a.ValidatorRevision < resultRecoveryBoundedSchemaRevision && a.AdditionalTurns != 2 &&
			!usesRemainingCollectionSchemaTurn(a, "normalization_schema_budget") {
			return errResultCollectionStale
		}
	case RecoveryResumeCollection:
		if a.AdditionalTurns != 0 && a.AdditionalTurns != 2 {
			return errResultCollectionStale
		}
		if a.ValidatorRevision >= 6 && a.Checkpoint.Response == nil && a.Checkpoint.Prepared == nil && a.RemainingTurns+a.AdditionalTurns == 0 {
			return errResultCollectionStale
		}
	default:
		return errResultCollectionBlocked
	}
	return nil
}

func usesRemainingCollectionSchemaTurn(a *ResultCollectionRecoveryAssessment, proof string) bool {
	remaining := a.TurnsUsed < resultCollectionDefaultTurns && a.RemainingTurns > 0
	if a.ValidatorRevision >= resultRecoveryBoundedSchemaRevision {
		remaining = hasOriginalCollectionSchemaTurn(a)
	}
	return a.AdditionalTurns == 0 && remaining &&
		slices.ContainsFunc(a.Checks, func(check ResultCollectionRecoveryCheck) bool {
			return check.Code == proof && check.Passed
		})
}

func hasOriginalCollectionSchemaTurn(a *ResultCollectionRecoveryAssessment) bool {
	cp := a.Checkpoint
	return cp.TurnsUsed == resultCollectionDefaultTurns-1 && cp.TurnLimit == resultCollectionDefaultTurns
}

// AssessResumeResultCollection uses a checked source projection and a read-only
// journal. It never activates the branch or restores a live occurrence lease.
// The receiver must be a fresh dry-run coordinator, not an executing runtime.
func (c *Coordinator) AssessResumeResultCollection(ctx context.Context, source *ResumeSource, taskID string) (*ResultCollectionRecoveryAssessment, error) {
	if c.eventStore != nil || c.executionRunID != "" || source == nil || source.Session == nil {
		return nil, fmt.Errorf("recovery preview requires a fresh read-only coordinator and source")
	}
	// Ordinary startup keeps and compares an already active checkpoint. A
	// result-only preview must not promise admission using only its replay.
	if err := validateResumeCheckpointProjection(source); err != nil {
		return nil, err
	}
	if err := c.prepareRecoveryValidation(); err != nil {
		return nil, err
	}
	store, err := OpenEventStoreReadOnly(c.session.Workspace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()
	restore := c.installResumeValidationSource(source, store)
	defer restore()
	if err := c.validateResumeRecoveryMode(WithResultOnlyRepair(ctx, taskID), source); err != nil {
		return nil, err
	}
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
	if a.Disposition != RecoveryBlocked {
		if admissionErr := c.validateResultRepairGrant(context.WithValue(ctx, referenceProjectionPreviewKey{}, true), c.todoItemByID(taskID), TaskProtocolIncomplete, c.collectionResultRepairGrant(a)); admissionErr != nil {
			a.Disposition, a.AdditionalTurns, a.ContinuesWorkflow = RecoveryBlocked, 0, false
			a.Checks = append(a.Checks, ResultCollectionRecoveryCheck{Code: "grant_admission", Diagnostic: schemaRepairDiagnostic(admissionErr.Error())})
			a.Fingerprint, err = recoveryAssessmentFingerprint(a)
		}
	}
	return a, err
}

// Historical grants validate against their saved revision and checkpoint,
// never by running today's evaluator on today's source results.
func validateCollectionRecoveryGrantEvent(event RunEvent, grant *ResultRepairGrant, receipt *ExecutionReceipt) error {
	a := grant.Recovery
	if a != nil && a.Disposition != RecoverySelectReferences && grant.ReferenceProjection != nil {
		return errResultCollectionStale
	}
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
	case RecoverySelectReferences:
		if err := validateAcceptedReferenceProjection(grant, next); err != nil {
			return err
		}
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
	sourceFieldGrants := make(map[[2]string]bool)
	for index, event := range events {
		if event.Type == string(EventContractRecovery) {
			for _, item := range ReduceToTodoList(events[:index+1]) {
				if cp := currentResultCollection(item); cp != nil {
					checkpoints[item.ID] = cp
				}
			}
			continue
		}
		if event.Type == string(EventTaskResultCollectionCheckpointed) {
			payload, err := decodeResultCollectionEvent(event)
			if err != nil {
				return fmt.Errorf("replay collection checkpoint for task %s: %w", event.TaskID, err)
			}
			if err := validateMigratedCollectionProgress(checkpoints[event.TaskID], payload.Receipt.ResultCollection); err != nil {
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
			key := [2]string{event.BranchID, prior.Work.ArtifactID}
			if sourceFieldGrants[key] && payload.Grant.AdditionalAttempts > 0 {
				return fmt.Errorf("additional recollection turns after a source-field grant at %s", event.ID)
			}
			if isCollectionSourceFieldRecollection(payload.Grant.Recovery) {
				sourceFieldGrants[key] = true
			}
		}
		if payload.Receipt != nil && payload.Receipt.ResultCollection != nil {
			if err := validateMigratedCollectionProgress(checkpoints[event.TaskID], payload.Receipt.ResultCollection); err != nil {
				return err
			}
			checkpoints[event.TaskID] = payload.Receipt.ResultCollection
		}
	}
	return nil
}

func validateMigratedCollectionProgress(prior, next *ResultCollectionCheckpoint) error {
	if next == nil || next.ContractUpgrade == nil && next.LegacySubmission == nil && (prior == nil || prior.ContractUpgrade == nil && prior.LegacySubmission == nil) {
		return nil
	}
	if prior != nil && prior.LegacySubmission == nil && next.ContractUpgrade == nil && next.LegacySubmission == nil && (next.Attempt != prior.Attempt || next.ModelExecutionID != prior.ModelExecutionID) {
		return nil // A separately admitted work attempt has its own ordinary binding.
	}
	if prior == nil || !sameCanonicalEvidence(prior.ContractUpgrade, next.ContractUpgrade) || prior != nil && !sameCanonicalEvidence(prior.LegacySubmission, next.LegacySubmission) {
		return errResultCollectionStale
	}
	before, err := collectionBindingDigest(prior)
	after, nextErr := collectionBindingDigest(next)
	if err != nil || nextErr != nil || before != after || prior.Work != next.Work || next.TurnsUsed < prior.TurnsUsed {
		return errResultCollectionStale
	}
	return nil
}

func validateStoredRecoveryGrant(grant ResultRepairGrant, checkpoint *ResultCollectionCheckpoint) error {
	a := grant.Recovery
	if a != nil && a.Disposition != RecoverySelectReferences && grant.ReferenceProjection != nil {
		return errResultCollectionStale
	}
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

func (c *Coordinator) assessRecoveryLineage(ctx context.Context, item *TodoItem, a *ResultCollectionRecoveryAssessment, check func(string, error) bool, validateTarget func(*TodoItem) error) bool {
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
	if !check("canonical_target", validateTarget(canonical)) {
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
	if !sameCanonicalEvidence(collectionRepairGrants(canonical.ExecutionReceipt), collectionRepairGrants(item.ExecutionReceipt)) {
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
