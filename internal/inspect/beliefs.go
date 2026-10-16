package inspect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/kjelly/hufu/internal/team"
)

type BeliefExecutionState struct {
	TaskID  string `json:"task_id"`
	Status  string `json:"status"`
	Attempt int    `json:"attempt"`
}

type BeliefsData struct {
	team.BeliefRunState
	Execution []BeliefExecutionState `json:"execution"`
}

// InspectBeliefs uses the same validated selected lineage as other run
// inspectors. It never repairs a source/admission gap or loads team files.
func InspectBeliefs(ctx context.Context, query InspectQuery) (*Envelope, error) {
	if err := query.Validate(KindBeliefs); err != nil {
		return nil, err
	}
	lineage, err := LoadLineage(ctx, query)
	if err != nil {
		return nil, err
	}
	selected, err := selectRun(lineage, query)
	if err != nil {
		return nil, err
	}
	var artifacts team.ArtifactStore
	reader, err := team.OpenFileArtifactStoreReadOnly(query.Workspace, query.Workspace)
	if err == nil {
		artifacts = reader
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: belief artifact store is unreadable", ErrIntegrity)
	}
	view, err := team.ProjectBeliefRun(ctx, selected.raw, artifacts, query.RunID)
	if err != nil {
		return nil, fmt.Errorf("%w: project beliefs: %v", ErrIntegrity, err)
	}
	tasks, err := team.ReplayTodoList(selected.runEvents)
	if err != nil {
		return nil, fmt.Errorf("%w: project belief execution state: %v", ErrIntegrity, err)
	}
	data := BeliefsData{BeliefRunState: view, Execution: []BeliefExecutionState{}}
	found := query.TaskID == ""
	for _, task := range tasks {
		if query.TaskID != "" && task.ID != query.TaskID {
			continue
		}
		found = true
		data.Execution = append(data.Execution, BeliefExecutionState{TaskID: task.ID, Status: string(task.Status), Attempt: task.Retries + 1})
	}
	if !found {
		return nil, fmt.Errorf("%w: task %q in run %q", ErrNotFound, query.TaskID, query.RunID)
	}
	if query.TaskID != "" {
		data.Epochs = slices.DeleteFunc(data.Epochs, func(epoch team.BeliefEpochState) bool { return epoch.Epoch.Scope.TaskID != query.TaskID })
		data.Diagnostics = slices.DeleteFunc(data.Diagnostics, func(diagnostic team.BeliefDiagnostic) bool {
			return diagnostic.TaskID != "" && diagnostic.TaskID != query.TaskID
		})
		data.Suggestions = slices.DeleteFunc(data.Suggestions, func(suggestion team.BeliefSuggestionView) bool {
			return !slices.ContainsFunc(data.Epochs, func(epoch team.BeliefEpochState) bool { return epoch.Epoch.ID == suggestion.Record.EpochID })
		})
	}
	result := envelope(KindBeliefs, query, lineage.BranchID, data)
	if len(data.Models) == 0 {
		result.Diagnostics = append(result.Diagnostics, Diagnostic{Code: "belief_not_recorded", Severity: "info", Message: "No model binding is recorded for this run."})
	}
	for _, model := range data.Models {
		if model.Unavailable != "" {
			result.Integrity.Projection = "unavailable"
		}
	}
	for _, epoch := range data.Epochs {
		if epoch.Unavailable != "" {
			result.Integrity.Projection = "unavailable"
		}
	}
	for _, policy := range data.Policies {
		if policy.Unavailable != "" {
			result.Integrity.Projection = "unavailable"
		}
	}
	for _, suggestion := range data.Suggestions {
		if suggestion.Validation == "unavailable" {
			result.Integrity.Projection = "unavailable"
		}
	}
	return result, nil
}
