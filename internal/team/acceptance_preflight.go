package team

import (
	"fmt"
	"strings"
)

// preflightActionAcceptance checks the blocking acceptance assertions that read
// a runtime action's outputs before the action is marked done.
//
// task_output_assert accepts only runtime action results, and an action run
// again on the same frozen inputs returns the same outputs, so a failure here
// is the failure finish would report. Without this check the run went on to
// dispatch every later phase and learned the result only at finish: one
// review run spent 30 minutes and 1.4M tokens after its producer had already
// returned a scope that could never match the frozen input.
//
// Advisory acceptance does not decide the outcome, so it is left to finish.
func (c *Coordinator) preflightActionAcceptance(todoID string, runtimeOutputs map[string]any) error {
	if c == nil || c.ExecutionProfile().AcceptanceMode != AcceptanceBlocking {
		return nil
	}
	c.mu.RLock()
	var verifications []VerificationSpec
	if c.acceptanceSpec != nil {
		verifications = append(verifications, c.acceptanceSpec.Verifications...)
	}
	c.mu.RUnlock()
	if len(verifications) == 0 {
		return nil
	}
	item := c.todoItemByID(todoID)
	if item == nil {
		return nil
	}
	source := cloneTodoItem(item)
	inputs := make(map[string]ResolvedRunInput)
	if snapshot := c.RunInputSnapshot(); snapshot != nil {
		for _, input := range snapshot.Inputs {
			inputs[input.Name] = input
		}
	}
	for _, raw := range verifications {
		spec := NormalizeVerificationSpec(raw, "", "")
		if spec.Type != VerifyTaskOutputAssert || spec.Mode == "observation" || !taskReferenceNamesItem(spec.WorksetSourceTask, source) {
			continue
		}
		if validateVerificationSpec(spec) != nil {
			// runAcceptance reports a malformed contract at finish.
			continue
		}
		assertionErr := evaluateTaskOutputAssertions(runtimeOutputs, spec, inputs, source)
		res := &VerificationResult{Spec: &spec}
		if assertionErr != nil {
			res.ExitCode = 1
		}
		if _, err := applyVerificationMode(res, assertionErr, spec.Mode); err != nil {
			return fmt.Errorf("blocking acceptance check on output %q already fails for this result, so the run cannot succeed: %w", spec.TaskOutputName, err)
		}
	}
	return nil
}

func evaluateTaskOutputAssertions(runtimeOutputs map[string]any, spec VerificationSpec, inputs map[string]ResolvedRunInput, source *TodoItem) error {
	root, ok := runtimeOutputs[strings.TrimSpace(spec.TaskOutputName)]
	if !ok {
		return fmt.Errorf("runtime output %q does not exist", spec.TaskOutputName)
	}
	for index, assertion := range spec.Assertions {
		if _, err := evaluateTaskOutputAssertion(root, assertion, inputs, source, spec.TaskOutputName); err != nil {
			return fmt.Errorf("assertion %d: %w", index, err)
		}
	}
	return nil
}

// taskReferenceNamesItem reports whether a source-task reference names the
// item, using the same identities uniqueTaskOutputSource accepts.
func taskReferenceNamesItem(reference string, item *TodoItem) bool {
	if item == nil {
		return false
	}
	normalized := normalizeTaskReferenceID(strings.TrimSpace(reference))
	if normalized == "" {
		return false
	}
	for _, id := range []string{item.ID, item.PlanTaskID, item.ContractID} {
		if normalizeTaskReferenceID(id) == normalized {
			return true
		}
	}
	return false
}
