package team

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// envRecordingActionProvider records the action environment it ran with.
type envRecordingActionProvider struct {
	executed int
	env      ActionEnvironment
}

func (*envRecordingActionProvider) ProviderName() string  { return "env-recording" }
func (*envRecordingActionProvider) Validate(Action) error { return nil }
func (p *envRecordingActionProvider) Execute(ctx context.Context, _ Action) (interface{}, error) {
	p.executed++
	p.env = ActionEnvironmentFromContext(ctx)
	return ActionResult{Outputs: map[string]any{"summary": "ok"}}, nil
}

func testCatalogBinding() *CatalogActionBinding {
	return &CatalogActionBinding{
		ActionID: "collect", EntryHash: "sha256:entry", CatalogHash: "sha256:catalog",
		ArgumentsHash: runInputHash([]byte(`{"service":"api"}`)), InvocationID: "tai_0123456789abcdef", ProposalIDs: []string{"tap_a"},
	}
}

func TestCatalogActionBindingSurvivesDurableRoundTrip(t *testing.T) {
	provider := &envRecordingActionProvider{}
	c, events := newDynamicCatalogCoordinator(t, dynamicCatalogSession(t, provider, true))
	binding := testCatalogBinding()
	task, item := createAdmittedTestTask(t, c, catalogActionTask(binding))
	if !reflect.DeepEqual(item.CatalogAction, binding) {
		t.Fatalf("admitted todo binding = %#v, want %#v", item.CatalogAction, binding)
	}

	stored, err := events.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	var replayed *TodoItem
	for _, candidate := range ReduceToTodoList(stored) {
		if candidate.ID == item.ID {
			replayed = candidate
		}
	}
	if replayed == nil || !reflect.DeepEqual(replayed.CatalogAction, binding) {
		t.Fatalf("replayed todo binding = %#v", replayed)
	}
	rebuilt := taskDefFromTodoItem(replayed)
	if !reflect.DeepEqual(rebuilt.CatalogAction, binding) {
		t.Fatalf("rebuilt task binding = %#v", rebuilt.CatalogAction)
	}
	if err := compareTaskDefWithTodoOccurrence(taskDefFromTodoItem(item), replayed, map[string]int{replayed.ID: 0}); err != nil {
		t.Fatalf("admitted occurrence does not match its replay: %v", err)
	}

	encoded, err := json.Marshal(cloneTodoItem(item))
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint TodoItem
	if err := json.Unmarshal(encoded, &checkpoint); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(checkpoint.CatalogAction, binding) {
		t.Fatalf("checkpoint binding = %#v", checkpoint.CatalogAction)
	}
	cloned := cloneTodoItem(item)
	cloned.CatalogAction.ProposalIDs[0] = "tap_changed"
	if item.CatalogAction.ProposalIDs[0] != "tap_a" {
		t.Fatal("cloneTodoItem shares the catalog binding")
	}

	// The durable path re-derives the TaskDef from the Todo and still runs
	// the provider with the stable invocation ID.
	if _, err := c.executeTask(context.Background(), task, item.ID); err != nil {
		t.Fatalf("durable catalog task failed: %v", err)
	}
	if provider.executed != 1 || provider.env.CatalogInvocationID != binding.InvocationID {
		t.Fatalf("provider executions = %d, catalog invocation ID %q, want 1 and %q", provider.executed, provider.env.CatalogInvocationID, binding.InvocationID)
	}
}

