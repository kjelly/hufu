package team

import (
	"encoding/json"
	"fmt"
	"strings"
)

// resolveFactRefs substitutes {Name} in each task's Goal and Constraints
// with the value named by its FactRefs, resolved directly from an earlier
// task's submitted TaskResult or receipt-verified runtime output. A reference
// to an unavailable result or a value that task never declared fails the
// whole dispatch (no partial substitution) so a coordinator's mistake is
// caught immediately rather than silently leaving a literal {placeholder} in
// a worker's prompt.
func (c *Coordinator) resolveFactRefs(tasks []TaskDef) ([]TaskDef, error) {
	resolved := make([]TaskDef, len(tasks))
	for i, t := range tasks {
		if len(t.FactRefs) == 0 {
			resolved[i] = t
			continue
		}
		var replacements []string
		for _, ref := range t.FactRefs {
			value, err := c.resolveFactRef(ref)
			if err != nil {
				return nil, fmt.Errorf("tasks[%d].fact_refs: %w", i, err)
			}
			placeholder := "{" + ref.Name + "}"
			replacements = append(replacements, placeholder, value)
		}
		// Substitute once: tokens inside a source value are data, not templates.
		replacer := strings.NewReplacer(replacements...)
		t.Goal = replacer.Replace(t.Goal)
		t.Constraints = replacer.Replace(t.Constraints)
		t.FactRefs = nil
		resolved[i] = t
	}
	return resolved, nil
}

func (c *Coordinator) resolveFactRef(ref FactRef) (string, error) {
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return "", fmt.Errorf("fact_ref requires a non-empty name")
	}
	taskID := strings.TrimSpace(ref.TaskID)
	if taskID == "" {
		return "", fmt.Errorf("fact_ref %q requires a non-empty task_id", name)
	}
	fact := strings.TrimSpace(ref.Fact)
	artifact := strings.TrimSpace(ref.Artifact)
	runtimeOutput := strings.TrimSpace(ref.RuntimeOutput)
	if factRefSelectorCount(ref) != 1 {
		return "", fmt.Errorf("fact_ref %q must set exactly one of fact or artifact or runtime_output", name)
	}
	if runtimeOutput != "" {
		return c.resolveRuntimeOutputRef(ref, runtimeOutput)
	}

	runtimeTaskID, err := c.resolveTaskReference(taskID)
	if err != nil {
		return "", fmt.Errorf("fact_ref %q: task %q has no submitted result yet: %w", name, taskID, err)
	}
	result := c.GetTaskResult(runtimeTaskID)
	if result == nil {
		return "", fmt.Errorf("fact_ref %q: task %q has no submitted result yet", name, taskID)
	}

	if fact != "" {
		value, ok := result.Facts[fact]
		if !ok {
			return "", fmt.Errorf("fact_ref %q: task %q has no fact named %q", name, taskID, fact)
		}
		return stringifyFactValue(value), nil
	}

	for _, a := range result.Artifacts {
		if a.Description == artifact {
			return a.Path, nil
		}
	}
	return "", fmt.Errorf("fact_ref %q: task %q has no artifact named %q", name, taskID, artifact)
}

func factRefSelectorCount(ref FactRef) int {
	count := 0
	for _, selector := range []string{ref.Fact, ref.Artifact, ref.RuntimeOutput} {
		if strings.TrimSpace(selector) != "" {
			count++
		}
	}
	return count
}

func (c *Coordinator) resolveRuntimeOutputRef(ref FactRef, output string) (string, error) {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return "", fmt.Errorf("fact_ref %q: canonical task occurrences are unavailable", ref.Name)
	}
	items := c.taskTracker.TodoList().Items()
	runID := coordinatorRuntimeRunID(c)
	spec := VerificationSpec{
		Type: VerifyTaskOutputAssert, WorksetSourceTask: ref.TaskID, TaskOutputName: output,
		Assertions: []JSONAssertion{{Pointer: "", Op: "exists"}},
	}
	if _, err := ReplayTaskOutputAssertions(items, runID, c.RunInputSnapshot(), spec); err != nil {
		return "", fmt.Errorf("fact_ref %q: runtime output is not canonical: %w", ref.Name, err)
	}
	source, err := uniqueTaskOutputSource(items, ref.TaskID, runID)
	if err != nil {
		return "", err
	}
	return stringifyFactValue(source.TypedResult.RuntimeOutputs[output]), nil
}

// stringifyFactValue renders a fact for text substitution: a plain string is
// used as-is (so a resolved path or ID is not wrapped in JSON quotes), and
// every other JSON type is encoded so a list, number, or object still
// produces a deterministic, literal substitution.
func stringifyFactValue(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
}
