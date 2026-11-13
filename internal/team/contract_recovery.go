package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"time"
)

const EventContractRecovery EventType = "contract_recovery_created"

type ContractRevalidation struct {
	Original         ResultContractRef `json:"original"`
	Accepted         ResultContractRef `json:"accepted"`
	PayloadSHA256    string            `json:"payload_sha256"`
	SourceTaskSHA256 string            `json:"source_task_sha256"`
}

type ContractRecoveryTask struct {
	PayloadReconstruction *ActionPayloadReconstruction     `json:"action_payload_reconstruction,omitempty"`
	LegacySubmission      *LegacySubmissionRecovery        `json:"legacy_submission,omitempty"`
	Collection            *ResultCollectionContractUpgrade `json:"collection,omitempty"`
	Reason                string                           `json:"reason,omitempty"`
	SourceStatus          TaskStatus                       `json:"source_status,omitempty"`
	AffectedBy            []string                         `json:"affected_by,omitempty"`
	ID                    string                           `json:"id"`
	SourceSHA256          string                           `json:"source_sha256"`
	Action                string                           `json:"action"`
	ResultContract        *ResultContractRef               `json:"result_contract,omitempty"`
	ContractHash          string                           `json:"contract_hash"`
	Revalidation          *ContractRevalidation            `json:"revalidation,omitempty"`
}

type ContractRecoveryPlan struct {
	ActionRepair         *ReadOnlyActionRepair    `json:"action_repair,omitempty"`
	Repaired             int                      `json:"repaired,omitzero"`
	WorkRetry            *ReadOnlyWorkRetry       `json:"work_retry,omitempty"`
	ContractChanges      []ContractRecoveryChange `json:"contract_changes,omitempty"`
	ReopenedAcceptance   []string                 `json:"reopened_acceptance,omitempty"`
	ReopenedVerification []string                 `json:"reopened_verification,omitempty"`
	InputPolicyUpgrade   *RunInputPolicyRecovery  `json:"input_policy_upgrade,omitempty"`
	MetadataOnly         bool                     `json:"metadata_only,omitzero"`
	Version              int                      `json:"version"`
	JournalHead          string                   `json:"journal_head,omitempty"`
	JournalHash          string                   `json:"journal_hash,omitempty"`
	SourceBranch         string                   `json:"source_branch"`
	SourceHead           string                   `json:"source_head"`
	SourceHash           string                   `json:"source_hash"`
	SourcePolicy         string                   `json:"source_policy"`
	Policy               *ExecutionPolicySnapshot `json:"policy"`
	Tasks                []ContractRecoveryTask   `json:"tasks"`
	ResumePhase          Phase                    `json:"resume_phase"`
	Retained             int                      `json:"retained"`
	Retried              int                      `json:"retried"`
	SHA256               string                   `json:"sha256"`
}

func recoveryPlanHash(plan ContractRecoveryPlan) (string, error) {
	plan.SHA256 = ""
	return collectionDigest(plan)
}

// AssessContractRecovery is read-only. Result schema/binding changes and an
// explicitly selected frozen-input policy upgrade can be admitted. Models,
// execution routes, tools, input scope and static actions must still match.
func (c *Coordinator) AssessContractRecovery(ctx context.Context, source *ResumeSource) (*ContractRecoveryPlan, error) {
	if err := c.prepareContractRecoveryAssessment(ctx, source); err != nil {
		return nil, err
	}
	return c.assessPreparedContractRecovery(ctx, source)
}

