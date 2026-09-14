package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func (c *Coordinator) executeTaskOutputAssertVerification(_ context.Context, spec VerificationSpec) (*VerificationResult, error) {
	workDir := c.verificationWorkDir()
	res := &VerificationResult{WorkDir: workDir, Spec: &spec}
	fail := func(err error) (*VerificationResult, error) {
		res.ExitCode = 1
		res.Stderr = "task_output_assert failed: " + err.Error()
		res.Fingerprint = ComputeVerificationFingerprint(spec, res, workDir)
		return applyVerificationMode(res, errors.New(res.Stderr), spec.Mode)
	}
	if err := validateVerificationSpec(spec); err != nil {
		res.ExitCode = -1
		res.Stderr = "task_output_assert malformed: " + err.Error()
		res.Fingerprint = ComputeVerificationFingerprint(spec, res, workDir)
		return res, fmt.Errorf("malformed verification spec (failed closed): %w", err)
	}
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return fail(errors.New("coordinator task occurrence state is unavailable"))
	}
	snapshot := c.RunInputSnapshot()
	evidence, err := ReplayTaskOutputAssertions(c.taskTracker.TodoList().Items(), coordinatorRuntimeRunID(c), snapshot, spec)
	res.TaskOutputAssertions = evidence
	if err != nil {
		return fail(err)
	}
	res.ExitCode = 0
	res.Stdout = fmt.Sprintf("task_output_assert passed (%d assertion(s))", len(spec.Assertions))
	res.Fingerprint = ComputeVerificationFingerprint(spec, res, workDir)
	return applyVerificationMode(res, nil, spec.Mode)
}

// ReplayTaskOutputAssertions independently re-evaluates a task_output_assert
// against event-replayed task and input state. It performs no I/O and is the
// shared trust boundary used by both live acceptance and offline audit.
func ReplayTaskOutputAssertions(items []*TodoItem, runID string, snapshot *RunInputSnapshot, spec VerificationSpec) ([]TaskOutputAssertionResult, error) {
	if err := validateVerificationSpec(spec); err != nil {
		return nil, fmt.Errorf("malformed verification spec: %w", err)
	}
	source, err := uniqueTaskOutputSource(items, spec.WorksetSourceTask, runID)
	if err != nil {
		return nil, err
	}
	if source.Status != TaskDone || source.TypedResult == nil {
		return nil, fmt.Errorf("source task %q is not a completed canonical occurrence", spec.WorksetSourceTask)
	}
	result := cloneTaskResult(source.TypedResult)
	if err := validateCompletedTaskResult(result); err != nil {
		return nil, fmt.Errorf("source task result is not successful: %w", err)
	}
	if result.Source != "runtime" {
		return nil, fmt.Errorf("source task result has untrusted source %q", result.Source)
	}
	if err := validateRuntimeTaskResult(result); err != nil {
		return nil, fmt.Errorf("source runtime outputs are invalid: %w", err)
	}
	if snapshot == nil || source.RunInputSnapshotID == "" || source.RunInputSnapshotHash == "" ||
		source.RunInputSnapshotID != snapshot.ID || source.RunInputSnapshotHash != snapshot.SnapshotHash ||
		result.RunInputSnapshotID != snapshot.ID || result.RunInputSnapshotHash != snapshot.SnapshotHash {
		return nil, errors.New("source task input snapshot is stale or does not match this invocation")
	}
	receipt := latestSuccessfulExecutionReceipt(source, runID)
	if receipt == nil || strings.TrimSpace(receipt.ActionInvocationID) == "" {
		return nil, errors.New("source task has no valid action invocation receipt for this run")
	}
	if receipt.RuntimeOutputsHash != result.RuntimeOutputsHash || receipt.RunInputSnapshotID != snapshot.ID ||
		receipt.RunInputSnapshotHash != snapshot.SnapshotHash ||
		receipt.MaterializedActionPayloadHash != source.MaterializedActionPayloadHash ||
		!equalStringMaps(receipt.BoundInputs, source.BoundInputs) {
		return nil, errors.New("source action receipt identity does not match the completed task occurrence")
	}
	root, ok := result.RuntimeOutputs[strings.TrimSpace(spec.TaskOutputName)]
	if !ok {
		return nil, fmt.Errorf("runtime output %q does not exist", spec.TaskOutputName)
	}
	inputs := make(map[string]ResolvedRunInput, len(snapshot.Inputs))
	for _, input := range snapshot.Inputs {
		inputs[input.Name] = input
	}
	evidence := make([]TaskOutputAssertionResult, 0, len(spec.Assertions))
	for index, assertion := range spec.Assertions {
		item, assertionErr := evaluateTaskOutputAssertion(root, assertion, inputs, source, spec.TaskOutputName)
		evidence = append(evidence, item)
		if assertionErr != nil {
			return evidence, fmt.Errorf("assertion %d: %w", index, assertionErr)
		}
	}
	return evidence, nil
}

