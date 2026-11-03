package team

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"slices"

	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/tools"
)

type readOnlyWorkRetryKey struct{}

// WithReadOnlyWorkRetry authorizes assessment of one original read-only work
// occurrence. Publication still requires the exact preview hash and a child
// branch; it never authorizes a result-only collector to execute tools.
func WithReadOnlyWorkRetry(ctx context.Context, taskID string) context.Context {
	return context.WithValue(ctx, readOnlyWorkRetryKey{}, taskID)
}

func readOnlyWorkRetryRequested(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(readOnlyWorkRetryKey{}).(string)
	return id, ok
}

// ReadOnlyWorkRetry is runtime-authored evidence, bound by the recovery plan
// hash. The pinned schema lets replay reconstruct missing slots from original
// accepted source arrays without consulting today's team files or outputs.
type ReadOnlyWorkRetry struct {
	Version          int                             `json:"version"`
	TaskID           string                          `json:"task_id"`
	Checkpoint       *ResultCollectionCheckpoint     `json:"checkpoint"`
	CheckpointSHA256 string                          `json:"checkpoint_sha256"`
	Schema           jsontext.Value                  `json:"schema"`
	MissingDecisions []resultDecisionBinding         `json:"missing_decisions"`
	Checks           []ResultCollectionRecoveryCheck `json:"checks"`
}

