package team

import (
	"cmp"
	"context"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"fmt"
)

type dependencyViewTerminalSource struct {
	ID                 string      `json:"id"`
	Status             TaskStatus  `json:"status"`
	OccurrenceRevision int         `json:"occurrence_revision"`
	TypedResult        *TaskResult `json:"typed_result"`
}

// dependencyViewLineage uses the same verified event and fork-cutoff authority
// as other canonical readers. It never opens or migrates a writable store.
func (c *Coordinator) dependencyViewLineage(ctx context.Context) ([]RunEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store := c.EventStore()
	if store == nil {
		return nil, fmt.Errorf("selected dependency view requires a canonical event store")
	}
	events, err := store.ReadEvents()
	if err != nil {
		return nil, err
	}
	root := c.session.Scope.ControlRoot
	if root == "" {
		root = c.artifactStoreRootPath()
	}
	tree, err := LoadSessionTreeContext(ctx, root)
	if err != nil {
		return nil, err
	}
	branch := cmp.Or(c.recoveryBranch, store.BranchID(), tree.ActiveBranch, "main")
	return ProjectValidatedEventsForBranch(events, tree, branch)
}

func (c *Coordinator) dependencyViewSources(ctx context.Context, consumer *TodoItem, results []TaskResult) ([]DependencyResultViewSource, error) {
	if len(consumer.DependsOn) == 0 || len(consumer.DependsOn) != len(results) {
		return nil, fmt.Errorf("selected dependency view lacks complete same-batch sources")
	}
	lineage, err := c.dependencyViewLineage(ctx)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(consumer.DependsOn))
	for _, id := range consumer.DependsOn {
		if id == "" || allowed[id] {
			return nil, fmt.Errorf("selected dependency view has invalid dependency identities")
		}
		allowed[id] = true
	}
	sources := make([]DependencyResultViewSource, 0, len(results))
	for _, result := range results {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !allowed[result.TaskID] {
			return nil, fmt.Errorf("selected dependency view includes an unauthorized source")
		}
		delete(allowed, result.TaskID)
		item := c.todoItemByID(result.TaskID)
		if item == nil || item.Status != TaskDone || item.TypedResult == nil || item.ParentID != consumer.ParentID {
			return nil, fmt.Errorf("selected dependency source is not an accepted peer task")
		}
		source, err := dependencyViewSourceFromEvents(ctx, lineage, item, result, consumer.WorksetBinding)
		if err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	return sources, nil
}

func dependencyViewSourceFromEvents(ctx context.Context, events []RunEvent, item *TodoItem, result TaskResult, workset *WorksetBinding) (DependencyResultViewSource, error) {
	var matched *DependencyResultViewSource
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return DependencyResultViewSource{}, err
		}
		if event.Type != string(EventTaskCompleted) || event.TaskID != item.ID || event.SchemaVersion != eventStoreSchemaVersion {
			continue
		}
		var terminal dependencyViewTerminalSource
		if err := json.Unmarshal(event.Payload, &terminal, jsonv1.DefaultOptionsV1()); err != nil {
			return DependencyResultViewSource{}, fmt.Errorf("decode dependency source: %w", err)
		}
		if terminal.ID != item.ID || terminal.Status != TaskDone || terminal.OccurrenceRevision != item.OccurrenceRevision {
			continue
		}
		if terminal.TypedResult == nil || terminal.TypedResult.StructuredPayload != nil {
			return DependencyResultViewSource{}, fmt.Errorf("selected dependency source has no eligible canonical typed payload")
		}
		if terminal.TypedResult.TaskID != result.TaskID || terminal.TypedResult.Attempt != result.Attempt || terminal.TypedResult.Agent != result.Agent {
			return DependencyResultViewSource{}, fmt.Errorf("selected dependency source result identity changed")
		}
		canonical := projectDependencyResultForWorker(terminal.TypedResult, workset)
		fullHash := dependencyViewDigest([]byte(FormatDependencyResults([]TaskResult{canonical})))
		if fullHash != dependencyViewDigest([]byte(FormatDependencyResults([]TaskResult{result}))) {
			return DependencyResultViewSource{}, fmt.Errorf("selected dependency source differs from its canonical handoff")
		}
		source := DependencyResultViewSource{BranchID: cmp.Or(event.BranchID, "main"), RunID: event.RunID, TaskID: event.TaskID,
			Attempt: result.Attempt, OccurrenceRevision: terminal.OccurrenceRevision,
			SourceEventID: event.ID, SourceEventSHA256: event.Hash, FullViewSHA256: fullHash}
		if err := validateDependencyViewSource(result, source); err != nil {
			return DependencyResultViewSource{}, err
		}
		if matched != nil && *matched != source {
			return DependencyResultViewSource{}, fmt.Errorf("selected dependency source has ambiguous canonical completion")
		}
		matched = &source
	}
	if matched == nil {
		return DependencyResultViewSource{}, fmt.Errorf("selected dependency source completion is unavailable on this lineage")
	}
	return *matched, nil
}
