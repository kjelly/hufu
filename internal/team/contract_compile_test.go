package team

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestCompileInitialTaskContractsPublishesStableEffectiveIdentity(t *testing.T) {
	session := &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{
		BindInitialTaskContracts: true,
		InitialBatch:             []string{"reader"},
	}}, ContractTasks: []TaskDef{{
		ID: "reader-ack-v1", Agent: "reader", OutputMode: TaskOutputModeVerbatim,
		Execution: ExecutionContract{ToolSequence: []string{"submit_result"}},
	}}}
	bound, effective, err := CompileInitialTaskContracts(session, []TaskDef{{Agent: "reader", Goal: "acknowledge"}})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(effective) != 1 || effective[0].Hash == "" || effective[0].ID != "reader-ack-v1" {
		t.Fatalf("effective contract = %#v", effective)
	}
	if bound[0].ContractHash != effective[0].Hash || bound[0].ContractID != effective[0].ID || bound[0].ContractRevision != effectiveTaskContractRevision {
		t.Fatalf("bound task did not retain effective identity: %#v", bound[0])
	}

	again, secondEffective, err := CompileInitialTaskContracts(session, []TaskDef{{Agent: "reader", Goal: "different prose is allowed"}})
	if err != nil || again[0].ContractHash != bound[0].ContractHash || secondEffective[0].Hash != effective[0].Hash {
		t.Fatalf("effective contract hash is not stable: %#v %#v err=%v", again, secondEffective, err)
	}
}

func TestCompileInitialTaskContractsBindsPartialBatchWhenExactDisabled(t *testing.T) {
	session := &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{
		BindInitialTaskContracts: true,
		InitialBatch:             []string{"reader", "probe"},
	}}, ContractTasks: []TaskDef{
		{Agent: "reader", Execution: ExecutionContract{ToolSequence: []string{"view", "submit_result"}}},
		{Agent: "probe", Execution: ExecutionContract{ToolSequence: []string{"bash", "submit_result"}}},
	}}

	bound, effective, err := CompileInitialTaskContracts(session, []TaskDef{{Agent: "probe", Goal: "inspect"}})
	if err != nil {
		t.Fatalf("compile partial batch: %v", err)
	}
	if len(effective) != 1 || !reflect.DeepEqual(bound[0].Execution.ToolSequence, []string{"bash", "submit_result"}) {
		t.Fatalf("partial contract was not bound: bound=%#v effective=%#v", bound, effective)
	}
}

func TestCompileTaskGoalContractsReplacesCoordinatorExecutionFields(t *testing.T) {
	session := &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{
		BindTaskGoalContracts: true,
	}}, ContractTasks: []TaskDef{{
		ID: "freeze-v1", Agent: "runner", WhenGoalContains: "candidate-freeze",
		OutputMode: TaskOutputModeVerbatim,
		Execution:  ExecutionContract{ForbidArtifacts: true, ToolSequence: []string{"bash", "bash", "submit_result"}},
	}}}
	requested := TaskDef{
		Agent: "runner", Goal: "§3.1 candidate-freeze",
		Execution: ExecutionContract{
			ToolSequence:          []string{"bash", "bash", "submit_result"},
			ToolExpectedExitCodes: [][]int{{}, {}, {}},
		},
	}
	bound, effective, err := CompileTaskGoalContracts(session, []TaskDef{requested})
	if err != nil {
		t.Fatalf("compile goal contract: %v", err)
	}
	if len(effective) != 1 || effective[0].ID != "freeze-v1" || bound[0].Goal != requested.Goal {
		t.Fatalf("goal contract identity/prose = bound=%#v effective=%#v", bound, effective)
	}
	if got := bound[0].Execution.ToolExpectedExitCodes; len(got) != 0 {
		t.Fatalf("coordinator expected-exit-codes survived static binding: %#v", got)
	}
	if !bound[0].Execution.ForbidArtifacts || !reflect.DeepEqual(bound[0].Execution.ToolSequence, []string{"bash", "bash", "submit_result"}) || bound[0].OutputMode != TaskOutputModeVerbatim {
		t.Fatalf("static goal contract was not authoritative: %#v", bound[0])
	}
}

func TestCompileTaskGoalContractsBindsStaticActionAndItsIdentity(t *testing.T) {
	session := &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{
		BindTaskGoalContracts: true,
	}}, ContractTasks: []TaskDef{{
		ID: "apply-v1", Agent: "executor", WhenGoalContains: "apply change", Phase: PhaseExecute,
		Action: &Action{Capability: "structured-actions", Type: "apply", Payload: `{"safe":true}`},
	}}}
	bound, effective, err := CompileTaskGoalContracts(session, []TaskDef{{Agent: "executor", Goal: "apply change"}})
	if err != nil {
		t.Fatalf("compile goal contract: %v", err)
	}
	if len(effective) != 1 || effective[0].Action == nil || bound[0].Action == nil {
		t.Fatalf("static action was not bound: bound=%#v effective=%#v", bound, effective)
	}
	if bound[0].Action == session.ContractTasks[0].Action {
		t.Fatal("bound action aliases mutable team contract")
	}
	if bound[0].Action.Capability != "structured-actions" || bound[0].ContractHash == "" {
		t.Fatalf("bound action identity = %#v", bound[0])
	}
}