func (c *Coordinator) assessReadOnlyWorkRetry(ctx context.Context, source *ResumeSource, taskID string) (*ReadOnlyWorkRetry, error) {
	if err := c.validateReadOnlyRecoveryContracts(source.Session, "read-only work retry"); err != nil {
		return nil, err
	}
	store, err := OpenEventStoreReadOnly(c.session.Workspace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()
	restore := c.installResumeValidationSource(source, store)
	defer restore()
	item := c.todoItemByID(taskID)
	a := &ResultCollectionRecoveryAssessment{TaskID: taskID, BranchID: source.BranchID}
	var checks []ResultCollectionRecoveryCheck
	var sourceErr error
	check := func(code string, err error) bool {
		if err != nil {
			sourceErr = fmt.Errorf("work retry rejected at %s: %w", code, err)
			return false
		}
		checks = append(checks, ResultCollectionRecoveryCheck{Code: code, Passed: true})
		return true
	}
	validateTarget := func(item *TodoItem) error { return validateWorkRetryTarget(item, currentResultCollection(item)) }
	if !check("target", validateTarget(item)) {
		return nil, sourceErr
	}
	a.Checkpoint = currentResultCollection(item)
	a.CheckpointSHA256, _ = collectionDigest(a.Checkpoint)
	a.SourceRunID, a.TranscriptRef = item.ExecutionReceipt.RunID, item.ExecutionReceipt.TranscriptRef
	if !c.assessRecoveryLineage(ctx, item, a, check, validateTarget) {
		return nil, sourceErr
	}
	if a.JournalHeadID != source.eventHead {
		return nil, ErrStaleResumeSource
	}
	_, err = blockedCollectionWorkflowPhase(source.Session, taskID, validateTarget)
	if !check("workflow_recovery", err) {
		return nil, sourceErr
	}
	if _, active := c.activeTaskResultOccurrence(taskID); active {
		return nil, errTaskResultStale
	}
	checks = append(checks, ResultCollectionRecoveryCheck{Code: "execution_ended", Passed: true})
	snapshot := contractRecoveryInputSnapshot(item, source.Session)
	if snapshot == nil || ValidateRunInputSnapshot(snapshot) != nil || snapshot.ID != a.Checkpoint.InputsID || snapshot.SnapshotHash != a.Checkpoint.InputsSHA256 {
		return nil, fmt.Errorf("work retry requires the original canonical frozen inputs")
	}
	work, err := c.loadCollectionWork(ctx, item, a.Checkpoint)
	if err != nil {
		return nil, err
	}
	if !collectionWorkCompleted(&work.Receipt, true) || len(work.Receipt.ToolDispositions) != 0 {
		return nil, fmt.Errorf("work retry requires completed successful work without tool policy denials")
	}
	checks = append(checks, ResultCollectionRecoveryCheck{Code: "work_sources_policy", Passed: true})
	if a.Checkpoint.State == ResultCollectionCommitted {
		accepted, err := c.acceptedCollectionResult(ctx, item, a.Checkpoint)
		if err != nil {
			return nil, err
		}
		if !sameCanonicalEvidence(accepted, item.TypedResult) {
			return nil, fmt.Errorf("blocked collection result changed")
		}
	}
	for _, blob := range []struct {
		kind string
		ref  *ResultCollectionRef
	}{{"request", a.Checkpoint.Request}, {"response", a.Checkpoint.Response}, {"claims", a.Checkpoint.Prepared}} {
		if blob.ref != nil {
			if _, err := c.readCollectionBlob(ctx, a.Checkpoint, blob.kind, blob.ref); err != nil {
				return nil, err
			}
		}
	}
	evidence, err := c.restoreTranscriptEvidence(ctx, &work.Receipt, a.SourceRunID)
	if err != nil {
		return nil, err
	}
	if err := validateRecoveryTerminalContext(c.session.Workspace, a.Checkpoint); err != nil {
		return nil, err
	}
	manifest, err := collectionContextManifest(item, a.Checkpoint, &work.Receipt)
	if err != nil {
		return nil, err
	}
	if item.DependencyResultView != nil || item.HistoricalResultBinding != nil {
		cp := a.Checkpoint
		claimsCtx := context.WithValue(ctx, resultCollectionEvidenceKey{}, evidence)
		claimsCtx = context.WithValue(claimsCtx, tools.AgentNameKey, cp.Agent)
		claimsCtx = withInvocationMetadata(claimsCtx, InvocationMetadata{RunID: cp.SourceRunID, TaskID: taskID, Attempt: cp.Attempt, AgentName: cp.Agent, AgentRole: "worker", ModelExecutionID: cp.ModelExecutionID})
		if err := c.validateSelectedRepairContext(claimsCtx, item, manifest); err != nil {
			return nil, err
		}
	}
	if denial := c.checkpointDenial(taskID); denial != "" {
		return nil, fmt.Errorf("work retry checkpoint denial: %s", denial)
	}
	_, compiled, err := c.boundResultContract(taskID)
	if err != nil {
		return nil, err
	}
	if !hasResultDecisions(compiled) {
		return nil, fmt.Errorf("work retry requires a bound decision contract")
	}
	if len(compiled.CanonicalSchema) > resultContractPromptSchemaMaxBytes {
		return nil, fmt.Errorf("work retry decision schema exceeds the recovery proof limit")
	}
	events, err := c.readActiveLineageEvents(ctx)
	if err != nil {
		return nil, err
	}
	plans, err := c.fixedCollectionPlans(taskID)
	if err != nil {
		return nil, err
	}
	missing, err := workRetryMissingDecisions(events, source.BranchID, item, compiled, plans, func(expected resultDecisionBinding, group resultDecisionGroup, payload decisionPrimitivePayload) error {
		return c.validateBoundDecision(ctx, expected, group, payload)
	})
	if err != nil {
		return nil, err
	}
	checks = append(checks, ResultCollectionRecoveryCheck{Code: "contract", Passed: true}, ResultCollectionRecoveryCheck{Code: "work_retry_sources", Passed: true})
	proof := &ReadOnlyWorkRetry{Version: 1, TaskID: taskID, Checkpoint: cloneResultCollectionCheckpoint(a.Checkpoint), CheckpointSHA256: a.CheckpointSHA256,
		Schema: slices.Clone(compiled.CanonicalSchema), MissingDecisions: missing, Checks: checks}
	return proof, nil
}

func validateWorkRetryTarget(item *TodoItem, checkpoint *ResultCollectionCheckpoint) error {
	if item == nil || item.Status != TaskBlocked || item.FailureEvent == nil || !metadataReadOnlyTask(item.SideEffect, item.Execution) ||
		item.CheckpointPause || item.RecoveryState == RecoveryStateUnknown || item.RecoveryState == RecoveryStatePartial ||
		(item.DecisionProfile != "" && item.DecisionProfile != "off") || item.WorkerWorkspace.isolated() || len(item.ExecutionTopology) > 1 {
		return fmt.Errorf("work retry requires one blocked read-only local collection without uncertain effects")
	}
	receipt := item.ExecutionReceipt
	if receipt == nil || checkpoint == nil || receipt.ExecutionWorldID != "" || checkpoint.FanoutLeaf ||
		receipt.Attempt != checkpoint.Attempt || receipt.OccurrenceAttempt > 0 && receipt.OccurrenceAttempt != item.Retries+1 ||
		receipt.FinishedAt.IsZero() || len(receipt.ToolDispositions) != 0 {
		return fmt.Errorf("work retry requires a finished original collection receipt")
	}
	if err := validateResultCollectionCheckpoint(checkpoint); err != nil {
		return err
	}
	switch checkpoint.State {
	case ResultCollectionBlocked:
		if err := validateCollectionRecoveryTarget(item); err != nil {
			return err
		}
		if item.FailureEvent.FailureType != FailureTypeResultCollection || item.TypedResult != nil || checkpoint.Accepted != nil || receipt.ExitCode == nil || *receipt.ExitCode != 0 {
			return fmt.Errorf("work retry requires a failed collection over successful sealed work")
		}
	case ResultCollectionCommitted:
		if item.FailureEvent.FailureClass != FailureExecution || item.TypedResult == nil || item.TypedResult.Status != TaskResultStatusBlocked ||
			item.TypedResult.StructuredPayload != nil || checkpoint.Accepted == nil || receipt.ExitCode == nil || *receipt.ExitCode != 1 {
			return fmt.Errorf("work retry requires a canonical blocked result with missing decisions")
		}
	default:
		return fmt.Errorf("work retry requires a settled blocked collection")
	}
	return nil
}

func workRetryMissingDecisions(events []RunEvent, branch string, item *TodoItem, compiled *CompiledResultContract, plans []resultFixedCollectionPlan,
	validate func(resultDecisionBinding, resultDecisionGroup, decisionPrimitivePayload) error,
) ([]resultDecisionBinding, error) {
	scope := decisionPrimitiveScope(events, branch, InvocationMetadata{RunID: item.ExecutionReceipt.RunID, TaskID: item.ID, AgentName: item.Agent})
	var missing []resultDecisionBinding
	for _, plan := range plans {
		for _, row := range plan.rows {
			for _, group := range plan.spec.DecisionGroups {
				expected := newResultDecisionBinding(scope, compiled, plan, row, group)
				payload, _, started, err := findBoundDecision(events, expected.Slot)
				if err != nil {
					return nil, err
				}
				if payload == nil {
					if started {
						return nil, fmt.Errorf("work retry refuses an interrupted decision; reconcile its original inference first")
					}
					missing = append(missing, expected)
					if len(missing) > 64 {
						return nil, fmt.Errorf("work retry exceeds 64 missing decision slots; split the original task")
					}
					continue
				}
				if payload.Binding == nil || !sameResultDecisionBinding(expected, *payload.Binding) || payload.Name != group.CatalogEntry || payload.ErrorCode != "" || payload.Result == nil || payload.Result.Status != decisionrt.StatusDecided {
					return nil, fmt.Errorf("work retry refuses an invalid or unsettled original decision")
				}
				if validate != nil {
					if err := validate(expected, group, *payload); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	if len(missing) == 0 {
		return nil, fmt.Errorf("work retry has no missing required decisions")
	}
	return missing, nil
}

// Explicit retry scope includes only the selected blocked task and its
// transitive consumers. Independent accepted work must remain retained.
func validateWorkRetryScope(tasks []*TodoItem, plan *ContractRecoveryPlan) error {
	if plan.WorkRetry == nil {
		return fmt.Errorf("work retry proof is missing")
	}
	selected := plan.WorkRetry.TaskID
	unresolved := UnresolvedTaskReferences(tasks)
	if len(unresolved) != 1 || unresolved[0].ID != selected {
		return fmt.Errorf("work retry requires the selected task to be the sole unresolved task")
	}
	closure := map[string]bool{selected: true}
	for changed := true; changed; {
		changed = false
		for _, item := range tasks {
			if item == nil || IsPrimaryOccurrence(item) || closure[item.ID] {
				continue
			}
			if slices.ContainsFunc(resultDependencyIDs(item), func(id string) bool { return closure[id] }) {
				closure[item.ID], changed = true, true
			}
		}
	}
	for _, patch := range plan.Tasks {
		item := todoItemByID(tasks, patch.ID)
		if item == nil || !metadataReadOnlyTask(item.SideEffect, item.Execution) || (patch.Action == "retry") != closure[patch.ID] || !sameCanonicalEvidence(patch.ResultContract, item.ResultContract) || patch.ContractHash != item.ContractHash {
			return fmt.Errorf("work retry changed an unrelated task or result contract")
		}
	}
	return nil
}

func validateReadOnlyWorkRetry(events []RunEvent, plan *ContractRecoveryPlan) error {
	proof := plan.WorkRetry
	if proof == nil || proof.Version != 1 {
		return fmt.Errorf("unsupported work retry proof")
	}
	session := ReduceToSessionData(events)
	item := todoItemByID(session.Tasks, proof.TaskID)
	if err := validateWorkRetryTarget(item, proof.Checkpoint); err != nil {
		return err
	}
	if err := validateWorkRetryScope(session.Tasks, plan); err != nil {
		return err
	}
	if _, err := blockedCollectionWorkflowPhase(session, proof.TaskID, func(item *TodoItem) error { return validateWorkRetryTarget(item, currentResultCollection(item)) }); err != nil {
		return err
	}
	digest, err := collectionDigest(currentResultCollection(item))
	if err != nil || digest != proof.CheckpointSHA256 || !sameCanonicalEvidence(currentResultCollection(item), proof.Checkpoint) || proof.Checkpoint.SourceRunID != item.ExecutionReceipt.RunID || proof.Checkpoint.BranchID != plan.SourceBranch {
		return fmt.Errorf("work retry checkpoint changed")
	}
	snapshot := contractRecoveryInputSnapshot(item, session)
	if snapshot == nil || ValidateRunInputSnapshot(snapshot) != nil || snapshot.SnapshotHash != item.RunInputSnapshotHash || snapshot.ID != proof.Checkpoint.InputsID || snapshot.SnapshotHash != proof.Checkpoint.InputsSHA256 {
		return fmt.Errorf("work retry frozen inputs changed")
	}
	required := map[string]bool{"target": false, "lineage": false, "canonical_replay": false, "canonical_target": false, "workflow_recovery": false, "work_sources_policy": false, "execution_ended": false, "contract": false, "frozen_inputs": false, "work_retry_sources": false}
	for _, check := range proof.Checks {
		if !check.Passed {
			return fmt.Errorf("work retry contains a failed proof check")
		}
		if _, ok := required[check.Code]; ok {
			required[check.Code] = true
		}
	}
	for name, passed := range required {
		if !passed {
			return fmt.Errorf("work retry lacks proof check %s", name)
		}
	}
	if item.ResultContract == nil || len(proof.Schema) > resultContractPromptSchemaMaxBytes {
		return fmt.Errorf("work retry has no bounded original decision schema")
	}
	compiled, err := CompileResultContractDocument(item.ResultContract.ID, proof.Schema)
	if err != nil || compiled.SchemaSHA256 != item.ResultContract.SchemaSHA256 || !hasResultDecisions(compiled) {
		return fmt.Errorf("work retry decision schema changed")
	}
	var results []TaskResult
	dependencies := resultDependencyIDs(item)
	for _, source := range session.Tasks {
		if source == nil || !slices.Contains(dependencies, source.ID) {
			continue
		}
		if source.Status != TaskDone || source.TypedResult == nil || !taskResultStatusIsSuccessful(source.TypedResult.Status) {
			return fmt.Errorf("work retry accepted source missing")
		}
		results = append(results, *source.TypedResult)
	}
	plans, err := buildResultFixedCollections(compiled.fixedCollections, item.EvidenceFrom, results)
	if err != nil {
		return err
	}
	missing, err := workRetryMissingDecisions(events, plan.SourceBranch, item, compiled, plans, nil)
	if err != nil {
		return err
	}
	if !sameCanonicalEvidence(missing, proof.MissingDecisions) {
		return fmt.Errorf("work retry missing decision binding changed")
	}
	return nil
}
