package team

import (
	"context"
	"fmt"
	"slices"
)

// newContractRecoveryPlan selects one explicit repair mode before evaluating
// task compatibility and downstream invalidation.
func (c *Coordinator) newContractRecoveryPlan(ctx context.Context, source *ResumeSource, old, current *ExecutionPolicySnapshot) (*ContractRecoveryPlan, error) {
	var err error
	actionRepairID := readOnlyActionRepairID(ctx)
	workRetryTask, retryWork := readOnlyWorkRetryRequested(ctx)
	if retryWork && old.ConfigurationHash != current.ConfigurationHash {
		return nil, fmt.Errorf("work retry requires unchanged result contracts and execution policy; preview contract recovery separately")
	}
	selected := todoItemByID(source.Session.Tasks, contractResultRepairID(ctx))
	acceptedRepair := selected != nil && selected.Status == TaskDone
	if !retryWork && actionRepairID == "" && !acceptedRepair && old.ConfigurationHash == current.ConfigurationHash &&
		(!metadataContractRecoveryRequested(ctx) || recoveredCollectionVerifierID(source.Session.Tasks, source.lineage, source.BranchID) == "") {
		return nil, fmt.Errorf("result contracts are unchanged; use ordinary resume or result-only repair")
	}
	// The journal head protects publication against concurrent writes; the
	// recovery proof must instead anchor the selected branch's own lineage.
	if len(source.lineage) == 0 {
		return nil, fmt.Errorf("contract recovery source lineage is empty")
	}
	head := source.lineage[len(source.lineage)-1]
	if effectiveEventBranchID(head) != source.BranchID {
		return nil, fmt.Errorf("contract recovery source branch has no canonical head")
	}
	plan := &ContractRecoveryPlan{MetadataOnly: metadataContractRecoveryRequested(ctx), Version: 1, JournalHead: source.eventHead, JournalHash: source.eventHash, SourceBranch: source.BranchID, SourceHead: head.ID, SourceHash: head.Hash, SourcePolicy: old.ConfigurationHash, Policy: current, ResumePhase: PhaseVerify}
	if actionRepairID != "" {
		if retryWork {
			return nil, fmt.Errorf("action repair cannot combine with work retry")
		}
		plan.ActionRepair, err = c.assessReadOnlyActionRepair(ctx, source, actionRepairID)
		if err != nil {
			return nil, err
		}
	}
	if retryWork {
		plan.WorkRetry, err = c.assessReadOnlyWorkRetry(ctx, source, workRetryTask)
		if err != nil {
			return nil, err
		}
	}
	if old.RunInputPolicyHash != current.RunInputPolicyHash {
		plan.InputPolicyUpgrade, err = c.assessRunInputPolicyRecovery(source.Session, old, current)
		if err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// A prepared result can be settled without re-opening a failed workflow.
// Its repair grant pins the failed downstream verifier that remains to run.
func recoveredCollectionVerifierID(tasks []*TodoItem, events []RunEvent, branchID string) string {
	unresolved := UnresolvedTaskReferences(tasks)
	if len(unresolved) != 1 {
		return ""
	}
	verifier := todoItemByID(tasks, unresolved[0].ID)
	if verifier == nil {
		return ""
	}
	for _, sourceID := range verifier.DependsOn {
		source := todoItemByID(tasks, sourceID)
		if !failedReadOnlyCollectionVerifier(verifier, source) || !hasCommittedWorkflowCollectionRepair(source, branchID) {
			continue
		}
		for _, grant := range collectionRepairGrants(source.ExecutionReceipt) {
			if provenCollectionVerifierRetry(events, branchID, verifier, source, grant) {
				return verifier.ID
			}
		}
	}
	return ""
}

func provenCollectionVerifierRetry(events []RunEvent, branchID string, verifier, producer *TodoItem, grant ResultRepairGrant) bool {
	assessment := grant.Recovery
	if !isWorkflowCollectionRepairGrant(grant, currentResultCollection(producer)) || assessment.Disposition != RecoveryCommitLocal ||
		!slices.ContainsFunc(assessment.Checks, func(check ResultCollectionRecoveryCheck) bool {
			return check.Code == "prepared_local_commit" && check.Passed
		}) {
		return false
	}
	i := slices.IndexFunc(events, func(event RunEvent) bool {
		return event.ID == assessment.JournalHeadID && effectiveEventBranchID(event) == branchID
	})
	if i < 0 {
		return false
	}
	before := ReduceToSessionData(events[:i+1])
	localOnly, err := resultRepairLocalCommitOnly(before, producer.ID)
	if err != nil || !localOnly {
		return false
	}
	previous := todoItemByID(before.Tasks, verifier.ID)
	checkpoint := currentResultCollection(todoItemByID(before.Tasks, producer.ID))
	digest, err := collectionDigest(checkpoint)
	return err == nil && digest == assessment.CheckpointSHA256 && sameResumeTaskEvidence(previous, verifier) &&
		previous.OccurrenceRevision == verifier.OccurrenceRevision && sameCanonicalEvidence(previous.FailureEvent, verifier.FailureEvent)
}

func validateUnchangedContractVerifierRetry(tasks []*TodoItem, events []RunEvent, plan *ContractRecoveryPlan) error {
	if plan.WorkRetry != nil || plan.ActionRepair != nil || plan.SourcePolicy != plan.Policy.ConfigurationHash {
		return nil
	}
	for _, patch := range plan.Tasks {
		if patch.Collection != nil && patch.Collection.AcceptedResult != nil {
			return nil
		}
	}
	if !plan.MetadataOnly || plan.Repaired != 0 || plan.Retried != 1 || len(plan.ContractChanges) != 0 || plan.InputPolicyUpgrade != nil {
		return fmt.Errorf("unchanged-policy recovery requires an explicit retry proof")
	}
	verifierID := recoveredCollectionVerifierID(tasks, events, plan.SourceBranch)
	for _, patch := range plan.Tasks {
		if patch.Action == "retry" && patch.ID == verifierID {
			return nil
		}
	}
	return fmt.Errorf("unchanged-policy recovery requires the failed verifier bound to the repaired result")
}

func validContractRecoveryJournalIdentity(plan *ContractRecoveryPlan) bool {
	return (plan.JournalHead == "") == (plan.JournalHash == "") &&
		(plan.JournalHash == "" || validCollectionDigest(plan.JournalHash))
}

// Replaying a source or repairing its accepted claims invalidates every
// dependent result. Enforce this in replay as well as the planner.
func validateRecoveryDependentResults(tasks []*TodoItem, plan *ContractRecoveryPlan) error {
	patches := make(map[string]ContractRecoveryTask, len(plan.Tasks))
	for _, patch := range plan.Tasks {
		patches[patch.ID] = patch
	}
	for _, item := range tasks {
		if item == nil {
			continue
		}
		patch, exists := patches[item.ID]
		if !exists || patch.Action != "retain" {
			continue
		}
		for _, id := range resultDependencyIDs(item) {
			if parent, ok := patches[id]; ok && (parent.Action == "retry" || parent.Action == "repair_result") {
				return fmt.Errorf("retained task %s depends on changed source %s", item.ID, id)
			}
		}
	}
	return nil
}