func TestCompileTaskGoalContractsBindsUniqueAgentContractAfterParaphrase(t *testing.T) {
	session := &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{
		BindTaskGoalContracts: true,
	}}, ContractTasks: []TaskDef{{
		ID: "author-v1", Agent: "author", WhenGoalContains: "author bundle",
		Execution: ExecutionContract{RequiresResult: true, ToolSequence: []string{"bash", "submit_result"}},
	}}}
	requested := TaskDef{Agent: "author", Goal: "Create the structured-action scenarios", Execution: ExecutionContract{
		ToolSequence: []string{"bash ./invented-script.sh", "submit_result"}, ToolExpectedExitCodes: [][]int{{0}},
	}}

	bound, effective, err := CompileTaskGoalContracts(session, []TaskDef{requested})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(effective) != 1 || effective[0].ID != "author-v1" {
		t.Fatalf("unique agent contract was not selected: %#v", effective)
	}
	if got := bound[0].Execution; !reflect.DeepEqual(got.ToolSequence, []string{"bash", "submit_result"}) || len(got.ToolExpectedExitCodes) != 0 {
		t.Fatalf("model-authored execution fields survived unique binding: %#v", got)
	}
}

func TestCompileTaskGoalContractsReplacesCoordinatorVerificationFields(t *testing.T) {
	session := &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{
		BindTaskGoalContracts: true,
	}}, ContractTasks: []TaskDef{{
		Agent:            "topology-preparer",
		WhenGoalContains: "topology",
		Execution:        ExecutionContract{ToolSequence: []string{"bash", "submit_result"}},
	}}}
	requested := TaskDef{
		Agent:      "topology-preparer",
		Goal:       "rebuild topology",
		Verify:     "test -s receipt.json",
		VerifySpec: &VerificationSpec{Type: VerifyFileExists, Path: "receipt.json"},
	}
	bound, _, err := CompileTaskGoalContracts(session, []TaskDef{requested})
	if err != nil {
		t.Fatalf("compile goal contract: %v", err)
	}
	if bound[0].Verify != "" || bound[0].VerifyMode != "" || bound[0].VerifySpec != nil {
		t.Fatalf("coordinator verification fields survived static contract: %#v", bound[0])
	}
}

// TestCompileTaskGoalContractsBindsStaticProgressCriterionFields guards
// against a gap where a static task contract's Kind/Advances (progress-
// criterion linkage, §8.1) never reached the live dispatched task: the
// goal-selector `agent` tool schema never exposes either field to the model,
// so the static contract is the only place they can originate from. Without
// this, a team that wires acceptance criteria via `advances:` on its task
// contracts would see the no-progress budget's counters never reset, because
// the dispatched task would always carry a zero-value Kind/Advances
// regardless of what the contract declared.
func TestCompileTaskGoalContractsBindsStaticProgressCriterionFields(t *testing.T) {
	session := &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{
		BindTaskGoalContracts: true,
	}}, ContractTasks: []TaskDef{{
		Agent:            "go-reviewer",
		WhenGoalContains: "review batch",
		Execution:        ExecutionContract{ToolSequence: []string{"view", "submit_result"}},
		Kind:             TaskKindOutcome,
		Advances:         []string{"all-batches-reviewed"},
	}}}
	requested := TaskDef{Agent: "go-reviewer", Goal: "review batch-0000"}

	bound, _, err := CompileTaskGoalContracts(session, []TaskDef{requested})
	if err != nil {
		t.Fatalf("compile goal contract: %v", err)
	}
	if bound[0].Kind != TaskKindOutcome {
		t.Fatalf("bound Kind = %q, want the static contract's %q", bound[0].Kind, TaskKindOutcome)
	}
	if !reflect.DeepEqual(bound[0].Advances, []string{"all-batches-reviewed"}) {
		t.Fatalf("bound Advances = %#v, want the static contract's criterion list", bound[0].Advances)
	}

	// The bound slice must not alias the team's own contract, or a later
	// mutation of the dispatched task would corrupt the reusable template.
	bound[0].Advances[0] = "mutated"
	if session.ContractTasks[0].Advances[0] != "all-batches-reviewed" {
		t.Fatalf("bound Advances aliases the team contract: %#v", session.ContractTasks[0].Advances)
	}
}

