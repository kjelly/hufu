package team

import (
	"context"
	"fmt"
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
	if !retryWork && actionRepairID == "" && old.ConfigurationHash == current.ConfigurationHash {
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

func validContractRecoveryJournalIdentity(plan *ContractRecoveryPlan) bool {
	return (plan.JournalHead == "") == (plan.JournalHash == "") &&
		(plan.JournalHash == "" || validCollectionDigest(plan.JournalHash))
}