func TestCatalogActionBindingNormalizesEmptyProposalIDs(t *testing.T) {
	provider := &envRecordingActionProvider{}
	c, _ := newDynamicCatalogCoordinator(t, dynamicCatalogSession(t, provider, true))
	binding := testCatalogBinding()
	binding.ProposalIDs = []string{}
	_, item := createAdmittedTestTask(t, c, catalogActionTask(binding))
	if item.CatalogAction.ProposalIDs != nil {
		t.Fatalf("stored proposal IDs = %#v, want nil", item.CatalogAction.ProposalIDs)
	}
	scheduled := taskDefFromTodoItem(item)
	scheduled.CatalogAction = binding
	if err := compareTaskDefWithTodoOccurrence(cloneTaskDef(scheduled), item, map[string]int{item.ID: 0}); err != nil {
		t.Fatalf("empty proposal IDs broke occurrence comparison: %v", err)
	}
}

func TestNonCatalogTasksKeepTheirDurableShape(t *testing.T) {
	item := todoItemFromSpec(TodoSpec{Agent: "worker", Goal: "work", Desc: "work"}, "1")
	payload, err := json.Marshal(taskTransitionPayload(item))
	if err != nil {
		t.Fatal(err)
	}
	shadow, err := json.Marshal(toCanonicalTaskShadow(item))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	for name, encoded := range map[string][]byte{"task_created payload": payload, "canonical shadow": shadow, "checkpoint": checkpoint} {
		if strings.Contains(string(encoded), "catalog_action") {
			t.Fatalf("%s of a non-catalog task mentions catalog_action: %s", name, encoded)
		}
	}
	projection, err := newTaskOccurrenceProjection(item)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := decisionOccurrenceInputDigest(projection)
	if err != nil {
		t.Fatal(err)
	}
	projection.CatalogAction = testCatalogBinding()
	bound, err := decisionOccurrenceInputDigest(projection)
	if err != nil {
		t.Fatal(err)
	}
	if plain == bound {
		t.Fatal("the occurrence digest ignores the catalog binding")
	}
}

func TestCatalogInvocationIDEnvironment(t *testing.T) {
	for _, tt := range []struct {
		id   string
		want bool
	}{{"tai_abc", true}, {"", false}} {
		env := actionCommandEnvironment(WithActionEnvironment(context.Background(), ActionEnvironment{CatalogInvocationID: tt.id}))
		got := slices.ContainsFunc(env, func(value string) bool { return strings.HasPrefix(value, "HUFU_CATALOG_INVOCATION_ID=") })
		if got != tt.want || (tt.want && !slices.Contains(env, "HUFU_CATALOG_INVOCATION_ID="+tt.id)) {
			t.Fatalf("id %q: HUFU_CATALOG_INVOCATION_ID present = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestCatalogActionIntegrityStopsDriftBeforeTheProvider(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*TeamSession, *TaskDef)
		want   string
	}{
		{name: "payload drift", mutate: func(_ *TeamSession, task *TaskDef) { task.Action.Payload = `{"service":"db"}` }, want: "team_action_arguments_drift"},
		{name: "entry hash drift", mutate: func(session *TeamSession, _ *TaskDef) { session.ActionCatalog.Entries[0].Hash = "sha256:other" }, want: "team_action_catalog_drift"},
		{name: "entry removed", mutate: func(session *TeamSession, _ *TaskDef) { session.ActionCatalog.Entries[0].ID = "renamed" }, want: "team_action_catalog_drift"},
		{name: "action type drift", mutate: func(_ *TeamSession, task *TaskDef) { task.Action.Type = "other" }, want: "team_action_catalog_drift"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &envRecordingActionProvider{}
			session := dynamicCatalogSession(t, provider, true)
			c, _ := newDynamicCatalogCoordinator(t, session)
			binding := testCatalogBinding()
			task := catalogActionTask(binding)
			tt.mutate(session, &task)
			_, item := addCatalogActionTodo(c, task)
			_, err := c.executeRuntimeAction(context.Background(), task, item.ID)
			var validation ActionValidationError
			if !errors.As(err, &validation) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("executeRuntimeAction error = %v, want ActionValidationError %s", err, tt.want)
			}
			if provider.executed != 0 {
				t.Fatalf("provider executions = %d, want 0", provider.executed)
			}
		})
	}
}
