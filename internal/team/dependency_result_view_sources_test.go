package team

import (
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"strings"
	"testing"
)

func TestDependencyResultViewSourceBindsCanonicalCompletion(t *testing.T) {
	result, source := dependencyViewRenderFixture()
	item := &TodoItem{ID: result.TaskID, Agent: result.Agent, Status: TaskDone, OccurrenceRevision: source.OccurrenceRevision, TypedResult: &result}
	payload, err := json.Marshal(dependencyViewTerminalSource{ID: item.ID, Status: TaskDone, OccurrenceRevision: item.OccurrenceRevision, TypedResult: &result}, jsonv1.DefaultOptionsV1())
	if err != nil {
		t.Fatal(err)
	}
	event := RunEvent{SchemaVersion: eventStoreSchemaVersion, ID: source.SourceEventID, Hash: source.SourceEventSHA256, Type: string(EventTaskCompleted), TaskID: item.ID, RunID: "acceptance-run", BranchID: "original-branch", Attempt: 17, Payload: payload}
	for _, tc := range []struct {
		name   string
		change func(*RunEvent, *TodoItem, *TaskResult)
		valid  bool
	}{
		{"canonical", func(*RunEvent, *TodoItem, *TaskResult) {}, true},
		{"revision drift", func(_ *RunEvent, i *TodoItem, _ *TaskResult) { i.OccurrenceRevision++ }, false},
		{"handoff drift", func(_ *RunEvent, _ *TodoItem, r *TaskResult) { r.Summary = "changed" }, false},
		{"attempt drift", func(_ *RunEvent, _ *TodoItem, r *TaskResult) { r.Attempt++ }, false},
		{"wrong task", func(e *RunEvent, _ *TodoItem, _ *TaskResult) { e.TaskID = "other" }, false},
		{"not completed", func(e *RunEvent, _ *TodoItem, _ *TaskResult) { e.Type = string(EventTaskStarted) }, false},
		{"legacy snapshot", func(e *RunEvent, _ *TodoItem, _ *TaskResult) { e.SchemaVersion = eventStoreLegacySchemaVersion }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate, changedItem, changedResult := event, *item, result
			tc.change(&candidate, &changedItem, &changedResult)
			bound, err := dependencyViewSourceFromEvents(t.Context(), []RunEvent{candidate}, &changedItem, changedResult, nil)
			if (err == nil) != tc.valid {
				t.Fatalf("source acceptance mismatch: %v", err)
			}
			if tc.valid && (bound.BranchID != "original-branch" || bound.RunID != "acceptance-run" || bound.Attempt != result.Attempt || bound.Attempt == event.Attempt || !validFullContentHash(bound.FullViewSHA256)) {
				t.Fatal("attribution substituted the consumer branch, another run, or the occurrence attempt")
			}
		})
	}
	other := event
	other.ID, other.Hash = "another-completion", strings.Repeat("e", 64)
	if _, err := dependencyViewSourceFromEvents(t.Context(), []RunEvent{event, other}, item, result, nil); err == nil {
		t.Fatal("ambiguous canonical completions were accepted")
	}
}

func TestDependencyResultViewSourceRespectsBranchCutoff(t *testing.T) {
	for _, test := range []struct {
		name             string
		beforeCompletion bool
		independent      bool
	}{
		{name: "inherited completion"},
		{name: "completion after fork", beforeCompletion: true},
		{name: "independent lineage", independent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, _, _, input := dependencyViewRuntimeFixture(t)
			events, err := c.EventStore().ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			tree := NewSessionTree()
			var branch *SessionBranch
			if test.independent {
				branch, err = tree.CreateRootBranch("independent")
			} else {
				fork := events[1].ID
				if test.beforeCompletion {
					fork = events[0].ID
				}
				branch, err = tree.CreateBranch("child", fork, c.EventStore())
			}
			if err != nil {
				t.Fatal(err)
			}
			tree.ActiveBranch = branch.ID
			if err := SaveSessionTree(c.session.Workspace, tree); err != nil {
				t.Fatal(err)
			}
			c.EventStore().SetBranchID(branch.ID)
			compiled, err := c.compileWorkerContext(t.Context(), input)
			if test.beforeCompletion || test.independent {
				if err == nil {
					t.Fatal("dependency source outside the visible lineage was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range compiled.IncludedItems {
				if item.DependencyResultView != nil && item.DependencyResultView.Sources[0].BranchID != "main" {
					t.Fatal("inherited source was relabeled as a child result")
				}
			}
		})
	}
}
