package team

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
	runtimeTools "github.com/kjelly/hufu/internal/tools"
)

func TestBindEvidenceSources(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer"}, {Agent: "reviewer"}, {Agent: "reviewer"}})
	done, running, failedResult := items[0], items[1], items[2]
	done.Status, done.TypedResult = TaskDone, &TaskResult{Status: TaskResultStatusSuccess}
	running.Status = TaskInProgress
	failedResult.Status, failedResult.TypedResult = TaskDone, &TaskResult{Status: TaskResultStatusFailed}
	critic := func(evidence ...string) TaskDef {
		return TaskDef{Agent: "critic", ContractID: "critic-review", EvidenceFrom: evidence, Execution: ExecutionContract{RequiresEvidence: true}}
	}
	cases := []struct {
		name    string
		task    TaskDef
		want    []string
		wantErr string
	}{
		{name: "completed source", task: critic(" "+done.ID+" ", done.ID), want: []string{done.ID}},
		{name: "contract requires evidence", task: critic(), wantErr: `contract "critic-review" requires evidence_from`},
		{name: "unknown task", task: critic("999"), wantErr: `evidence_from "999" is not a task of this run`},
		{name: "unfinished task", task: critic(running.ID), wantErr: "not a completed task with a successful result"},
		{name: "unsuccessful result", task: critic(failedResult.ID), wantErr: "not a completed task with a successful result"},
		{name: "too many sources", task: critic("1", "2", "3", "4", "5", "6", "7", "8", "9"), wantErr: "at most 8 are allowed"},
		{name: "optional evidence omitted", task: TaskDef{Agent: "reviewer"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bound, err := c.bindEvidenceSources([]TaskDef{tc.task})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("bindEvidenceSources: %v", err)
			}
			if !slices.Equal(bound[0].EvidenceFrom, tc.want) {
				t.Fatalf("evidence_from = %q, want %q", bound[0].EvidenceFrom, tc.want)
			}
		})
	}
}

func TestEvidenceFromGivesCriticTheReviewedInputsAndFinding(t *testing.T) {
	c, _, reviewer, ref, receipt := newWorksetAuthorizationFixture(t)
	reviewer.WorksetReceipt = cloneWorksetReceipt(receipt)
	reviewResult := &TaskResult{
		TaskID: reviewer.ID, Attempt: 1, Agent: "reviewer", Status: TaskResultStatusSuccess, Summary: "one blocker",
		Findings: []Finding{{Category: "correctness", Severity: "blocker", Summary: "prefilter skips redaction"}},
	}
	reviewer.Status, reviewer.TypedResult = TaskDone, reviewResult
	c.taskResults[reviewer.ID] = reviewResult
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{PlanTaskID: "critic", Agent: "critic", EvidenceFrom: []string{reviewer.ID}},
		{PlanTaskID: "other", Agent: "critic"},
		{PlanTaskID: "dependent", Agent: "critic", DependsOn: []string{reviewer.ID}},
	})
	critic, other, dependent := items[0], items[1], items[2]

	scope, err := c.buildArtifactAccessScope(critic.ID, 1)
	if err != nil {
		t.Fatalf("critic scope: %v", err)
	}
	if !slices.ContainsFunc(scope.AuthorizedRefs, func(r ArtifactRef) bool { return r.ID == ref.ID }) {
		t.Fatalf("critic scope does not authorize the reviewed input %s: %#v", ref.ID, scope.AuthorizedRefs)
	}
	ctx := context.WithValue(context.Background(), todoIDKey{}, critic.ID)
	ctx = context.WithValue(ctx, executionAttemptKey{}, 1)
	ctx = context.WithValue(ctx, artifactAccessScopeKey, cloneArtifactAccessScope(scope))
	ctx = runtimeTools.SetToolsAllowed(ctx, []string{"view"})
	view := runtimeTools.NewViewTool(runtimeTools.WithArtifactOpener(c.openArtifactRef))
	response, err := view.Run(ctx, fantasy.ToolCall{Input: `{"artifact_ref":"` + ref.ID + `"}`})
	if err != nil || response.IsError || !strings.Contains(response.Content, "assigned input") {
		t.Fatalf("critic view of the reviewed input: response=%#v err=%v", response, err)
	}

	dependencies := c.dependencyResultsForTask(critic.ID)
	if len(dependencies) != 1 || dependencies[0].TaskID != reviewer.ID {
		t.Fatalf("critic dependency results = %#v, want the review", dependencies)
	}
	rendered := FormatDependencyResults(dependencies)
	for _, want := range []string{"prefilter skips redaction", "severity: blocker", ref.ID} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("critic context lacks %q:\n%s", want, rendered)
		}
	}

	// Only evidence_from grants the reviewed inputs; an ordinary dependency
	// keeps receiving just the dependency's result artifacts.
	for _, item := range []*TodoItem{other, dependent} {
		itemCtx := context.WithValue(context.Background(), todoIDKey{}, item.ID)
		if _, err := c.openArtifactRef(itemCtx, ref.ID); err == nil {
			t.Fatalf("task %s without evidence_from opened the reviewer's input", item.PlanTaskID)
		}
		scope, err := c.buildArtifactAccessScope(item.ID, 1)
		if err != nil {
			t.Fatalf("scope for %s: %v", item.PlanTaskID, err)
		}
		if slices.ContainsFunc(scope.AuthorizedRefs, func(r ArtifactRef) bool { return r.ID == ref.ID }) {
			t.Fatalf("task %s without evidence_from was authorized for the reviewer's input", item.PlanTaskID)
		}
	}
}

func TestEvidenceFromSurvivesEventReplayAndShadow(t *testing.T) {
	item := todoItemFromSpec(TodoSpec{Agent: "critic", Desc: "confirm finding", Goal: "confirm finding", EvidenceFrom: []string{"12"}}, "15")
	payload, err := json.Marshal(taskTransitionPayload(item))
	if err != nil {
		t.Fatal(err)
	}
	events := []RunEvent{{SchemaVersion: eventStoreSchemaVersion, Type: string(EventTaskCreated), TaskID: item.ID, Payload: payload}}
	replayed, err := ReplayTodoList(events)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(replayed) != 1 || !slices.Equal(replayed[0].EvidenceFrom, []string{"12"}) {
		t.Fatalf("replayed evidence_from = %#v, want [12]; a resumed critic would lose its evidence", replayed)
	}
	if got := taskDefFromTodoItem(replayed[0]).EvidenceFrom; !slices.Equal(got, []string{"12"}) {
		t.Fatalf("task definition evidence_from = %q", got)
	}
	if err := CompareCanonicalProjection(&SessionData{Tasks: []*TodoItem{item}}, events); err != nil {
		t.Fatalf("checkpoint/event shadow rejected evidence_from parity: %v", err)
	}
	if !slices.Equal(item.DependsOn, nil) {
		t.Fatalf("evidence_from leaked into depends_on: %q", item.DependsOn)
	}
}
