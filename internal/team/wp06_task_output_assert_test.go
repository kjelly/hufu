package team

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"gopkg.in/yaml.v3"
)

func TestCanonicalizeRuntimeOutputsIsDeterministicBoundedAndDetached(t *testing.T) {
	first, firstHash, err := CanonicalizeRuntimeOutputs(map[string]any{
		" scope ": map[string]any{"satisfied": true, "count": 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, secondHash, err := CanonicalizeRuntimeOutputs(map[string]any{
		"scope": map[string]any{"count": 10, "satisfied": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if firstHash == "" || firstHash != secondHash || first["scope"] == nil || first[" scope "] != nil {
		t.Fatalf("canonical outputs/hash = %#v / %q / %q", first, firstHash, secondHash)
	}

	tooMany := make(map[string]any, maxRuntimeOutputKeys+1)
	for index := 0; index <= maxRuntimeOutputKeys; index++ {
		tooMany[string(rune('a'+index))] = true
	}
	if _, _, err := CanonicalizeRuntimeOutputs(tooMany); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("too many output keys error = %v", err)
	}
	deep := any(true)
	for index := 0; index < maxRuntimeOutputDepth; index++ {
		deep = map[string]any{"next": deep}
	}
	if _, _, err := CanonicalizeRuntimeOutputs(map[string]any{"scope": deep}); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("deep output error = %v", err)
	}
}

func TestRuntimeOutputsRemainValidWhenLearnedSecretsGrow(t *testing.T) {
	const collision = "runtime-output-secret-collision-7f2c9b1e"
	outputs, digest, err := CanonicalizeRuntimeOutputs(map[string]any{
		"documentation_verification": map[string]any{
			"passed":        true,
			"checked_files": []any{collision},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	canonicalVerification := outputs["documentation_verification"].(map[string]any)
	canonicalFiles := canonicalVerification["checked_files"].([]any)
	canonicalFile := canonicalFiles[0]
	result := &TaskResult{
		TaskID: "1", Status: TaskResultStatusSuccess, Source: "runtime",
		RuntimeOutputs: outputs, RuntimeOutputsHash: digest,
	}
	session := NewSession()
	session.Entries = []SessionEntry{{Role: "assistant", Content: "api_token: " + collision}}
	session.Tasks = []*TodoItem{{ID: "1", Status: TaskDone, TypedResult: result}}
	workspace := t.TempDir()
	if err := SaveSession(workspace, session); err != nil {
		t.Fatal(err)
	}
	if err := validateRuntimeTaskResult(result); err != nil {
		t.Fatalf("live runtime result became invalid after learned-secret growth: %v", err)
	}
	reloaded, err := loadSessionQuiet(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded == nil || len(reloaded.Tasks) != 1 || reloaded.Tasks[0].TypedResult == nil {
		t.Fatalf("reloaded session lost runtime result: %#v", reloaded)
	}
	if err := validateRuntimeTaskResult(reloaded.Tasks[0].TypedResult); err != nil {
		t.Fatalf("persisted runtime result became invalid after session redaction: %v", err)
	}
	verification := reloaded.Tasks[0].TypedResult.RuntimeOutputs["documentation_verification"].(map[string]any)
	files := verification["checked_files"].([]any)
	if len(files) != 1 || files[0] != canonicalFile {
		t.Fatalf("persisted canonical runtime outputs = %#v", reloaded.Tasks[0].TypedResult.RuntimeOutputs)
	}

	payload, err := json.Marshal(map[string]any{
		"id": "1", "status": TaskDone, "typed_result": result,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewEventStore(t.TempDir(), "run-runtime-redaction", "session-runtime-redaction")
	if err != nil {
		t.Fatal(err)
	}
	event, err := store.AppendPersisted(RunEvent{Type: "task_completed", Actor: "runtime", TaskID: "1", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	replayed := reduceToTodoList([]RunEvent{event})
	if len(replayed.tasks) != 1 || replayed.tasks[0].TypedResult == nil {
		t.Fatalf("event replay lost runtime result: %#v", replayed.tasks)
	}
	if err := validateRuntimeTaskResult(replayed.tasks[0].TypedResult); err != nil {
		t.Fatalf("event replay invalidated runtime outputs: %v", err)
	}
}

func TestTaskOutputAssertUsesRuntimeOccurrenceAndFrozenInput(t *testing.T) {
	definitions := []RunInputDefinition{{Name: "review.scope", Schema: RunInputSchema{Type: "object"}, Required: true}}
	snapshot := mustRunInputSnapshot(t, definitions, []RunInputAssignment{{
		Name: "review.scope", RawValue: []byte(`{"kind":"last_n","count":10}`), Source: RunInputSourceCLI,
	}})
	scope := map[string]any{
		"requested":            map[string]any{"kind": "last_n", "count": 10},
		"requested_input_hash": snapshot.Inputs[0].ValueHash,
		"satisfied":            true,
		"resolved":             map[string]any{"selected_commit_count": 10},
	}
	outputs, digest, err := CanonicalizeRuntimeOutputs(map[string]any{"scope": scope})
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{
		PlanTaskID: "produce-workset", ContractID: "produce-workset", Agent: "runtime", Desc: "produce",
		RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
		MaterializedActionPayloadHash: "sha256:payload", BoundInputs: map[string]string{"review.scope": snapshot.Inputs[0].ValueHash},
	}})[0]
	item.Status = TaskDone
	item.OccurrenceRevision = 7
	item.TypedResult = &TaskResult{
		TaskID: item.ID, Attempt: 1, Status: TaskResultStatusSuccess, Summary: "produced", Source: "runtime",
		RuntimeOutputs: outputs, RuntimeOutputsHash: digest,
		RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
		MaterializedActionPayloadHash: "sha256:payload", BoundInputs: map[string]string{"review.scope": snapshot.Inputs[0].ValueHash},
	}
	item.ExecutionReceipt = &ExecutionReceipt{
		RunID: "run-wp06", TaskID: item.ID, Attempt: 1, StartedAt: time.Now(), FinishedAt: time.Now(), ExitCode: &zero,
		ProducerID: "runtime", ModelExecutionID: "runtime-1", TranscriptRef: "sha256-transcript",
		ActionInvocationID: "action-1", RuntimeOutputsHash: digest,
		RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
		MaterializedActionPayloadHash: "sha256:payload", BoundInputs: map[string]string{"review.scope": snapshot.Inputs[0].ValueHash},
	}
	item.ExecutionReceipts = []ExecutionReceipt{*item.ExecutionReceipt}
	tracker.TodoList().Restore([]*TodoItem{item})
	c := &Coordinator{
		session: &TeamSession{RunInputDefinitions: definitions}, taskTracker: tracker, executionRunID: "run-wp06",
		sessionData: &SessionData{RunInputSnapshots: []RunInputSnapshot{*snapshot}, ActiveRunInputSnapshotID: snapshot.ID},
	}
	spec := VerificationSpec{
		Type: VerifyTaskOutputAssert, WorksetSourceTask: "produce-workset", TaskOutputName: "scope",
		Assertions: []JSONAssertion{
			{Pointer: "/requested", Op: "equals_input", Input: "review.scope"},
			{Pointer: "/requested_input_hash", Op: "equals_input_hash", Input: "review.scope"},
			{Pointer: "/satisfied", Op: "equals", Value: true},
			{Pointer: "/resolved/selected_commit_count", Op: "minimum", Value: 1},
		},
	}
	result, err := c.executeTaskOutputAssertVerification(context.Background(), spec)
	if err != nil || result.ExitCode != 0 || len(result.TaskOutputAssertions) != 4 {
		t.Fatalf("task output assertion result = %#v, err=%v", result, err)
	}
	for _, evidence := range result.TaskOutputAssertions {
		if !evidence.Passed || evidence.SourceTaskID != item.ID || evidence.RunInputSnapshotID != snapshot.ID || evidence.ActualHash == "" {
			t.Fatalf("assertion evidence = %#v", evidence)
		}
	}
	c.acceptanceSpec = &AcceptanceSpec{Mode: "blocking", Verifications: []VerificationSpec{spec}}
	acceptance, err := c.runAcceptance(context.Background())
	if err != nil || acceptance == nil || !acceptance.Passed {
		t.Fatalf("acceptance task_output_assert = %#v, err=%v", acceptance, err)
	}

	item.TypedResult.RuntimeOutputsHash = "sha256:tampered"
	tracker.TodoList().Restore([]*TodoItem{item})
	if _, err := c.executeTaskOutputAssertVerification(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("tampered digest error = %v", err)
	}
}

func TestRuntimeOutputsReplayWithTaskOccurrenceIdentity(t *testing.T) {
	outputs, digest, err := CanonicalizeRuntimeOutputs(map[string]any{"scope": map[string]any{"satisfied": true}})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"id": "1", "status": TaskDone, "plan_task_id": "produce-workset",
		"action_input_bindings": []ActionInputBinding{{Input: "review.scope", Target: "/scope"}},
		"run_input_snapshot_id": "input-snapshot-1", "run_input_snapshot_hash": "sha256:snapshot",
		"materialized_action_payload_hash": "sha256:payload", "bound_inputs": map[string]string{"review.scope": "sha256:value"},
		"typed_result": &TaskResult{TaskID: "1", Status: TaskResultStatusSuccess, Summary: "done", Source: "runtime", RuntimeOutputs: outputs, RuntimeOutputsHash: digest},
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed := reduceToTodoList([]RunEvent{{Type: "task_completed", TaskID: "1", RunID: "run-1", Payload: payload}})
	if len(replayed.tasks) != 1 || replayed.tasks[0].TypedResult == nil || replayed.tasks[0].TypedResult.RuntimeOutputsHash != digest {
		t.Fatalf("replayed runtime outputs = %#v", replayed.tasks)
	}
	if replayed.tasks[0].RunInputSnapshotID != "input-snapshot-1" || replayed.tasks[0].BoundInputs["review.scope"] != "sha256:value" {
		t.Fatalf("replayed occurrence binding = %#v", replayed.tasks[0])
	}
}

func TestTaskOutputAssertManifestShapeAndScopeLint(t *testing.T) {
	raw := []byte(`type: task_output_assert
source-task: produce-workset
output: scope
assertions:
  - pointer: /requested
    op: equals_input
    input: review.scope
  - pointer: /requested_input_hash
    op: equals_input_hash
    input: review.scope
  - pointer: /satisfied
    op: equals
    value: true
`)
	var spec VerificationSpec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if err := validateVerificationSpec(spec); err != nil {
		t.Fatalf("valid task_output_assert rejected: %v", err)
	}
	definition := RunInputDefinition{Name: "review.scope", Schema: RunInputSchema{Type: "object"}, Required: true}
	session := &TeamSession{
		RunInputDefinitions: []RunInputDefinition{definition},
		Config: agent.TeamConfig{AcceptanceSpec: &agent.AcceptanceSpec{Mode: "blocking", Verifications: []agent.VerificationSpec{
			spec,
			{Type: agent.VerifyWorksetComplete, WorksetSourceTask: "review-workset"},
		}}},
		ContractTasks: []TaskDef{{
			ID: "produce-workset", Action: &Action{InputBindings: []ActionInputBinding{{Input: "review.scope", Target: "/scope"}}},
		}, {
			ID: "review-workset", FanOut: &FanOutSpec{SourceArtifact: FactRef{TaskID: "produce-workset", Artifact: "manifest"}, GoalTemplate: "review {item}"},
		}},
	}
	if codes := findingCodes(ValidateTeamPolicyContracts(session)); codes[FindingWorksetScopeAssertion] {
		t.Fatalf("valid scope assertion linted: %#v", codes)
	}
	session.Config.AcceptanceSpec.Verifications[0].Assertions = session.Config.AcceptanceSpec.Verifications[0].Assertions[:2]
	if codes := findingCodes(ValidateTeamPolicyContracts(session)); !codes[FindingWorksetScopeAssertion] {
		t.Fatalf("missing satisfied assertion was not linted: %#v", codes)
	}

	encoded, err := json.Marshal(spec)
	if err != nil || !strings.Contains(string(encoded), `"output":"scope"`) {
		t.Fatalf("task output spec JSON = %s, err=%v", encoded, err)
	}
}
