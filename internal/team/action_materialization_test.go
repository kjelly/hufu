package team

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterializeActionReplacesExistingPointersCanonically(t *testing.T) {
	snapshot := mustRunInputSnapshot(t, []RunInputDefinition{
		{Name: "review.scope", Schema: RunInputSchema{Type: "object"}, Required: true},
		{Name: "label", Schema: RunInputSchema{Type: "string"}, Required: true},
	}, []RunInputAssignment{
		{Name: "review.scope", RawValue: []byte(`{"count":10,"kind":"last_n"}`), Source: RunInputSourceCLI},
		{Name: "label", RawValue: []byte(`"current"`), Source: RunInputSourceCLI},
	})
	action := Action{Capability: "produce-workset", Type: "prepare", Payload: `{"scope":{"kind":"default"},"labels":["old"]}`}
	bindings := []ActionInputBinding{{Input: "review.scope", Target: "/scope"}, {Input: "label", Target: "/labels/0"}}

	got, identity, err := MaterializeAction(action, bindings, snapshot)
	if err != nil {
		t.Fatalf("MaterializeAction: %v", err)
	}
	if got.Payload != `{"labels":["current"],"scope":{"count":10,"kind":"last_n"}}` {
		t.Fatalf("payload = %s", got.Payload)
	}
	if len(got.InputBindings) != 0 {
		t.Fatalf("materialized provider action retained configuration bindings: %#v", got.InputBindings)
	}
	if identity.RunInputSnapshotID != snapshot.ID || identity.RunInputSnapshotHash != snapshot.SnapshotHash {
		t.Fatalf("identity = %#v", identity)
	}
	if !runInputHashPattern.MatchString(identity.ContractHash) || !runInputHashPattern.MatchString(identity.PayloadHash) {
		t.Fatalf("invalid hashes: %#v", identity)
	}
	if identity.BoundInputs["review.scope"] != runInputHash(snapshot.Inputs[1].CanonicalValue) && identity.BoundInputs["review.scope"] != runInputHash(snapshot.Inputs[0].CanonicalValue) {
		t.Fatalf("bound inputs = %#v", identity.BoundInputs)
	}
	if action.Payload == got.Payload {
		t.Fatal("materialization mutated or reused static payload")
	}
}

