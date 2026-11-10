package team

import (
	"encoding/json/v2"
	"errors"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func dependencyViewSession() *TeamSession {
	return &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{BindTaskGoalContracts: true}},
		Agents: map[string]*agent.AgentDef{"producer": {Name: "producer", Role: "worker"}, "worker": testWorkerDef()},
		ContractTasks: []TaskDef{
			{ID: "produce", Agent: "producer"},
			{ID: "consume", Agent: "worker", DependsOnContracts: []string{"produce"}, DependencyResultView: &DependencyResultViewSpec{Mode: "selected", Fields: []string{"verification", "summary"}}},
		},
	}
}

func TestDependencyResultViewContractBinding(t *testing.T) {
	for _, initial := range []bool{false, true} {
		t.Run(map[bool]string{false: "named", true: "initial"}[initial], func(t *testing.T) {
			session := dependencyViewSession()
			session.Config.Delegation.BindTaskGoalContracts = !initial
			session.Config.Delegation.BindInitialTaskContracts = initial
			session.Config.Delegation.InitialBatch = []string{"producer", "worker"}
			requested := []TaskDef{{Agent: "producer", ContractID: "produce", Goal: "source"}, {Agent: "worker", ContractID: "consume", Goal: "use source"}}
			compile := CompileTaskGoalContracts
			if initial {
				compile = CompileInitialTaskContracts
			}
			bound, effective, err := compile(session, requested)
			if err != nil {
				t.Fatal(err)
			}
			if len(bound) != 2 || len(effective) != 2 {
				t.Fatalf("bound=%v, effective=%v", bound, effective)
			}
			if bound[0].ContractRevision != 1 || bound[0].DependencyResultView != nil || bound[1].ContractRevision != 2 || effective[1].Revision != 2 {
				t.Fatalf("contract revisions are not opt-in: %+v", bound)
			}
			expected := []string{"summary", "verification"}
			if !reflect.DeepEqual(bound[1].DependencyResultView.Fields, expected) || !reflect.DeepEqual(effective[1].DependencyResultView.Fields, expected) || !reflect.DeepEqual(bound[1].DependsOn, []int{0}) {
				t.Fatalf("policy or edges not bound: %+v", bound[1])
			}
			firstHash := bound[1].ContractHash
			session.ContractTasks[1].DependencyResultView.Fields = []string{"summary", "verification"}
			again, _, err := compile(session, requested)
			if err != nil || again[1].ContractHash != firstHash {
				t.Fatalf("author field ordering changed hash: %v", err)
			}
			session.ContractTasks[1].DependencyResultView.Fields = []string{"summary"}
			changed, _, err := compile(session, requested)
			if err != nil || changed[1].ContractHash == firstHash {
				t.Fatalf("policy change did not change hash: %v", err)
			}
			bound[1].DependencyResultView.Fields[0] = "artifacts"
			if effective[1].DependencyResultView.Fields[0] != "summary" {
				t.Fatal("effective contract aliases bound task")
			}
			session.ContractTasks[1].DependencyResultView = nil
			absent, absentEffective, err := compile(session, requested)
			if err != nil {
				t.Fatal(err)
			}
			session.ContractTasks[1].DependencyResultView = &DependencyResultViewSpec{Mode: "full"}
			full, fullEffective, err := compile(session, requested)
			if err != nil || !reflect.DeepEqual(absent, full) || !reflect.DeepEqual(absentEffective, fullEffective) {
				t.Fatalf("full changed legacy identity: %v", err)
			}
		})
	}
}

