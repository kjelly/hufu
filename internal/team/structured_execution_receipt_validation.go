package team

import (
	"fmt"
	"strings"
)

// ValidateSuccessfulContract proves that a claimed successful result covers
// the complete structured DAG in causal order. It also proves that each
// mutation ran exactly once and consumed the digest from a prior successful
// validator receipt.
func (r *ExecutionStepReceiptRegistry) ValidateSuccessfulContract(taskID string, attempt int, contract ExecutionContract, ids []string) error {
	if err := r.ValidateCompleteClaims(taskID, attempt, ids); err != nil {
		return err
	}
	ordered, err := orderedStructuredExecutionSteps(contract.Steps)
	if err != nil {
		return err
	}
	declared := make(map[string]ExecutionStep, len(ordered))
	for _, step := range ordered {
		declared[step.ID] = step
	}
	receipts := r.claimedReceiptsInOrder(ids)
	succeeded := make(map[string]bool, len(ordered))
	validatorDigests := make(map[string]string)
	validatorFailures := make(map[string]int)
	mutationCalls := make(map[string]int)
	mutationStarted := false

	for _, receipt := range receipts {
		step, ok := declared[receipt.StepID]
		if !ok {
			return fmt.Errorf("successful structured result cites receipt %q for undeclared step %q", receipt.ID, receipt.StepID)
		}
		if receipt.Tool != step.Tool {
			return fmt.Errorf("receipt %q tool %q does not match step %q tool %q", receipt.ID, receipt.Tool, step.ID, step.Tool)
		}
		if receipt.PolicyVerdict != "allowed" {
			return fmt.Errorf("receipt %q has non-allowed policy verdict %q", receipt.ID, receipt.PolicyVerdict)
		}
		for _, dependency := range step.DependsOn {
			if !succeeded[dependency] {
				return fmt.Errorf("receipt %q ran step %q before dependency %q succeeded", receipt.ID, step.ID, dependency)
			}
		}
		if mutationStarted && (step.Effect == ExecutionEffectProduce || step.Effect == ExecutionEffectValidate) {
			return fmt.Errorf("receipt %q attempts %s step %q after mutation started", receipt.ID, step.Effect, step.ID)
		}

		if receipt.ExitCode != 0 {
			if step.Effect != ExecutionEffectValidate || normalizedFailurePolicy(step.OnFailure) != StepFailureRepairable {
				return fmt.Errorf("receipt %q records terminal failure for step %q", receipt.ID, step.ID)
			}
			validatorFailures[step.ID]++
			if validatorFailures[step.ID] > step.MaxRepairs {
				return fmt.Errorf("step %q exceeds max_repairs=%d", step.ID, step.MaxRepairs)
			}
			continue
		}

		switch step.Effect {
		case ExecutionEffectValidate:
			if receipt.ValidatorVerdict != "pass" {
				return fmt.Errorf("successful validator receipt %q lacks pass verdict", receipt.ID)
			}
			for _, artifact := range step.Consumes {
				digest := strings.TrimSpace(receipt.ConsumedDigests[artifact])
				if digest == "" {
					return fmt.Errorf("validator receipt %q lacks consumed digest for %q", receipt.ID, artifact)
				}
				validatorDigests[artifact] = digest
			}
		case ExecutionEffectMutate:
			mutationStarted = true
			mutationCalls[step.ID]++
			if mutationCalls[step.ID] > 1 {
				return fmt.Errorf("mutation step %q executed more than once", step.ID)
			}
			for _, artifact := range step.Consumes {
				consumed := strings.TrimSpace(receipt.ConsumedDigests[artifact])
				if consumed == "" || consumed != validatorDigests[artifact] {
					return fmt.Errorf("mutation receipt %q did not consume validator-approved digest for %q", receipt.ID, artifact)
				}
			}
		}
		succeeded[step.ID] = true
	}

	for _, step := range ordered {
		if !succeeded[step.ID] {
			return fmt.Errorf("successful structured result has no successful receipt for step %q", step.ID)
		}
		if step.Effect == ExecutionEffectMutate && mutationCalls[step.ID] != 1 {
			return fmt.Errorf("successful structured result requires exactly one execution of mutation step %q", step.ID)
		}
	}
	return nil
}

func (r *ExecutionStepReceiptRegistry) claimedReceiptsInOrder(ids []string) []ExecutionStepReceipt {
	receipts := make([]ExecutionStepReceipt, 0, len(ids))
	for _, id := range ids {
		if receipt, ok := r.Get(strings.TrimSpace(id)); ok {
			receipts = append(receipts, receipt)
		}
	}
	sortExecutionReceipts(receipts)
	return receipts
}

func sortExecutionReceipts(receipts []ExecutionStepReceipt) {
	for i := 1; i < len(receipts); i++ {
		for j := i; j > 0; j-- {
			left, right := receipts[j-1], receipts[j]
			if left.StartedAt.Before(right.StartedAt) || (left.StartedAt.Equal(right.StartedAt) && left.ID <= right.ID) {
				break
			}
			receipts[j-1], receipts[j] = receipts[j], receipts[j-1]
		}
	}
}
