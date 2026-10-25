package team

import (
	"context"
	"encoding/json/v2"
	"slices"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"go.yaml.in/yaml/v3"
)

func staticEdgesFixture() *TeamSession {
	return &TeamSession{
		Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{BindTaskGoalContracts: true}},
		ContractTasks: []TaskDef{
			{ID: "draft", Agent: "writer", Phase: PhaseVerify, SideEffect: SideEffectNone, Recovery: RecoveryRetry},
			{ID: "check", Agent: "checker", Phase: PhaseVerify, DependsOnContracts: []string{"draft"}, OnFailureContract: "draft", MaxRetries: 1, OnFailureClasses: []TaskFailureClass{FailureVerify}, SideEffect: SideEffectNone, Recovery: RecoveryRetry},
		},
	}
}

func TestStaticContractEdgesResolveNamesAndOverrideModelIndices(t *testing.T) {
	session := staticEdgesFixture()
	for _, reversed := range []bool{false, true} {
		tasks := []TaskDef{{Agent: "writer", ContractID: "draft", DependsOn: []int{99}, OnFailure: new(99), Pipeline: true}, {Agent: "checker", ContractID: "check", DependsOn: []int{99}, OnFailure: new(99), Pipeline: true}}
		producer, check := 0, 1
		if reversed {
			slices.Reverse(tasks)
			producer, check = 1, 0
		}
		bound, effective, err := CompileTaskGoalContracts(session, tasks)
		if err != nil {
			t.Fatal(err)
		}
		if len(bound[producer].DependsOn) != 0 || bound[producer].OnFailure != nil || bound[producer].Pipeline || bound[check].Pipeline || !slices.Equal(bound[check].DependsOn, []int{producer}) || bound[check].OnFailure == nil || *bound[check].OnFailure != producer {
			t.Fatalf("wrong resolved graph: %+v", bound)
		}
		if effective[check].OnFailureContract != "draft" || !slices.Equal(effective[check].DependsOnContracts, []string{"draft"}) || bound[check].MaxRetries != 1 || !slices.Equal(bound[check].OnFailureClasses, []TaskFailureClass{FailureVerify}) {
			t.Fatalf("static graph/recovery identity missing: %+v", effective)
		}
		if err := validateOnFailureTargets(bound); err != nil || detectTaskCycle(bound) {
			t.Fatalf("invalid resolved graph: %v", err)
		}

	}
}

func TestStaticContractEdgesRejectIncompleteOrDuplicatedBatch(t *testing.T) {
	for _, tasks := range [][]TaskDef{
		{{Agent: "writer", ContractID: "draft"}},
		{{Agent: "checker", ContractID: "check"}},
		{{Agent: "writer", ContractID: "draft"}, {Agent: "writer", ContractID: "draft"}, {Agent: "checker", ContractID: "check"}},
	} {
		if _, _, err := CompileTaskGoalContracts(staticEdgesFixture(), tasks); err == nil {
			t.Fatalf("partial or duplicate group accepted: %+v", tasks)
		}
	}
}

func TestStaticContractEdgesPreflight(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*TeamSession)
	}{
		{"missing target", func(s *TeamSession) { s.ContractTasks[1].DependsOnContracts = []string{"absent"} }},
		{"empty dependency", func(s *TeamSession) { s.ContractTasks[1].DependsOnContracts = []string{""} }},
		{"duplicate dependency", func(s *TeamSession) { s.ContractTasks[1].DependsOnContracts = []string{"draft", "DRAFT"} }},
		{"self dependency", func(s *TeamSession) { s.ContractTasks[1].DependsOnContracts = []string{"check"} }},
		{"cycle", func(s *TeamSession) { s.ContractTasks[0].DependsOnContracts = []string{"check"} }},
		{"unrelated return", func(s *TeamSession) { s.ContractTasks[1].DependsOnContracts = nil }},
		{"different phase", func(s *TeamSession) { s.ContractTasks[0].Phase = PhasePrepare }},
		{"fanout target", func(s *TeamSession) { s.ContractTasks[0].FanOut = &FanOutSpec{} }},
		{"fanout source", func(s *TeamSession) { s.ContractTasks[1].FanOut = &FanOutSpec{} }},
		{"binding disabled", func(s *TeamSession) { s.Config.Delegation.BindTaskGoalContracts = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := staticEdgesFixture()
			tc.edit(s)
			if err := validateStaticContractEdges(s); err == nil {
				t.Fatal("invalid declaration accepted")
			}
		})
	}
	if err := validateStaticContractEdges(staticEdgesFixture()); err != nil {
		t.Fatal(err)
	}
	var contract TaskDef
	if err := yaml.Unmarshal([]byte("depends-on-contracts: [draft]\non-failure-contract: draft\n"), &contract); err != nil || contract.OnFailureContract != "draft" || !slices.Equal(contract.DependsOnContracts, []string{"draft"}) {
		t.Fatalf("YAML declaration: %+v %v", contract, err)
	}
}

