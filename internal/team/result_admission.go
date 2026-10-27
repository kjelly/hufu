package team

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
)

// localResultAdmission owns validation and occurrence commit for local worker
// claims. Transport adapters decode their input and map these typed rejections;
// admission never executes a tool or marks the task done.
type localResultAdmission struct {
	coordinator *Coordinator
	todoID      string
	sink        TaskResultSink
}

type localResultAdmissionError struct {
	reason RepairFailureReason
	cause  error
}

func (e *localResultAdmissionError) Error() string { return e.cause.Error() }
func (e *localResultAdmissionError) Unwrap() error { return e.cause }

func rejectLocalResult(reason RepairFailureReason, err error) error {
	return &localResultAdmissionError{reason: reason, cause: err}
}

func localResultRejectionReason(err error) RepairFailureReason {
	if rejection, ok := errors.AsType[*localResultAdmissionError](err); ok {
		return rejection.reason
	}
	return RepairFailureContract
}

func rejectLocalResultPersistence(err error) error {
	reason := RepairFailureCommit
	if errors.Is(err, errTaskResultStale) {
		reason = RepairFailureSource
	}
	return rejectLocalResult(reason, err)
}

// admit consumes schema-decoded claims. The optional classifier distinguishes a
// model-facing payload shape error from failure to bind trusted source data.
// It cannot grant authority or replace the canonical payload validator.
func (a *localResultAdmission) admit(ctx context.Context, input SubmitResultInput, contract taskResultSubmissionContract, classifyPayloadRejection func() RepairFailureReason) (admissionErr error) {
	reason := RepairFailureInvalidSchema
	defer func() {
		if admissionErr != nil {
			if _, classified := errors.AsType[*localResultAdmissionError](admissionErr); !classified {
				admissionErr = rejectLocalResult(reason, admissionErr)
			}
		}
	}()
	res := input.taskResult()
	if res.Summary == "" {
		return errors.New("summary is required")
	}
	switch strings.ToLower(strings.TrimSpace(res.Status)) {
	case TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps, TaskResultStatusPartial, TaskResultStatusFailed, TaskResultStatusBlocked:
		res.Status = strings.ToLower(strings.TrimSpace(res.Status))
	default:
		return errors.New("status must be success, completed_with_gaps, partial, failed, or blocked")
	}
	reason = RepairFailureContract
	if decision, blocked := a.coordinator.decisionBlock(ctx); blocked && res.Status != TaskResultStatusBlocked {
		return errors.New(decisionBlockResultDenial(decision, res.Status))
	}
	if err := contract.validateWorkerClaims(&res); err != nil {
		return errors.New("submit_result contract violation: " + err.Error())
	}
	if err := validateFindingSeverities(res.Findings); err != nil {
		return errors.New("submit_result contract violation: " + err.Error())
	}
	if contract.InvariantVerification == "" && input.InvariantAssessments != nil {
		return errors.New("submit_result contract violation: ordinary task must omit invariant_assessments or submit null")
	}
	if a.coordinator != nil {
		var observations []taskTranscriptRecord
		_, compiled, contractErr := a.coordinator.boundResultContract(a.todoID)
		if contractErr != nil {
			return rejectLocalResult(RepairFailureContract, contractErr)
		}
		if compiled != nil && compiled.toolEvidence != nil {
			var evidenceErr error
			observations, evidenceErr = submissionToolEvidence(ctx, a.coordinator, a.todoID)
			if evidenceErr != nil {
				return rejectLocalResult(RepairFailureSource, evidenceErr)
			}
		}
		if res.Status != TaskResultStatusBlocked || len(bytes.TrimSpace(input.StructuredPayload)) != 0 || !hasResultDecisions(compiled) {
			payload, rejection := a.coordinator.structuredPayloadForSubmission(a.todoID, input.StructuredPayload, observations)
			if rejection != "" {
				payloadReason := RepairFailureSource
				if classifyPayloadRejection != nil {
					payloadReason = classifyPayloadRejection()
				}
				return rejectLocalResult(payloadReason, errors.New(rejection))
			}
			res.StructuredPayload = payload
			if payload != nil && payload.EvidenceDowngrades > 0 {
				res.Summary = "Result includes claims without matching successful tool evidence; consult the structured payload for downgraded findings."
			}
		}
	} else if len(bytes.TrimSpace(input.StructuredPayload)) > 0 {
		return errors.New("structured_payload is not accepted for this task because it has no result contract; omit the field")
	}

	if protocolRepairExecution(ctx) && len(res.Artifacts) > 0 {
		return errors.New("protocol repair cannot add artifact evidence; submit the corrected result without artifacts")
	}
	if len(res.Outputs) > 0 {
		return errors.New("outputs are runtime-owned; cite execution receipt_ids instead of declaring task outputs")
	}
	if res.RawOutputRef != nil {
		return errors.New("raw_output_ref is runtime-owned; workers cannot declare or copy transcript references")
	}
	if a.forbidsArtifacts() && len(res.Artifacts) > 0 {
		return errors.New("artifacts are forbidden by this task's execution contract; omit the artifacts field and submit the result again")
	}
	// A model-authored failure summary is useful context, but it is not the
	// canonical execution record. Prefix non-success results with the first
	// runtime-observed closed-sequence failure so downstream errors cannot
	// misidentify which slot or exit status actually failed.
	if !taskResultStatusIsSuccessful(res.Status) {
		if fact := taskToolSequenceFromContext(ctx).failureSummary(); fact != "" {
			workerSummary := strings.TrimSpace(res.Summary)
			res.Summary = fact
			if workerSummary != "" {
				res.Summary += ". Worker report: " + workerSummary
			}
			res.Findings = append(res.Findings, Finding{
				Category: "runtime_execution",
				Summary:  fact,
				Detail:   "Runtime-owned closed-sequence evidence; consult the sealed transcript for tool output.",
			})
		}
	}
	// Security: strip model-injected HMAC signatures from submit_result tool input
	for i := range res.Evidence {
		res.Evidence[i].SystemHMAC = ""
	}
	return a.commit(ctx, input, &res, contract)
}