func uniqueTaskOutputSource(items []*TodoItem, logicalID, runID string) (*TodoItem, error) {
	logicalID = strings.TrimSpace(logicalID)
	normalizedID := normalizeTaskReferenceID(logicalID)
	var matches []*TodoItem
	for _, item := range items {
		if item == nil || (normalizeTaskReferenceID(item.ID) != normalizedID && normalizeTaskReferenceID(item.PlanTaskID) != normalizedID && normalizeTaskReferenceID(item.ContractID) != normalizedID) {
			continue
		}
		if item.Status != TaskDone {
			continue
		}
		if latestSuccessfulExecutionReceipt(item, runID) == nil {
			continue
		}
		matches = append(matches, item)
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf("source-task %q resolved to %d current-run terminal occurrences; exactly one is required", logicalID, len(matches))
	}
	return matches[0], nil
}

func evaluateTaskOutputAssertion(root any, assertion TaskOutputAssertion, inputs map[string]ResolvedRunInput, source *TodoItem, output string) (TaskOutputAssertionResult, error) {
	evidence := TaskOutputAssertionResult{
		SourceTaskID: source.ID, SourceOccurrence: source.OccurrenceRevision,
		Output: output, Pointer: assertion.Pointer, Op: assertion.Op,
		RunInputSnapshotID: source.RunInputSnapshotID,
	}
	actual, err := resolveJSONPointer(root, assertion.Pointer)
	if err != nil {
		return evidence, fmt.Errorf("pointer %q could not be resolved: %w", assertion.Pointer, err)
	}
	evidence.ActualHash, err = hashCanonicalAssertionValue(actual)
	if err != nil {
		return evidence, fmt.Errorf("pointer %q is not canonical JSON: %w", assertion.Pointer, err)
	}

	var passed bool
	switch assertion.Op {
	case "exists":
		passed = true
	case "non_empty":
		passed = taskResultValueNonEmpty(actual)
	case "equals":
		evidence.ExpectedHash, err = hashCanonicalAssertionValue(assertion.Value)
		passed = err == nil && equalJSONValues(actual, assertion.Value)
	case "minimum", "maximum":
		evidence.ExpectedHash, err = hashCanonicalAssertionValue(assertion.Value)
		actualNumber, actualOK := toExactJSONNumber(actual)
		expectedNumber, expectedOK := toExactJSONNumber(assertion.Value)
		if err == nil && actualOK && expectedOK {
			comparison := actualNumber.Cmp(expectedNumber)
			passed = assertion.Op == "minimum" && comparison >= 0 || assertion.Op == "maximum" && comparison <= 0
		}
	case "min_items":
		evidence.ExpectedHash, err = hashCanonicalAssertionValue(assertion.Value)
		want, wantOK := taskResultAssertionInt(assertion.Value)
		count, countOK := taskResultValueItemCount(actual)
		passed = err == nil && wantOK && countOK && count >= want
	case "contains_scalar":
		evidence.ExpectedHash, err = hashCanonicalAssertionValue(assertion.Value)
		items, itemsOK := actual.([]any)
		if itemsOK && err == nil {
			for _, item := range items {
				if equalJSONValues(item, assertion.Value) {
					passed = true
					break
				}
			}
		}
	case "equals_input", "equals_input_hash":
		input, exists := inputs[strings.TrimSpace(assertion.Input)]
		if !exists {
			return evidence, fmt.Errorf("frozen input %q does not exist", assertion.Input)
		}
		if source.BoundInputs[input.Name] != input.ValueHash {
			return evidence, fmt.Errorf("source task was not bound to frozen input %q", input.Name)
		}
		if assertion.Op == "equals_input_hash" {
			evidence.ExpectedHash, err = hashCanonicalAssertionValue(input.ValueHash)
			actualHash, ok := actual.(string)
			passed = err == nil && ok && actualHash == input.ValueHash
		} else {
			evidence.ExpectedHash = input.ValueHash
			var expected any
			if decodeErr := decodeSingleJSON(input.CanonicalValue, &expected); decodeErr != nil {
				return evidence, fmt.Errorf("decode frozen input %q: %w", input.Name, decodeErr)
			}
			passed = equalJSONValues(actual, expected)
		}
	}
	if err != nil {
		return evidence, err
	}
	evidence.Passed = passed
	if !passed {
		return evidence, fmt.Errorf("pointer %q did not satisfy %s", assertion.Pointer, assertion.Op)
	}
	return evidence, nil
}

func hashCanonicalAssertionValue(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return runInputHash(encoded), nil
}

func equalStringMaps(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}