func TestMaterializeActionRejectsInvalidBindingContracts(t *testing.T) {
	snapshot := mustRunInputSnapshot(t, []RunInputDefinition{{Name: "scope", Schema: RunInputSchema{Type: "object"}, Required: true}}, []RunInputAssignment{{Name: "scope", RawValue: []byte(`{"count":1}`), Source: RunInputSourceCLI}})
	action := Action{Capability: "review", Type: "prepare", Payload: `{"scope":{"count":0},"other":1}`}
	tests := []struct {
		name     string
		bindings []ActionInputBinding
		want     string
	}{
		{"missing target", []ActionInputBinding{{Input: "scope", Target: "/missing"}}, "does not exist"},
		{"unknown input", []ActionInputBinding{{Input: "absent", Target: "/scope"}}, "not present"},
		{"overlap", []ActionInputBinding{{Input: "scope", Target: "/scope"}, {Input: "scope", Target: "/scope/count"}}, "overlaps"},
		{"bad pointer", []ActionInputBinding{{Input: "scope", Target: "scope"}}, "RFC 6901"},
		{"type mismatch", []ActionInputBinding{{Input: "scope", Target: "/other"}}, "incompatible"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := MaterializeAction(action, test.bindings, snapshot); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestActionInputBindingsAreExcludedFromJSON(t *testing.T) {
	action := Action{Capability: "review", Type: "prepare", Payload: `{}`, InputBindings: []ActionInputBinding{{Input: "scope", Target: "/scope"}}}
	encoded, err := json.Marshal(action)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "input-bindings") || strings.Contains(string(encoded), "input_bindings") || strings.Contains(string(encoded), "scope") {
		t.Fatalf("configuration-only binding crossed JSON boundary: %s", encoded)
	}
	encoded, err = json.Marshal(TaskDef{Agent: "worker", Goal: "review", Action: &action})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "action") || strings.Contains(string(encoded), "input") {
		t.Fatalf("configuration-only action crossed coordinator boundary: %s", encoded)
	}
}

func TestCoordinatorMaterializesActionBeforeOccurrenceAdmissionAndDetectsDrift(t *testing.T) {
	definitions := []RunInputDefinition{{Name: "scope", Schema: RunInputSchema{Type: "object"}, Required: true}}
	snapshot := mustRunInputSnapshot(t, definitions, []RunInputAssignment{{Name: "scope", RawValue: []byte(`{"count":10}`), Source: RunInputSourceCLI}})
	c := &Coordinator{
		session:         &TeamSession{RunInputDefinitions: definitions},
		sessionData:     &SessionData{RunInputSnapshots: []RunInputSnapshot{*snapshot}, ActiveRunInputSnapshotID: snapshot.ID},
		executionPolicy: &executionPolicyState{snapshot: &ExecutionPolicySnapshot{RunInputPolicyHash: runInputHash([]byte("policy"))}},
	}
	static := TaskDef{ID: "review", ContractHash: runInputHash([]byte("static")), Action: &Action{
		Capability: "review", Type: "prepare", Payload: `{"scope":{"count":1}}`,
		InputBindings: []ActionInputBinding{{Input: "scope", Target: "/scope"}},
	}}
	bound, err := c.materializeTaskActions([]TaskDef{static})
	if err != nil {
		t.Fatalf("materializeTaskActions: %v", err)
	}
	got := bound[0]
	if got.Action.Payload != `{"scope":{"count":10}}` || got.RunInputSnapshotID != snapshot.ID || got.ContractHash == static.ContractHash {
		t.Fatalf("materialized task = %#v", got)
	}
	if err := c.validateMaterializedActionIdentity(got); err != nil {
		t.Fatalf("validate materialized action: %v", err)
	}
	tampered := got
	tampered.Action = cloneActionPtr(got.Action)
	tampered.Action.Payload = `{"scope":{"count":11}}`
	if err := c.validateMaterializedActionIdentity(tampered); err == nil || !strings.Contains(err.Error(), "execution_input_drift") {
		t.Fatalf("tampered payload error = %v", err)
	}

	changed := mustRunInputSnapshot(t, definitions, []RunInputAssignment{{Name: "scope", RawValue: []byte(`{"count":20}`), Source: RunInputSourceCLI}})
	c.sessionData = &SessionData{RunInputSnapshots: []RunInputSnapshot{*changed}, ActiveRunInputSnapshotID: changed.ID}
	if err := c.validateMaterializedActionIdentity(got); err == nil || !strings.Contains(err.Error(), "execution_input_drift") {
		t.Fatalf("snapshot drift error = %v", err)
	}
}

func TestValidateTeamTaskContractsRejectsInvalidActionInputBinding(t *testing.T) {
	session := &TeamSession{
		RunInputDefinitions: []RunInputDefinition{{Name: "scope", Schema: RunInputSchema{Type: "object"}, Required: true}},
		ContractTasks: []TaskDef{{Action: &Action{
			Capability: "review", Type: "prepare", Payload: `{"scope":{"count":1}}`,
			InputBindings: []ActionInputBinding{{Input: "missing", Target: "/scope"}},
		}}},
	}
	findings := ValidateTeamTaskContracts(session)
	found := false
	for _, finding := range findings {
		if finding.Code == "action_input_binding_invalid" {
			found = true
		}
	}
	if !found {
		t.Fatalf("findings = %#v", findings)
	}
}

func TestActionInputBindingsSurviveOccurrenceJSONReplay(t *testing.T) {
	action := &Action{Capability: "review", Type: "prepare", Payload: `{"scope":{}}`, InputBindings: []ActionInputBinding{{Input: "scope", Target: "/scope"}}}
	item := todoItemFromSpec(TodoSpec{Action: action, Agent: "reviewer", Goal: "review"}, "1")
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"input-bindings"`) || !strings.Contains(string(encoded), `"action_input_bindings"`) {
		t.Fatalf("occurrence binding encoding = %s", encoded)
	}
	var replayed TodoItem
	if err := json.Unmarshal(encoded, &replayed); err != nil {
		t.Fatal(err)
	}
	reconstructed := taskDefFromTodoItem(&replayed)
	if reconstructed.Action == nil || len(reconstructed.ActionInputBindings) != 1 || reconstructed.ActionInputBindings[0].Target != "/scope" || len(reconstructed.Action.InputBindings) != 0 {
		t.Fatalf("reconstructed task = %#v", reconstructed)
	}
}

func TestTaskExecutionInputCacheIdentitySeparatesMaterializedScopes(t *testing.T) {
	first := TaskDef{RunInputSnapshotHash: runInputHash([]byte("one")), MaterializedActionPayloadHash: runInputHash([]byte("payload-one"))}
	second := TaskDef{RunInputSnapshotHash: runInputHash([]byte("ten")), MaterializedActionPayloadHash: runInputHash([]byte("payload-ten"))}
	if taskExecutionInputCacheIdentity(first) == taskExecutionInputCacheIdentity(second) {
		t.Fatal("different frozen action inputs produced the same cache identity")
	}
	if got := taskExecutionInputCacheIdentity(TaskDef{}); got != "" {
		t.Fatalf("unbound task cache identity = %q", got)
	}
}

func TestLoadTeamContractTasksParsesActionInputBindings(t *testing.T) {
	dir := t.TempDir()
	manifest := `name: review
inputs:
  scope:
    type: object
tasks:
  - id: prepare
    agent: reviewer
    goal: prepare
    action:
      capability: review
      type: prepare
      payload: '{"scope":{}}'
      input-bindings:
        - input: scope
          target: /scope
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	tasks, err := loadTeamContractTasks(dir, nil)
	if err != nil {
		t.Fatalf("loadTeamContractTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Action == nil || len(tasks[0].Action.InputBindings) != 1 || tasks[0].Action.InputBindings[0].Input != "scope" {
		t.Fatalf("tasks = %#v", tasks)
	}
}

func TestExecutionRunInputPolicyHashIncludesActionBindings(t *testing.T) {
	definition := RunInputDefinition{Name: "scope", Schema: RunInputSchema{Type: "object"}, Required: true}
	first := &TeamSession{RunInputDefinitions: []RunInputDefinition{definition}, ContractTasks: []TaskDef{{Action: &Action{
		Capability: "review", Type: "prepare", Payload: `{"scope":{}}`, InputBindings: []ActionInputBinding{{Input: "scope", Target: "/scope"}},
	}}}}
	second := &TeamSession{RunInputDefinitions: []RunInputDefinition{definition}, ContractTasks: []TaskDef{{Action: &Action{
		Capability: "review", Type: "prepare", Payload: `{"scope":{},"other":{}}`, InputBindings: []ActionInputBinding{{Input: "scope", Target: "/other"}},
	}}}}
	firstHash, err := executionRunInputPolicyHash(first)
	if err != nil {
		t.Fatal(err)
	}
	secondHash, err := executionRunInputPolicyHash(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstHash == secondHash {
		t.Fatal("action binding change did not change run input policy hash")
	}
}

func mustRunInputSnapshot(t *testing.T, definitions []RunInputDefinition, assignments []RunInputAssignment) *RunInputSnapshot {
	t.Helper()
	snapshot, err := ResolveRunInputSnapshot(definitions, assignments, "run-1", "invocation-1", "team")
	if err != nil {
		t.Fatalf("ResolveRunInputSnapshot: %v", err)
	}
	return snapshot
}