// commit reserves the occurrence before materializing artifacts and publishing
// canonical results. Only the coordinator's existing lifecycle may complete it.
func (a *localResultAdmission) commit(ctx context.Context, input SubmitResultInput, res *TaskResult, contract taskResultSubmissionContract) error {
	sink := a.sink
	if sink == nil && a.coordinator != nil {
		sink = coordinatorTaskResultSink{coordinator: a.coordinator}
	}
	if a.coordinator != nil {
		identity, err := submitResultRuntimeIdentityFromContext(ctx, a.coordinator, a.todoID)
		if err != nil {
			return rejectLocalResult(RepairFailureSource, fmt.Errorf("invalid submit_result runtime identity: %w", err))
		}
		// Receipt admission still needs the active occurrence, but it must run
		// before the transaction mutex is held because that validator reads the
		// occurrence through the public snapshot helper. The attempt is runtime
		// supplied here, never worker-authored.
		receiptCandidate := *res
		receiptCandidate.Attempt = identity.Attempt
		if err := a.coordinator.validateTaskResultReceiptClaims(a.todoID, &receiptCandidate); err != nil {
			return rejectLocalResult(RepairFailureSource, err)
		}
		metadata, _ := invocationMetadataFromContext(ctx)
		if err := a.coordinator.attestInvariantClaims(a.todoID, identity.Attempt, metadata.ModelExecutionID, input.InvariantAssessments, res); err != nil {
			return errors.New("invalid invariant assessment: " + err.Error())
		}
		a.coordinator.attachTaskKnowledgeCoverage(a.todoID, identity.Attempt, metadata.ModelExecutionID, res)
		tx, txErr := a.coordinator.beginTaskResultSubmission(identity)
		if txErr != nil {
			return rejectLocalResultPersistence(fmt.Errorf("store submitted result: %w", txErr))
		}
		if base, ok := ctx.Value(resultPatchBaseKey{}).(string); ok {
			current := a.coordinator.todoItemByID(a.todoID)
			if current == nil || current.ResultCandidate == nil || current.ResultCandidate.SHA256 != base {
				_ = tx.rollback()
				return rejectLocalResult(RepairFailureSource, errors.New("candidate_stale"))
			}
		}
		// Runtime ownership begins only after the occurrence is reserved. The
		// candidate now contains the values that worker-independent assertions
		// are allowed to inspect.
		res.TaskID = identity.TaskID
		res.Attempt = identity.Attempt
		res.Agent = identity.Agent
		res.Source = "submitted"
		if _, collecting := ctx.Value(resultCollectionAdmissionKey{}).(*ResultCollectionCheckpoint); collecting {
			res.Source = "collected"
		}
		if res.Confidence == 0 {
			res.Confidence = 1.0
		}
		rollback := func(prefix string) error {
			if rollbackErr := tx.rollback(); rollbackErr != nil {
				prefix += "; rollback: " + rollbackErr.Error()
			}
			return errors.New(prefix)
		}
		if err := a.coordinator.validateMemoryUseClaims(ctx, a.todoID, res); err != nil {
			return rollback(err.Error())
		}
		// Only a success claim can be mechanically contradicted by terminal
		// evidence. Check it while the occurrence is reserved so no projection
		// can be published before the result is accepted.
		if res.Status == "success" {
			if err := a.coordinator.terminalTaskFailure(ctx, a.todoID); err != nil {
				return rollback("success rejected: " + err.Error())
			}
		}
		if err := a.materializeSubmittedArtifacts(ctx, res, tx); err != nil {
			return rollback("invalid artifact declaration: " + err.Error())
		}
		if err := contract.validateFinalizableResult(res); err != nil {
			return rollback("submit_result contract violation: " + err.Error())
		}
		// A reported assumption check is one of the three sources allowed to
		// change an assumption's status (spec §18.1). Validate it while the
		// occurrence is reserved: a check naming an assumption the decision
		// never declared is a contract violation, not a warning.
		if _, err := a.coordinator.ApplyAssumptionChecks(ctx, a.todoID, res.AssumptionChecks, AssumptionSourceTaskResult); err != nil {
			return rollback("invalid assumption check: " + err.Error())
		}
		// A reported check can invalidate the governing decision.  That is a
		// lifecycle outcome, not merely a later tool denial: reject this same
		// submit_result while its occurrence is still reserved so no result cache,
		// TaskDone projection, or success event can be published.
		if denial := a.coordinator.checkpointDenial(a.todoID); denial != "" {
			return rollback("submit_result rejected by checkpoint: " + denial)
		}
		if _, isCoordinatorSink := sink.(coordinatorTaskResultSink); isCoordinatorSink {
			// The coordinator sink is represented by this transaction; invoking it
			// again would attempt a second reservation while this gate is held.
		} else if sink != nil {
			if err := sink.Submit(ctx, a.todoID, *res); err != nil {
				return rejectLocalResult(RepairFailureCommit, rollback("store submitted result: "+err.Error()))
			}
		}
		var commitErr error
		if checkpoint, ok := ctx.Value(resultCollectionAdmissionKey{}).(*ResultCollectionCheckpoint); ok {
			commitErr = tx.commitCollection(ctx, res, checkpoint)
		} else {
			commitErr = tx.commit(res)
		}
		if err := commitErr; err != nil {
			return rejectLocalResult(RepairFailureCommit, rollback("store submitted result: "+err.Error()))
		}
		if stop, ok := ctx.Value(acceptedTerminalResultStopKey{}).(*acceptedTerminalResultStop); ok {
			stop.markAccepted(identity)
		}
		a.coordinator.emitMemoryUsageEvents(res)
		tx.finish()
	} else if sink != nil {
		res.TaskID = a.todoID
		res.Source = "submitted"
		if res.Confidence == 0 {
			res.Confidence = 1.0
		}
		if err := contract.validateFinalizableResult(res); err != nil {
			return errors.New("submit_result contract violation: " + err.Error())
		}
		if err := sink.Submit(ctx, a.todoID, *res); err != nil {
			return rejectLocalResult(RepairFailureCommit, errors.New("store submitted result: "+err.Error()))
		}
	}
	return nil
}

