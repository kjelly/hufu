package team

import (
	"encoding/json"
	"fmt"
	"strings"
)

// resolveFactRefs substitutes {Name} in each task's Goal and Constraints
// with the value named by its FactRefs, resolved directly from an earlier
// task's own submitted TaskResult. A reference to a task with no submitted
// result yet, or to a fact/artifact name that task never declared, fails the
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
		goal := t.Goal
		constraints := t.Constraints
		for _, ref := range t.FactRefs {
			value, err := c.resolveFactRef(ref)
			if err != nil {
				return nil, fmt.Errorf("tasks[%d].fact_refs: %w", i, err)
			}
			placeholder := "{" + ref.Name + "}"
			goal = strings.ReplaceAll(goal, placeholder, value)
			constraints = strings.ReplaceAll(constraints, placeholder, value)
		}
		t.Goal = goal
		t.Constraints = constraints
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
	if (fact == "") == (artifact == "") {
		return "", fmt.Errorf("fact_ref %q must set exactly one of fact or artifact", name)
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
