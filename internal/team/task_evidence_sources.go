package team

import (
	"fmt"
	"slices"
	"strings"
)

// maxEvidenceSources bounds evidence_from. A task that checks other work
// names the few tasks it checks, not a whole run.
const maxEvidenceSources = 8

// bindEvidenceSources validates each task's evidence_from before any TODO
// exists. An evidence task must be a completed task with a successful typed
// result, because its result and the artifacts it was given are what the new
// task reads. A contract with requires-evidence rejects a dispatch without
// one: a critic sent only goal prose has no finding or diff to check, and in
// the 2026-10-01 review run it spent its turns searching memory and runtime
// logs and ended without a result.
func (c *Coordinator) bindEvidenceSources(tasks []TaskDef) ([]TaskDef, error) {
	bound := append([]TaskDef(nil), tasks...)
	for index := range bound {
		task := &bound[index]
		ids := make([]string, 0, len(task.EvidenceFrom))
		for _, raw := range task.EvidenceFrom {
			if id := strings.TrimSpace(raw); id != "" && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		if len(ids) > maxEvidenceSources {
			return nil, fmt.Errorf("tasks[%d] evidence_from names %d tasks; at most %d are allowed", index, len(ids), maxEvidenceSources)
		}
		for _, id := range ids {
			source := c.todoItemByID(id)
			if source == nil {
				return nil, fmt.Errorf("tasks[%d] evidence_from %q is not a task of this run", index, id)
			}
			if source.Status != TaskDone || source.TypedResult == nil || !taskResultStatusIsSuccessful(source.TypedResult.Status) {
				return nil, fmt.Errorf("tasks[%d] evidence_from %q is %s, not a completed task with a successful result", index, id, source.Status)
			}
			if err := c.validateAcceptedDependencyPayload(source); err != nil {
				return nil, fmt.Errorf("tasks[%d] evidence_from %q: %w", index, id, err)
			}
		}
		if task.Execution.RequiresEvidence && len(ids) == 0 {
			return nil, fmt.Errorf("tasks[%d] contract %q requires evidence_from: set it to the ID of the completed task whose result this task checks, such as the review that reported the finding", index, firstNonEmpty(task.ContractID, task.Agent))
		}
		task.EvidenceFrom = nil
		if len(ids) > 0 {
			task.EvidenceFrom = ids
		}
	}
	return bound, nil
}

// resultDependencyIDs lists the tasks whose results a task receives: its
// batch dependencies and its evidence_from tasks.
func resultDependencyIDs(item *TodoItem) []string {
	if item == nil {
		return nil
	}
	ids := append([]string(nil), item.DependsOn...)
	for _, id := range item.EvidenceFrom {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// dependencyReviewedInputs returns the workset inputs a bound dependency was
// given, each re-authorized against that dependency's committed workset
// receipt. An unbound dependent that checks the dependency's result, such as a
// critic, needs the diff and source it reviewed, which are not part of the
// dependency's own result.
func (c *Coordinator) dependencyReviewedInputs(dependency *TodoItem) []ArtifactRef {
	if c == nil || dependency == nil || dependency.WorksetBinding == nil {
		return nil
	}
	refs := make([]ArtifactRef, 0, len(dependency.WorksetBinding.Inputs))
	for _, input := range dependency.WorksetBinding.Inputs {
		if ref, _, ok := c.authorizedWorksetInput(dependency, input.ID); ok {
			refs = append(refs, ref)
		}
	}
	return refs
}

func appendMissingArtifactRefs(refs []ArtifactRef, extra []ArtifactRef) []ArtifactRef {
	for _, ref := range extra {
		if !slices.ContainsFunc(refs, func(existing ArtifactRef) bool { return existing.ID == ref.ID }) {
			ref.Path = ""
			refs = append(refs, ref)
		}
	}
	return refs
}