func (a *localResultAdmission) forbidsArtifacts() bool {
	if a == nil || a.coordinator == nil || a.coordinator.taskTracker == nil || a.coordinator.taskTracker.TodoList() == nil {
		return false
	}
	for _, item := range a.coordinator.taskTracker.TodoList().Items() {
		if item != nil && item.ID == a.todoID {
			return item.Execution.ForbidArtifacts
		}
	}
	return false
}

// materializeSubmittedArtifacts makes a worker's artifact claim durable before
// accepting its terminal result. Previously a model could declare a missing
// path, receive a successful submit_result response, and leave the evidence
// manifest to fail only at run finalization. Artifacts are workspace-contained
// evidence, so snapshot them here or reject the result while the worker can
// still correct its claim.
func (a *localResultAdmission) materializeSubmittedArtifacts(ctx context.Context, res *TaskResult, tx *taskResultOccurrenceTransaction) error {
	if res == nil || len(res.Artifacts) == 0 {
		return nil
	}
	if a.coordinator == nil || a.coordinator.session == nil || strings.TrimSpace(a.coordinator.session.Workspace) == "" {
		return fmt.Errorf("artifact submission requires a workspace")
	}
	workspace := a.coordinator.session.Workspace
	storeRoot := workspace
	if checkpoint, _ := ctx.Value(resultCollectionAdmissionKey{}).(*ResultCollectionCheckpoint); checkpoint != nil && checkpoint.FanoutLeaf {
		// The source directory belongs to this leaf, while accepted bytes
		// must outlive its disposable workspace in the parent's durable CAS.
		storeRoot = a.coordinator.artifactStoreRootPath()
	}
	store, err := NewFileArtifactStore(storeRoot, workspace)
	if err != nil {
		return err
	}
	identity, err := submitResultRuntimeIdentityFromContext(ctx, a.coordinator, a.todoID)
	if err != nil {
		return err
	}
	if res.TaskID != identity.TaskID || res.Attempt != identity.Attempt || res.Agent != identity.Agent {
		return fmt.Errorf("result provenance does not match runtime occurrence")
	}
	if err := validateSubmittedArtifactClaims(ctx, workspace, res.Artifacts); err != nil {
		return err
	}
	for i, artifact := range res.Artifacts {
		ref, err := putSubmittedArtifact(ctx, store, identity, artifact)
		if err != nil {
			if tx != nil {
				tx.addMaterialized(nil)
			}
			return fmt.Errorf("artifacts[%d] %q: %w", i, artifact.Path, err)
		}
		res.Artifacts[i] = ref
	}
	if tx != nil {
		tx.addMaterialized(res.Artifacts)
	}
	return nil
}