// TestCompileTaskGoalContractsReplacesCoordinatorProgressCriterionFields
// mirrors TestCompileTaskGoalContractsReplacesCoordinatorVerificationFields
// for Kind/Advances: a static contract that declares neither must clear
// whatever a model-authored task guessed, the same way it already does for
// Verify/VerifySpec.
func TestCompileTaskGoalContractsReplacesCoordinatorProgressCriterionFields(t *testing.T) {
	session := &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{
		BindTaskGoalContracts: true,
	}}, ContractTasks: []TaskDef{{
		Agent:            "topology-preparer",
		WhenGoalContains: "topology",
		Execution:        ExecutionContract{ToolSequence: []string{"bash", "submit_result"}},
	}}}
	requested := TaskDef{
		Agent:    "topology-preparer",
		Goal:     "rebuild topology",
		Kind:     TaskKindRepair,
		Advances: []string{"invented-criterion"},
	}
	bound, _, err := CompileTaskGoalContracts(session, []TaskDef{requested})
	if err != nil {
		t.Fatalf("compile goal contract: %v", err)
	}
	if bound[0].Kind != "" || bound[0].Advances != nil {
		t.Fatalf("coordinator progress-criterion fields survived static contract: %#v", bound[0])
	}
}

func TestCompileTaskGoalContractsMatchesGoalSelectorCaseInsensitively(t *testing.T) {
	session := &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{
		BindTaskGoalContracts: true,
	}}, ContractTasks: []TaskDef{{
		Agent:            "author",
		WhenGoalContains: "AUTHOR STRUCTURED-ACTION BUNDLE",
		Execution:        ExecutionContract{ToolSequence: []string{"bash", "submit_result"}},
	}}}
	bound, effective, err := CompileTaskGoalContracts(session, []TaskDef{{
		Agent: "author",
		Goal:  "Author structured-action bundle",
	}})
	if err != nil {
		t.Fatalf("compile case-insensitive selector: %v", err)
	}
	if len(effective) != 1 || !reflect.DeepEqual(bound[0].Execution.ToolSequence, []string{"bash", "submit_result"}) {
		t.Fatalf("case-insensitive selector did not bind static contract: bound=%#v effective=%#v", bound, effective)
	}
}

func TestCompileTaskGoalContractsRejectsAmbiguousSelectors(t *testing.T) {
	session := &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{
		BindTaskGoalContracts: true,
	}}, ContractTasks: []TaskDef{
		{Agent: "runner", WhenGoalContains: "freeze", Execution: ExecutionContract{ToolSequence: []string{"submit_result"}}},
		{Agent: "runner", WhenGoalContains: "candidate-freeze", Execution: ExecutionContract{ToolSequence: []string{"submit_result"}}},
	}}
	if _, _, err := CompileTaskGoalContracts(session, []TaskDef{{Agent: "runner", Goal: "candidate-freeze"}}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous goal selectors error = %v, want rejection", err)
	}
}

func TestValidateTeamTaskContractsRejectsMissingInitialContract(t *testing.T) {
	session := &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{
		BindInitialTaskContracts: true,
		InitialBatch:             []string{"reader"},
	}}}
	findings := ValidateTeamTaskContracts(session)
	if len(findings) != 1 || findings[0].Code != "initial_contract_missing" {
		t.Fatalf("findings = %#v", findings)
	}
	if messages := sortedContractFindingMessages(findings); len(messages) != 1 || !strings.Contains(messages[0], "reader") {
		t.Fatalf("messages = %#v", messages)
	}
}

func TestValidateTeamTaskContractsRejectsUndersizedCoordinatorRoundBudget(t *testing.T) {
	session := &TeamSession{Config: agent.TeamConfig{MaxRounds: 3, MinimumCoordinatorRounds: 4}}
	findings := ValidateTeamTaskContracts(session)
	if len(findings) != 1 || findings[0].Code != "max_rounds_below_minimum_coordinator_rounds" {
		t.Fatalf("findings = %#v, want coordinator-round budget rejection", findings)
	}

	session.Config.MaxRounds = 4
	if findings := ValidateTeamTaskContracts(session); len(findings) != 0 {
		t.Fatalf("sufficient coordinator-round budget findings = %#v, want none", findings)
	}
}

func TestEffectiveContractIdentityIsIncludedInWorkerProtocol(t *testing.T) {
	instructions := resultProtocolInstructions(TaskDef{
		ContractID:       "reader-ack-v1",
		ContractHash:     "deadbeef",
		ContractRevision: 1,
		Execution:        ExecutionContract{RequiresResult: true, ToolSequence: []string{"submit_result"}},
	}, map[string]bool{"submit_result": true})
	for _, want := range []string{"reader-ack-v1", "deadbeef", "authoritative over any conflicting prose"} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("worker protocol omitted %q: %s", want, instructions)
		}
	}
}