func TestDependencyResultViewContractRejectsUnsupportedTasks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*TeamSession)
	}{
		{"no binding", func(s *TeamSession) { s.Config.Delegation.BindTaskGoalContracts = false }},
		{"unknown worker", func(s *TeamSession) { delete(s.Agents, "worker") }},
		{"coordinator", func(s *TeamSession) { s.Agents["worker"].Role = "coordinator" }},
		{"sidecar worker", func(s *TeamSession) { s.Agents["worker"].Role = "sidecar" }},
		{"no dependencies", func(s *TeamSession) { s.ContractTasks[1].DependsOnContracts = nil }},
		{"requires evidence", func(s *TeamSession) { s.ContractTasks[1].Execution.RequiresEvidence = true }},
		{"cross batch evidence", func(s *TeamSession) { s.ContractTasks[1].EvidenceFrom = []string{"1"} }},
		{"result schema", func(s *TeamSession) { s.ContractTasks[1].ResultContractSpec = &agent.ResultContractSpec{} }},
		{"worker schema", func(s *TeamSession) { s.Agents["worker"].ResultContract = &agent.ResultContractSpec{} }},
		{"native action", func(s *TeamSession) { s.ContractTasks[1].Action = &Action{} }},
		{"structured execution", func(s *TeamSession) { s.ContractTasks[1].Execution.Steps = []ExecutionStep{{ID: "read", Tool: "view"}} }},
		{"sidecar", func(s *TeamSession) { s.ContractTasks[1].Sidecar = true }},
		{"plan first", func(s *TeamSession) { s.ContractTasks[1].PlanFirst = true }},
		{"worker plan requirement", func(s *TeamSession) { s.Agents["worker"].Requirements.PlanFirst = true }},
		{"team plan requirement", func(s *TeamSession) { s.Config.Requirements.PlanFirst = true }},
		{"closed sequence", func(s *TeamSession) { s.ContractTasks[1].Execution.ToolSequence = []string{"submit_result"} }},
		{"closed invariant", func(s *TeamSession) {
			s.Config.Delegation.TaskGoalInvariants = []agent.TaskGoalInvariant{{ContractID: "consume", RequiredToolSequence: []string{"submit_result"}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := dependencyViewSession()
			tc.change(session)
			findings := dependencyResultViewFindings(session, 1, session.ContractTasks[1])
			if len(findings) != 1 || findings[0].Code != "dependency_result_view_invalid" {
				t.Fatalf("missing static rejection: %+v", findings)
			}
		})
	}
	for _, key := range []string{"dependency_result_view", "dependency-result-view", "DependencyResultView", "DEPENDENCY_RESULT_VIEW"} {
		t.Run(key, func(t *testing.T) {
			input := `{"tasks":[{"agent":"worker","goal":"work","` + key + `":{"mode":"selected","fields":["summary"]}}]}`
			if _, err := decodeModelTaskDefs([]byte(input)); err == nil {
				t.Fatal("model supplied static view policy")
			}
		})
	}
}

func TestDependencyResultViewPersistsCanonicalOccurrence(t *testing.T) {
	session := dependencyViewSession()
	tasks, _, err := CompileTaskGoalContracts(session, []TaskDef{{Agent: "producer", ContractID: "produce", Goal: "produce"}, {Agent: "worker", ContractID: "consume", Goal: "consume"}})
	if err != nil {
		t.Fatal(err)
	}
	task := tasks[1]
	item := todoItemFromSpec(TodoSpec{Agent: task.Agent, Desc: task.Goal, Goal: task.Goal, ContractID: task.ContractID, ContractHash: task.ContractHash, ContractRevision: task.ContractRevision, DependencyResultView: task.DependencyResultView.clone(), DependsOn: []string{"1"}}, "2")
	payload, err := json.Marshal(taskTransitionPayloadWithCoordinator(item, nil))
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToTodoList([]RunEvent{{Type: string(EventTaskCreated), TaskID: "2", Payload: payload}})
	if len(replayed) != 1 || !reflect.DeepEqual(replayed[0].DependencyResultView, item.DependencyResultView) {
		t.Fatalf("event lost policy: %+v", replayed)
	}
	checkpoint, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var restored TodoItem
	if err := json.Unmarshal(checkpoint, &restored); err != nil {
		t.Fatal(err)
	}
	for _, copy := range []*TodoItem{&restored, cloneTodoItem(item), replayed[0]} {
		recovered := taskDefFromTodoItem(copy)
		if !reflect.DeepEqual(recovered.DependencyResultView, task.DependencyResultView) || recovered.ContractRevision != 2 {
			t.Fatalf("restore lost policy: %+v", recovered)
		}
		projection, err := newTaskOccurrenceProjection(copy)
		if err != nil {
			t.Fatal(err)
		}
		digest, err := decisionTaskInputDigest(projection)
		if err != nil {
			t.Fatal(err)
		}
		projection.DependencyResultView.Fields = []string{"artifacts"}
		mutated, err := decisionTaskInputDigest(projection)
		if err != nil || digest == mutated {
			t.Fatalf("occurrence admission ignores policy: %v", err)
		}
		if copy.DependencyResultView.Fields[0] != "summary" {
			t.Fatal("projection mutation changed durable task")
		}
	}
	if _, err := CompileWorkerContext(t.Context(), WorkerContextInput{TaskDef: task}); !errors.Is(err, errDependencyResultViewUnprepared) {
		t.Fatalf("selected execution silently used full: %v", err)
	}
}

// These values were measured by running the original helpers in an isolated
// b02b302b archive, before adding dependency-result-view. Do not regenerate
// them from the current implementation when changing policy serialization.
func TestDependencyResultViewLegacyIdentityFixture(t *testing.T) {
	task := TaskDef{ID: "consume", Agent: "worker", DependsOnContracts: []string{"produce"}}
	const contractHash = "e498a0bf132c51107107fc5530fdb783c6d3be22347c5b000ae9a529da7ccba7"
	hash, err := effectiveContractHash(task.ID, task.Agent, task.Execution, task.OutputMode, task.SideEffect, task.Recovery, task.MaxRetries, task.Action, task.FanOut, task.Optional, task)
	if err != nil || hash != contractHash {
		t.Fatalf("legacy full contract hash changed: %q, %v", hash, err)
	}
	projection := TaskOccurrenceProjection{ID: "2", Agent: "worker", Desc: "consume", Goal: "consume", ContractID: "consume", ContractHash: hash, ContractRevision: 1, DependsOn: []string{"1"}}
	digest, err := decisionTaskInputDigest(projection)
	if err != nil || digest != "bb42a42ccfd26c78b38dd31092b9f6195837829f2ab3f9122f4acf60338edf24" {
		t.Fatalf("legacy full occurrence digest changed: %q, %v", digest, err)
	}
}