func (c *Coordinator) prepareContractRecoveryAssessment(ctx context.Context, source *ResumeSource) error {
	if c == nil || c.session == nil || source == nil || source.Session == nil || source.BranchID != source.activeBranch || source.workspace != c.session.Workspace {
		return fmt.Errorf("contract recovery requires the active canonical branch")
	}
	if digest, err := collectionDigest(source.Session); err != nil || digest != source.sessionSHA256 {
		return fmt.Errorf("contract recovery source projection changed")
	}
	if actionPayloadReconstructionRequested(ctx) && readOnlyActionRepairID(ctx) == "" {
		return fmt.Errorf("action payload reconstruction requires an explicit read-only Go action repair")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if contractResultRepairID(ctx) != "" && frozenInputPolicyRecoveryRequested(ctx) {
		return fmt.Errorf("result-only contract recovery cannot change input policy")
	}
	if err := validateContractRecoverySessionSafety(ctx, source.Session); err != nil {
		return err
	}
	if metadataContractRecoveryRequested(ctx) {
		if err := c.validateMetadataContractRecovery(ctx, source.Session); err != nil {
			return err
		}
	}
	return c.prepareRecoveryValidation()
}

func (c *Coordinator) assessPreparedContractRecovery(ctx context.Context, source *ResumeSource) (*ContractRecoveryPlan, error) {
	old, current, bindings, err := c.contractRecoveryPolicies(ctx, source)
	if err != nil {
		return nil, err
	}
	plan, err := c.newContractRecoveryPlan(ctx, source, old, current)
	if err != nil {
		return nil, err
	}
	tasks := map[string]TaskDef{}
	for _, task := range c.session.ContractTasks {
		tasks[task.ID] = task
	}
	retry := map[string]bool{}
	changedResults := map[string]bool{}
	for _, item := range source.Session.Tasks {
		if item == nil || IsPrimaryOccurrence(item) {
			continue
		}
		if item.Status == TaskInProgress || item.Status == TaskPaused || item.Status == TaskVerifying {
			return nil, fmt.Errorf("task %s has unsettled work", item.ID)
		}
		patch, needsRetry, err := c.assessContractRecoveryTask(ctx, item, source.Session, tasks, bindings)
		if err != nil {
			return nil, err
		}
		retry[item.ID] = needsRetry
		changedResults[item.ID] = !sameCanonicalEvidence(item.ResultContract, patch.ResultContract)
		plan.Tasks = append(plan.Tasks, patch)
	}
	if err := c.assessContractResultRepair(ctx, source, plan); err != nil {
		return nil, err
	}
	// A downstream result must be regenerated whenever its source is replayed.
	for changed := true; changed; {
		changed = false
		for _, item := range source.Session.Tasks {
			if item == nil || retry[item.ID] {
				continue
			}
			for _, id := range append(slices.Clone(item.DependsOn), item.EvidenceFrom...) {
				if retry[id] || changedResults[id] {
					retry[item.ID] = true
					changed = true
					break
				}
			}
		}
	}
	for i := range plan.Tasks {
		patch := &plan.Tasks[i]
		if retry[patch.ID] {
			item := todoItemByID(source.Session.Tasks, patch.ID)
			if item == nil {
				return nil, fmt.Errorf("contract recovery task %s missing", patch.ID)
			}
			if patch.Collection != nil || patch.LegacySubmission != nil {
				patch.Action, patch.Revalidation = "repair_result", nil
				plan.Repaired++
			} else {
				if item.SideEffect != SideEffectNone {
					return nil, fmt.Errorf("task %s may have side effects; explicit reconciliation is required", item.ID)
				}
				patch.Action, patch.Revalidation = "retry", nil
				plan.Retried++
			}
			if item.Phase != "" && phaseBefore(item.Phase, plan.ResumePhase) {
				plan.ResumePhase = item.Phase
			}
		} else {
			plan.Retained++
		}
	}
	if selectedLegacySubmission(source.Session, contractResultRepairID(ctx)) != nil && plan.Retried != 0 {
		return nil, fmt.Errorf("legacy recovery cannot replay other tasks")
	}
	if plan.Retried+plan.Repaired == 0 {
		return nil, fmt.Errorf("contract recovery has no failed or invalid task to resume")
	}
	if err := validateContractCollectionProjection(source, plan); err != nil {
		return nil, err
	}
	c.describeContractRecovery(plan, source.Session, old, current)
	if plan.WorkRetry != nil {
		if err := validateWorkRetryScope(source.Session.Tasks, plan); err != nil {
			return nil, err
		}
	}
	if err := validateActionPayloadReconstructionProofs(source.lineage, source.Session.Tasks, plan); err != nil {
		return nil, err
	}
	if err := validateActionRepairProof(source.lineage, source.Session.Tasks, plan); err != nil {
		return nil, err
	}
	plan.SHA256, err = recoveryPlanHash(*plan)
	return plan, err
}

func phaseBefore(left, right Phase) bool {
	phases := []Phase{PhaseInit, PhasePrepare, PhaseAudit, PhaseExecute, PhaseVerify, PhaseDone, PhaseFailed}
	return slices.Index(phases, left) < slices.Index(phases, right)
}

func (c *Coordinator) validateContractRecoverySource(ctx context.Context, item *TodoItem, session *SessionData) error {
	if item.TypedResult == nil || !taskResultStatusIsSuccessful(item.TypedResult.Status) {
		return fmt.Errorf("task %s has no accepted result", item.ID)
	}
	var snapshot *RunInputSnapshot
	for i := range session.RunInputSnapshots {
		if session.RunInputSnapshots[i].ID == item.RunInputSnapshotID {
			snapshot = &session.RunInputSnapshots[i]
			break
		}
	}
	if snapshot == nil || ValidateRunInputSnapshot(snapshot) != nil || snapshot.SnapshotHash != item.RunInputSnapshotHash {
		return fmt.Errorf("task %s frozen inputs cannot be verified", item.ID)
	}
	receipt := recoveredEvidenceReceipt(item, snapshot)
	if receipt == nil {
		return fmt.Errorf("task %s lacks an input-bound successful receipt", item.ID)
	}
	if item.ResultContract != nil && (receipt.ResultContractID != item.ResultContract.ID || receipt.ResultContractSHA256 != item.ResultContract.SchemaSHA256) && !validContractRevalidation(item, receipt) {
		return fmt.Errorf("task %s original contract receipt mismatch", item.ID)
	}
	if err := c.validateResumeTranscript(ctx, receipt); err != nil {
		return err
	}
	if _, digest, e := hashCanonicalRuntimeOutputs(item.TypedResult.RuntimeOutputs); e != nil || digest != item.TypedResult.RuntimeOutputsHash || digest != receipt.RuntimeOutputsHash {
		return fmt.Errorf("task %s runtime output integrity mismatch", item.ID)
	}
	if payload := item.TypedResult.StructuredPayload; payload != nil {
		if receipt.ResultPayloadSHA256 != payload.SHA256 || receipt.ResultValidation != ResultValidationValid {
			return fmt.Errorf("task %s receipt/payload mismatch", item.ID)
		}
		sum := sha256.Sum256(payload.Value)
		if hex.EncodeToString(sum[:]) != payload.SHA256 {
			return fmt.Errorf("task %s payload bytes changed", item.ID)
		}
		if item.ResultContract == nil || payload.Contract != *item.ResultContract {
			return fmt.Errorf("task %s payload contract mismatch", item.ID)
		}
	}
	return nil
}

// ApplyContractRecovery creates a child under the existing workspace/project
// locks. A crash after native fork but before publication leaves an ordinary
// old-policy child, never a partially upgraded runnable session.
func (c *Coordinator) ApplyContractRecovery(ctx context.Context, source *ResumeSource, name, expected string) (string, error) {
	if source == nil {
		return "", fmt.Errorf("recovery source is missing")
	}
	fresh, err := ReadResumeSource(c.session.Workspace, source.BranchID, "")
	if err != nil {
		return "", err
	}
	if fresh.eventHead != source.eventHead || fresh.eventHash != source.eventHash {
		return "", ErrStaleResumeSource
	}
	plan, err := c.AssessContractRecovery(ctx, fresh)
	if err != nil {
		return "", err
	}
	if expected == "" || expected != plan.SHA256 {
		return "", fmt.Errorf("recovery plan changed; preview again and supply its sha256")
	}
	version := c.session.WorkspaceVersion
	if !plan.MetadataOnly && (!version.Required() || !version.HoldsProjectLock) {
		return "", fmt.Errorf("contract recovery apply requires managed required workspace versioning and project lock")
	}
	if err := c.persistLegacySubmissionWork(ctx, source, plan); err != nil {
		return "", err
	}
	tree, err := LoadSessionTree(c.session.Workspace)
	if err != nil {
		return "", err
	}
	if c.eventStore == nil {
		c.eventStore, err = OpenEventStore(c.session.Workspace)
		if err != nil {
			return "", err
		}
		c.eventStore.SetBranchID(source.BranchID)
		c.SetEventJournal(eventStoreJournal{store: c.eventStore})
	}
	branch, err := c.forkContractRecovery(ctx, source, tree, name, plan.MetadataOnly)
	if err != nil {
		return "", err
	}
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(plan, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	candidate := RunEvent{SchemaVersion: eventStoreSchemaVersion, Type: string(EventContractRecovery), BranchID: branch.ID, Actor: "operator-contract-recovery", IdempotencyKey: "contract-recovery:" + plan.SHA256, Payload: payload}
	lineage, err := ProjectValidatedEventsForBranch(events, tree, branch.ID)
	if err != nil {
		return "", err
	}
	admitted, err := executionPolicySnapshotFromEvents(lineage)
	if err != nil {
		return "", err
	}
	if _, err = validateContractRecoveryLineage(candidate, lineage, admitted); err != nil {
		return "", err
	}
	_, err = c.eventStore.AppendPersistedBatchAtHead(ctx, []RunEvent{candidate}, events[len(events)-1].ID)
	if err != nil {
		return "", err
	}
	if err = RebuildSessionForBranch(c.session.Workspace, tree, c.eventStore, branch.ID); err != nil {
		return "", err
	}
	return branch.ID, nil
}

// A revalidation changes only the result contract binding, never native
// receipts, payload bytes, judgments or original source identity.
func validContractRevalidation(item *TodoItem, receipt *ExecutionReceipt) bool {
	if item == nil {
		return false
	}
	p := item.ContractRevalidation
	return p != nil && receipt != nil && item.ResultContract != nil && p.Accepted == *item.ResultContract &&
		receipt.ResultContractID == p.Original.ID && receipt.ResultContractSHA256 == p.Original.SchemaSHA256 && receipt.ResultPayloadSHA256 == p.PayloadSHA256 &&
		item.TypedResult != nil && item.TypedResult.StructuredPayload != nil && item.TypedResult.StructuredPayload.SHA256 == p.PayloadSHA256 && validCollectionDigest(p.SourceTaskSHA256)
}

func cloneContractRevalidation(proof *ContractRevalidation) *ContractRevalidation {
	if proof == nil {
		return nil
	}
	out := *proof
	return &out
}

func decodeContractRecovery(event RunEvent) (*ContractRecoveryPlan, error) {
	if event.SchemaVersion < eventStoreSchemaVersion {
		return nil, fmt.Errorf("contract recovery requires current events")
	}
	var plan ContractRecoveryPlan
	if _, err := decodeUniqueJSON(event.Payload); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(event.Payload, &plan, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	hash, err := recoveryPlanHash(plan)
	if err != nil || hash != plan.SHA256 || plan.Version != 1 || plan.SourceBranch == "" || plan.SourceBranch == effectiveEventBranchID(event) || plan.SourceHead == "" || !validCollectionDigest(plan.SourceHash) || !validCollectionDigest(plan.SourcePolicy) || validateExecutionPolicySnapshot(plan.Policy) != nil {
		return nil, fmt.Errorf("invalid contract recovery plan")
	}
	if !validContractRecoveryJournalIdentity(&plan) {
		return nil, fmt.Errorf("invalid contract recovery journal identity")
	}
	if plan.ResumePhase != PhaseInit && plan.ResumePhase != PhasePrepare && plan.ResumePhase != PhaseAudit && plan.ResumePhase != PhaseExecute && plan.ResumePhase != PhaseVerify {
		return nil, fmt.Errorf("invalid contract recovery phase")
	}
	seen := map[string]bool{}
	retained, retried, repaired := 0, 0, 0
	for _, patch := range plan.Tasks {
		if patch.ID == "" || seen[patch.ID] || !validCollectionDigest(patch.SourceSHA256) || (!validCollectionDigest(patch.ContractHash) && !runInputHashPattern.MatchString(patch.ContractHash)) {
			return nil, fmt.Errorf("invalid recovery task identity")
		}
		seen[patch.ID] = true
		if (patch.Collection != nil || patch.LegacySubmission != nil) && patch.Action != "repair_result" {
			return nil, fmt.Errorf("collection proof requires result-only action")
		}
		switch patch.Action {
		case "retain":
			retained++
		case "repair_result":
			repaired++
			if (patch.Collection == nil) == (patch.LegacySubmission == nil) || patch.Revalidation != nil {
				return nil, fmt.Errorf("result-only repair requires collection proof")
			}
		case "retry":
			retried++
			if patch.Revalidation != nil {
				return nil, fmt.Errorf("retry cannot reuse result proof")
			}
		default:
			return nil, fmt.Errorf("invalid contract recovery action")
		}
	}
	if retried+repaired == 0 || repaired > 1 || repaired != plan.Repaired || retained != plan.Retained || retried != plan.Retried {
		return nil, fmt.Errorf("invalid recovery counts")
	}
	return &plan, nil
}

func validateContractRecoveryLineage(event RunEvent, preceding []RunEvent, admitted *ExecutionPolicySnapshot) (*ContractRecoveryPlan, error) {
	plan, err := decodeContractRecovery(event)
	if err != nil {
		return nil, err
	}
	if admitted == nil || admitted.ConfigurationHash != plan.SourcePolicy {
		return nil, fmt.Errorf("contract recovery original policy mismatch")
	}
	if plan.WorkRetry == nil && plan.ActionRepair == nil && plan.SourcePolicy == plan.Policy.ConfigurationHash {
		return nil, fmt.Errorf("unchanged-policy recovery requires an explicit work retry proof")
	}
	if plan.WorkRetry != nil {
		if admitted.ConfigurationHash != plan.Policy.ConfigurationHash || plan.InputPolicyUpgrade != nil || len(plan.ContractChanges) != 0 {
			return nil, fmt.Errorf("work retry cannot change execution policy or result contracts")
		}
		if err := validateReadOnlyWorkRetry(preceding, plan); err != nil {
			return nil, err
		}
	}
	if plan.ActionRepair != nil && (!sameCanonicalEvidence(admitted.ResultContracts, plan.Policy.ResultContracts) || admitted.StaticWorkflowHash != plan.Policy.StaticWorkflowHash) {
		return nil, fmt.Errorf("action repair cannot change result schemas or static contracts")
	}
	left, right := *cloneExecutionPolicySnapshot(admitted), *cloneExecutionPolicySnapshot(plan.Policy)
	if plan.InputPolicyUpgrade != nil {
		if err := validateRunInputPolicyRecovery(ReduceToSessionData(preceding), admitted, plan.Policy, plan.InputPolicyUpgrade); err != nil {
			return nil, err
		}
		left.RunInputPolicyHash, right.RunInputPolicyHash = "", ""
	}
	if err := validateLegacyPolicyRecovery(&left, &right, plan, ReduceToTodoList(preceding)); err != nil {
		return nil, err
	}
	left.ResultContracts, right.ResultContracts = nil, nil
	left.StaticWorkflowHash, right.StaticWorkflowHash = "", ""
	left.ConfigurationHash, right.ConfigurationHash = "", ""
	if !sameCanonicalEvidence(left, right) {
		return nil, fmt.Errorf("contract recovery changed execution authority")
	}
	found := false
	for _, source := range preceding {
		if source.ID == plan.SourceHead && source.Hash == plan.SourceHash && effectiveEventBranchID(source) == plan.SourceBranch {
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("contract recovery source head missing")
	}
	tasks := ReduceToTodoList(preceding)
	if err := validateRecoveryTaskSources(tasks, plan); err != nil {
		return nil, err
	}
	if err := validateActionPayloadReconstructionProofs(preceding, tasks, plan); err != nil {
		return nil, err
	}
	if err := validateActionRepairProof(preceding, tasks, plan); err != nil {
		return nil, err
	}

	if plan.Repaired > 0 {
		projection := make(map[string]*TodoItem)
		for _, item := range tasks {
			projection[item.ID] = cloneTodoItem(item)
		}
		if err := applyContractRecoveryTasks(projection, plan, effectiveEventBranchID(event)); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

func validateRecoveryTaskSources(tasks []*TodoItem, plan *ContractRecoveryPlan) error {
	originals := map[string]*TodoItem{}
	for _, item := range tasks {
		if !IsPrimaryOccurrence(item) {
			originals[item.ID] = item
		}
	}
	if len(originals) != len(plan.Tasks) {
		return fmt.Errorf("contract recovery source tasks changed")
	}
	for _, patch := range plan.Tasks {
		item := originals[patch.ID]
		digest, err := collectionDigest(item)
		if item == nil || err != nil || digest != patch.SourceSHA256 {
			return fmt.Errorf("contract recovery source task %s changed", patch.ID)
		}
		if patch.PayloadReconstruction != nil && plan.ActionRepair == nil {
			return fmt.Errorf("payload reconstruction requires action repair proof")
		}
		if err := validateReconstructedActionPayload(item, patch.PayloadReconstruction); err != nil {
			return err
		}
		preservedBrowser := plan.Repaired > 0 && collectionRecoveryBrowserTask(item, contractRepairTaskID(plan))
		if plan.MetadataOnly && !metadataReadOnlyTask(item.SideEffect, item.Execution) && !preservedBrowser {
			return fmt.Errorf("metadata-only recovery refuses task %s side effects", item.ID)
		}
		if patch.Action == "repair_result" {
			if err := validateContractResultRepairSource(item, patch, plan); err != nil {
				return err
			}
		}
		if patch.Action == "retry" && item.SideEffect != SideEffectNone {
			return fmt.Errorf("contract recovery refuses replay of task %s side effects", patch.ID)
		}
		if patch.Action == "retain" && item.Status != TaskDone {
			return fmt.Errorf("contract recovery can retain only accepted tasks")
		}
		if proof := patch.Revalidation; proof != nil {
			if item.ResultContract == nil || patch.ResultContract == nil || !recoveryOriginalContractMatches(item, proof.Original) || proof.Accepted != *patch.ResultContract || proof.SourceTaskSHA256 != digest || item.TypedResult == nil || item.TypedResult.StructuredPayload == nil || proof.PayloadSHA256 != item.TypedResult.StructuredPayload.SHA256 {
				return fmt.Errorf("invalid contract revalidation proof")
			}
		} else if patch.Action == "retain" && !sameCanonicalEvidence(item.ResultContract, patch.ResultContract) {
			return fmt.Errorf("contract recovery missing revalidation proof")
		}
	}
	return nil
}

func applyContractRecoveryTasks(tasks map[string]*TodoItem, plan *ContractRecoveryPlan, branchID string) error {
	values := make([]*TodoItem, 0, len(tasks))
	for _, item := range tasks {
		values = append(values, item)
	}
	if err := validateRecoveryTaskSources(values, plan); err != nil {
		return err
	}
	for _, patch := range plan.Tasks {
		item := cloneTodoItem(tasks[patch.ID])
		if patch.PayloadReconstruction != nil {
			item.Action.Payload = patch.PayloadReconstruction.Payload
		}
		item.ResultContract = patch.ResultContract.clone()
		item.ContractHash = patch.ContractHash
		item.ContractRevalidation = cloneContractRevalidation(patch.Revalidation)
		switch patch.Action {
		case "retain":
			if item.TypedResult.StructuredPayload != nil && patch.ResultContract != nil {
				item.TypedResult.StructuredPayload.Contract = *patch.ResultContract
			}
		case "repair_result":
			if patch.LegacySubmission != nil {
				projectLegacySubmission(item, patch, plan, branchID)
			} else {
				projectContractResultRepair(item, patch, plan, branchID)
			}
		case "retry":
			// Historical native receipts remain immutable. Clear only the child's
			// active attempt, so ordinary retry opens a fresh lease and collector.
			if item.ExecutionReceipt != nil {
				item.ExecutionReceipts = appendExecutionReceipt(item.ExecutionReceipts, *item.ExecutionReceipt)
			}
			item.ExecutionReceipt = nil
			item.TypedResult = nil
			item.ResultCandidate = nil
			item.VerifyResult = nil
			item.RuntimeError = nil
			item.FailureEvent = nil
			item.Resolution = nil
			item.Status = TaskPending
			item.Detail = "contract recovery: retry read-only work"
			item.Output = ""
			item.StartedAt = time.Time{}
			item.EndedAt = time.Time{}
			item.OccurrenceRevision++
			item.DispatchID = ""
			item.Retries = 0
			item.CheckpointPause = false
			item.RecoveryState = ""
			item.FailureFingerprints = nil
			item.RecoveryHypothesis = nil
			item.RemediationContext = nil
		default:
			return fmt.Errorf("invalid contract recovery action %q", patch.Action)
		}
		tasks[patch.ID] = item
	}
	return bindContractRepairEvidence(tasks, plan)
}

func applyContractRecoverySession(session *SessionData, plan *ContractRecoveryPlan) {
	session.ExecutionPolicySnapshot = cloneExecutionPolicySnapshot(plan.Policy)
	session.RunResult = nil
	session.ContinuationCheckpoint = nil
	session.CriterionResults = nil
	session.CriterionCheckpoints = nil
	session.LastCriterionProgressAt = ""
	session.PendingWrapUp = nil
	session.RetryState = NewRetryState()
	session.WorkflowState = plan.ResumePhase
	for phase := range session.PhaseResults {
		if !phaseBefore(phase, plan.ResumePhase) {
			delete(session.PhaseResults, phase)
		}
	}
	session.WorksetStates = nil // Recomputed from retained expansion receipts and child tasks.
}

func (c *Coordinator) contractRecoveryPolicies(ctx context.Context, source *ResumeSource) (old, current *ExecutionPolicySnapshot, bindings map[string]ExecutionResultContractPolicySnapshot, err error) {
	old, current = source.Session.ExecutionPolicySnapshot, c.ExecutionPolicySnapshot()
	if old == nil || current == nil || validateExecutionPolicySnapshot(old) != nil || validateExecutionPolicySnapshot(current) != nil {
		return nil, nil, nil, fmt.Errorf("contract recovery requires a valid original policy")
	}
	// Use the same unreachable-backend environment compatibility as ordinary
	// resume, retaining the frozen values in the child policy.
	current = normalizeUnusedExecutionEnvironment(old, current)
	current.ConfigurationHash, err = executionPolicyConfigurationHash(current)
	if err != nil {
		return nil, nil, nil, err
	}
	if readOnlyActionRepairID(ctx) != "" && (!sameCanonicalEvidence(old.ResultContracts, current.ResultContracts) || old.StaticWorkflowHash != current.StaticWorkflowHash) {
		return nil, nil, nil, fmt.Errorf("action repair cannot change result schemas or static contracts")
	}
	// Reconstruct the current static workflow with original result refs. This
	// proves that the opaque static hash changed only because of those refs.
	historical := *c.session
	historical.ContractTasks = slices.Clone(c.session.ContractTasks)
	historical.AgentResultContracts = maps.Clone(c.session.AgentResultContracts)
	bindings = map[string]ExecutionResultContractPolicySnapshot{}
	for _, b := range old.ResultContracts {
		bindings[b.Owner] = b
	}
	if len(bindings) != len(current.ResultContracts) {
		return nil, nil, nil, fmt.Errorf("contract recovery cannot add or remove result owners")
	}
	for _, b := range current.ResultContracts {
		if _, ok := bindings[b.Owner]; !ok {
			return nil, nil, nil, fmt.Errorf("contract recovery changed result owner %s", b.Owner)
		}
	}
	for i := range historical.ContractTasks {
		task := &historical.ContractTasks[i]
		if b, ok := bindings["contract:"+task.ID]; ok {
			task.ResultContract = &ResultContractRef{ID: b.ID, SchemaSHA256: b.SchemaSHA256, RequireStructured: b.RequireStructured}
			task.ResultContractSpec = &resultContractSpec{Schema: b.ID, RequireStructured: b.RequireStructured}
		}
	}
	if item := selectedLegacySubmission(source.Session, contractResultRepairID(ctx)); item != nil {
		if !sameCanonicalEvidence(old.ResultContracts, current.ResultContracts) {
			return nil, nil, nil, fmt.Errorf("legacy submission migration cannot also change schemas")
		}
		for i := range historical.ContractTasks {
			if historical.ContractTasks[i].ID == item.ContractID {
				if !validateLegacyExecutionChange(item.Execution, historical.ContractTasks[i].Execution) {
					return nil, nil, nil, fmt.Errorf("legacy migration changed execution beyond submission sequence")
				}
				historical.ContractTasks[i].Execution = cloneExecutionContract(item.Execution)
			}
		}
	}
	hash, err := staticContractEdgesHash(&historical)
	if err != nil {
		return nil, nil, nil, err
	}
	left, right := *cloneExecutionPolicySnapshot(old), *cloneExecutionPolicySnapshot(current)
	if frozenInputPolicyRecoveryRequested(ctx) && old.RunInputPolicyHash != current.RunInputPolicyHash {
		if _, err := c.assessRunInputPolicyRecovery(source.Session, old, current); err != nil {
			return nil, nil, nil, err
		}
		left.RunInputPolicyHash, right.RunInputPolicyHash = "", ""
	}
	if item := selectedLegacySubmission(source.Session, contractResultRepairID(ctx)); item != nil {
		if err := normalizeLegacyTransport(&left, &right, item.Agent); err != nil {
			return nil, nil, nil, err
		}
	}
	right.StaticWorkflowHash = hash
	left.ResultContracts, right.ResultContracts = nil, nil
	left.ConfigurationHash, right.ConfigurationHash = "", ""
	if !sameCanonicalEvidence(left, right) {
		return nil, nil, nil, fmt.Errorf("contract recovery refuses non-result policy drift; restore models, tools, input and execution settings\n%s", executionPolicyDifferences(&left, &right))
	}

	return old, current, bindings, nil
}

func (c *Coordinator) assessContractRecoveryTask(ctx context.Context, item *TodoItem, session *SessionData, tasks map[string]TaskDef, bindings map[string]ExecutionResultContractPolicySnapshot) (ContractRecoveryTask, bool, error) {
	needsRetry := false
	definition, ok := tasks[item.ContractID]
	if !ok {
		return ContractRecoveryTask{}, false, fmt.Errorf("task %s has no current static contract", item.ID)
	}
	oldDef := definition
	if b, ok := bindings["contract:"+definition.ID]; ok {
		oldDef.ResultContract = &ResultContractRef{ID: b.ID, SchemaSHA256: b.SchemaSHA256, RequireStructured: b.RequireStructured}
		oldDef.ResultContractSpec = &resultContractSpec{Schema: b.ID, RequireStructured: b.RequireStructured}
	}
	if contractResultRepairID(ctx) == item.ID && legacySubmissionTarget(item) {
		if !validateLegacyExecutionChange(item.Execution, definition.Execution) {
			return ContractRecoveryTask{}, false, fmt.Errorf("legacy task execution drift")
		}
		oldDef.Execution = cloneExecutionContract(item.Execution)
	}
	comparison, reconstruction, e := prepareRecoveredActionPayload(ctx, oldDef, item, session)
	if e != nil {
		return ContractRecoveryTask{}, false, e
	}
	oldHash, e := contractRecoveryTaskHash(oldDef, comparison, session)
	if e != nil {
		return ContractRecoveryTask{}, false, fmt.Errorf("task %s contract inputs: %w", item.ID, e)
	}
	if oldHash != item.ContractHash {
		return ContractRecoveryTask{}, false, fmt.Errorf("task %s non-result contract changed (contract %q)", item.ID, definition.ID)
	}
	updated := *session
	updated.ExecutionPolicySnapshot = c.ExecutionPolicySnapshot()
	newHash, e := contractRecoveryTaskHash(definition, comparison, &updated)
	if e != nil {
		return ContractRecoveryTask{}, false, e
	}
	digest, e := collectionDigest(item)
	if e != nil {
		return ContractRecoveryTask{}, false, e
	}
	targetRef := definition.ResultContract.clone()
	if targetRef == nil {
		if bound, ok := c.session.AgentResultContracts[normalizedName(item.Agent)]; ok {
			targetRef = bound.clone()
		}
	}
	patch := ContractRecoveryTask{PayloadReconstruction: reconstruction, Reason: "accepted_result", SourceStatus: item.Status, ID: item.ID, SourceSHA256: digest, Action: "retain", ResultContract: targetRef, ContractHash: newHash, Revalidation: nil}
	if validContractRevalidation(item, item.ExecutionReceipt) {
		patch.Revalidation = cloneContractRevalidation(item.ContractRevalidation)
		patch.Revalidation.SourceTaskSHA256 = digest
	}
	if item.Status != TaskDone {
		patch.Reason = "unfinished_task"
		if item.Status == TaskError || item.Status == TaskBlocked {
			patch.Reason = "original_failure"
		}
		needsRetry = true
	} else {
		if err := c.validateContractRecoverySource(ctx, item, session); err != nil {
			return ContractRecoveryTask{}, false, err
		}
		if patch.ResultContract != nil && !resultProjectionMatchesAccepted(item.TypedResult, c.session.ResultContracts[patch.ResultContract.ID]) {
			patch.Reason = "result_projection_changed"
			return patch, true, nil
		}
		if item.ResultContract != nil && patch.ResultContract != nil && *item.ResultContract != *patch.ResultContract {
			compiled := c.session.ResultContracts[patch.ResultContract.ID]
			if compiled == nil || item.TypedResult.StructuredPayload == nil {
				return ContractRecoveryTask{}, false, fmt.Errorf("task %s lacks revalidatable payload", item.ID)
			}
			var observations []taskTranscriptRecord
			if compiled.toolEvidence != nil {
				receipt := recoveredEvidenceReceipt(item, contractRecoveryInputSnapshot(item, session))
				if receipt == nil {
					return ContractRecoveryTask{}, false, fmt.Errorf("task %s lacks its original tool-evidence receipt", item.ID)
				}
				transcript, err := c.restoreTranscriptEvidence(ctx, receipt, receipt.RunID)
				if err != nil {
					return ContractRecoveryTask{}, false, err
				}
				observations = transcript.evidenceRecords()
			}
			payload, e := validateStructuredResultPayload(compiled, *patch.ResultContract, item.TypedResult.StructuredPayload.Value, observations)
			if e != nil || payload.SHA256 != item.TypedResult.StructuredPayload.SHA256 {
				patch.Reason = "result_schema_invalid"
				needsRetry = true
			} else {
				original := *item.ResultContract
				if validContractRevalidation(item, item.ExecutionReceipt) {
					original = item.ContractRevalidation.Original
				}
				patch.Reason = "result_revalidated"
				patch.Revalidation = &ContractRevalidation{Original: original, Accepted: *patch.ResultContract, PayloadSHA256: payload.SHA256, SourceTaskSHA256: digest}
			}
		} else if (item.ResultContract == nil) != (patch.ResultContract == nil) {
			patch.Reason = "result_contract_binding_changed"
			needsRetry = true
		}
	}

	return patch, needsRetry, nil
}

func recoveryOriginalContractMatches(item *TodoItem, original ResultContractRef) bool {
	if item.ResultContract == nil {
		return false
	}
	if validContractRevalidation(item, item.ExecutionReceipt) {
		return original == item.ContractRevalidation.Original
	}
	return original == *item.ResultContract
}