func TestStaticContractEdgesPinExecutionPolicyAndEffectiveHash(t *testing.T) {
	c := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 1, 1)
	s := staticEdgesFixture()
	c.session.ContractTasks = s.ContractTasks
	c.session.Config.Delegation = s.Config.Delegation
	before, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	requests := []TaskDef{{Agent: "writer", ContractID: "draft"}, {Agent: "checker", ContractID: "check"}}
	_, effective, err := CompileTaskGoalContracts(s, requests)
	if err != nil {
		t.Fatal(err)
	}
	s.ContractTasks[1].OnFailureContract = "check"
	after, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	_, changed, err := CompileTaskGoalContracts(s, requests)
	if err != nil {
		t.Fatal(err)
	}
	if before.snapshot.StaticWorkflowHash == "" || before.snapshot.ConfigurationHash == after.snapshot.ConfigurationHash || effective[1].Hash == changed[1].Hash {
		t.Fatal("workflow edge drift did not change frozen identities")
	}
}

func TestHufuCodeReviewDeclaresFinalReportGraph(t *testing.T) {
	s := loadHufuCodeReviewTeam(t)
	requests, err := decodeModelTaskDefsForMode([]byte(`{"tasks":[{"agent":"synthesizer","contract_id":"synthesize-review","goal":"Synthesize accepted evidence"},{"agent":"reviewer","contract_id":"verify-review-report","goal":"Audit the report"}]}`), true)
	if err != nil {
		t.Fatal(err)
	}
	bound, _, err := CompileTaskGoalContracts(s, requests)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(bound[1].DependsOn, []int{0}) || bound[1].OnFailure == nil || *bound[1].OnFailure != 0 || bound[1].MaxRetries != 1 || !slices.Equal(bound[1].OnFailureClasses, []TaskFailureClass{FailureVerify}) {
		t.Fatalf("report workflow not bound from YAML: %+v", bound[1])
	}
	for _, task := range requests {
		if _, _, err := CompileTaskGoalContracts(s, []TaskDef{task}); err == nil {
			t.Fatalf("standalone final report contract admitted: %s", task.ContractID)
		}
	}
}

func TestStaticContractEdgesAdmissionPersistsResolvedGraph(t *testing.T) {
	c := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 1, 1)
	s := staticEdgesFixture()
	s.ContractTasks[0].Agent = "worker"
	s.ContractTasks[1].Agent = "codex-worker"
	c.session.ContractTasks = s.ContractTasks
	c.session.Config.Delegation = s.Config.Delegation
	// The fixture creates its coordinator before the contracts are assigned.
	// Freeze the complete test configuration before admission, as startup does.
	policy, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	c.executionPolicy = policy
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Cancel after the real creation transaction and before scheduler launch,
	// so this persistence test never calls a live model or external backend.
	c.reportStatus = func(event StatusEvent) {
		if event.Type == "todos_updated" && len(event.Todos) == 2 {
			cancel()
		}
	}
	_, dispatchErr := c.ExecuteTasks(ctx, []TaskDef{{Agent: "worker", ContractID: "draft", Goal: "Prepare draft"}, {Agent: "codex-worker", ContractID: "check", Goal: "Check draft"}})
	items := c.taskTracker.TodoList().Items()
	if len(items) != 2 {
		t.Fatalf("want two admitted occurrences, got %d: %v", len(items), dispatchErr)
	}
	if items[1].OnFailure != items[0].ID || !slices.Equal(items[1].DependsOn, []string{items[0].ID}) || items[1].MaxRetries != 1 || len(items[0].DependsOn) != 0 {
		t.Fatalf("durable graph differs from declaration: %+v", items[1])
	}
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	var restored []*TodoItem
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if restored[1].OnFailure != restored[0].ID || !slices.Equal(restored[1].DependsOn, []string{restored[0].ID}) {
		t.Fatal("durable graph lost during restore")
	}
}
