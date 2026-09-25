package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

const dispatchTestEntries = `  collect-debug-bundle:
    description: Collect bounded runtime diagnostics for one service.
    capability: diagnostics
    type: collect_debug_bundle
    agent: runtime-engineer
    side-effect: none
    input-schema:
      type: object
      properties:
        service:
          type: string
        count:
          type: integer
      required-properties: [service]
      additional-properties: false
    access:
      discover: [runtime-engineer, network-engineer]
      propose: [runtime-engineer, network-engineer]
    invocation:
      max-invocations: 2
  restart-service:
    description: Restart one service.
    capability: diagnostics
    type: restart_service
    agent: runtime-engineer
    side-effect: workspace_write
    recovery: manual
    input-schema:
      type: object
      properties:
        service:
          type: string
      required-properties: [service]
      additional-properties: false
    access:
      discover: [runtime-engineer]
      propose: [runtime-engineer]
    invocation:
      require-proposal: true
`

// dispatchTestCoordinator builds a real coordinator for a catalog team with a
// durable event store and a recording provider for the diagnostics capability.
func dispatchTestCoordinator(t *testing.T, header string, prepare func(*TeamSession)) (*Coordinator, *envRecordingActionProvider) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	provider := &envRecordingActionProvider{}
	teamDir := writeActionCatalogTeam(t, actionCatalogTestManifest("default-llm-backend: ollama\n"+header, dispatchTestEntries))
	c := newActionCatalogCoordinator(t, t.TempDir(), teamDir, func(session *TeamSession) {
		session.ProviderRegistry.Register("diagnostics", provider)
		if prepare != nil {
			prepare(session)
		}
	})
	c.initEventStore()
	if c.eventStore == nil || c.durableBranchID() == "" {
		t.Fatal("dispatch fixture has no durable event store")
	}
	return c, provider
}

func catalogRequest(agentName, id, arguments string) TaskDef {
	return TaskDef{Agent: agentName, Goal: "run " + id, CatalogInvocation: &CatalogInvocation{ID: id, Arguments: json.RawMessage(arguments)}}
}

func TestDecodeCatalogTaskDefs(t *testing.T) {
	valid := `{"agent":"runtime-engineer","goal":"collect","catalog_action":{"id":"collect-debug-bundle","arguments":{"service":"api"}}}`
	tests := []struct {
		name     string
		task     string
		workflow bool
		wantCode string
	}{
		{name: "valid", task: valid},
		{name: "missing arguments default to an empty object", task: `{"agent":"a","goal":"g","catalog_action":{"id":"x"}}`},
		{name: "dynamic depends_on", task: `{"agent":"a","goal":"g","depends_on":[0],"catalog_action":{"id":"x"}}`},
		{name: "workflow depends_on", task: `{"agent":"a","goal":"g","depends_on":[0],"catalog_action":{"id":"x"}}`, workflow: true, wantCode: teamActionTaskFieldForbidden},
		{name: "side_effect", task: `{"agent":"a","goal":"g","side_effect":"none","catalog_action":{"id":"x"}}`, wantCode: teamActionTaskFieldForbidden},
		{name: "Side_Effect", task: `{"agent":"a","goal":"g","Side_Effect":"none","catalog_action":{"id":"x"}}`, wantCode: teamActionTaskFieldForbidden},
		{name: "id", task: `{"agent":"a","goal":"g","id":"t1","catalog_action":{"id":"x"}}`, wantCode: teamActionTaskFieldForbidden},
		{name: "contract_id", task: `{"agent":"a","goal":"g","contract_id":"c","catalog_action":{"id":"x"}}`, wantCode: teamActionTaskFieldForbidden},
		{name: "max_retries", task: `{"agent":"a","goal":"g","max_retries":3,"catalog_action":{"id":"x"}}`, wantCode: teamActionTaskFieldForbidden},
		{name: "verify", task: `{"agent":"a","goal":"g","verify":"true","catalog_action":{"id":"x"}}`, wantCode: teamActionTaskFieldForbidden},
		{name: "legacy task alias", task: `{"agent":"a","task":"g","catalog_action":{"id":"x"}}`, wantCode: teamActionTaskFieldForbidden},
		{name: "exact duplicate key", task: `{"agent":"a","agent":"b","goal":"g","catalog_action":{"id":"x"}}`, wantCode: teamActionTaskInvalid},
		{name: "case duplicate key", task: `{"agent":"a","Agent":"b","goal":"g","catalog_action":{"id":"x"}}`, wantCode: teamActionTaskInvalid},
		{name: "case duplicate catalog key", task: `{"agent":"a","goal":"g","catalog_action":{"id":"x","ID":"y"}}`, wantCode: teamActionTaskInvalid},
		{name: "unknown catalog field", task: `{"agent":"a","goal":"g","catalog_action":{"id":"x","capability":"c"}}`, wantCode: teamActionTaskInvalid},
		{name: "null arguments", task: `{"agent":"a","goal":"g","catalog_action":{"id":"x","arguments":null}}`, wantCode: teamActionArgumentsInvalid},
		{name: "array arguments", task: `{"agent":"a","goal":"g","catalog_action":{"id":"x","arguments":["api"]}}`, wantCode: teamActionArgumentsInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tasks, err := decodeModelTaskDefsForMode([]byte(`{"tasks":[`+tt.task+`]}`), tt.workflow)
			if tt.wantCode == "" {
				if err != nil || len(tasks) != 1 || tasks[0].CatalogInvocation == nil {
					t.Fatalf("decode = %#v, err %v", tasks, err)
				}
				return
			}
			var dispatchErr *teamActionDispatchError
			if !errors.As(err, &dispatchErr) || dispatchErr.Code != tt.wantCode || dispatchErr.Index != 0 {
				t.Fatalf("decode error = %v, want %s", err, tt.wantCode)
			}
		})
	}
	tasks, err := decodeModelTaskDefs([]byte(`{"tasks":[{"agent":"a","goal":"g","side_effect":"none"}]}`))
	if err != nil || len(tasks) != 1 || tasks[0].CatalogInvocation != nil || tasks[0].SideEffect != SideEffectNone {
		t.Fatalf("ordinary task decode changed: %#v, err %v", tasks, err)
	}
}

func TestCompileCatalogActionTasksRejections(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*Coordinator)
		tasks   []TaskDef
		want    string
	}{
		{name: "catalog absent", prepare: func(c *Coordinator) { c.session.ActionCatalog = nil }, tasks: []TaskDef{catalogRequest("runtime-engineer", "collect-debug-bundle", `{"service":"api"}`)}, want: teamActionCatalogAbsent},
		{name: "initial batch pending", prepare: func(c *Coordinator) {
			c.session.Config.Delegation.RequireExactInitialBatch = true
			c.session.Config.Delegation.InitialBatch = []string{"network-engineer"}
			c.sessionData.DelegationPhase = DelegationPhaseInitialPending
		}, tasks: []TaskDef{catalogRequest("runtime-engineer", "collect-debug-bundle", `{"service":"api"}`)}, want: teamActionInitialBatchPending},
		{name: "no journal", prepare: func(c *Coordinator) { _ = c.eventStore.Close(); c.eventStore = nil; c.SetEventJournal(nil) }, tasks: []TaskDef{catalogRequest("runtime-engineer", "collect-debug-bundle", `{"service":"api"}`)}, want: teamActionJournalRequired},
		{name: "unknown action", tasks: []TaskDef{catalogRequest("runtime-engineer", "missing", `{}`)}, want: teamActionUnknown},
		{name: "agent mismatch", tasks: []TaskDef{catalogRequest("network-engineer", "collect-debug-bundle", `{"service":"api"}`)}, want: teamActionAgentMismatch},
		{name: "phase forbids workspace_write", prepare: func(c *Coordinator) { c.phaseWorkflow = &runtimeWorkflow{enabled: true, state: PhasePrepare} }, tasks: []TaskDef{catalogRequest("runtime-engineer", "restart-service", `{"service":"api"}`)}, want: teamActionPhaseForbidden},
		{name: "unattended", prepare: func(c *Coordinator) { c.unattended = true }, tasks: []TaskDef{catalogRequest("runtime-engineer", "collect-debug-bundle", `{"service":"api"}`)}, want: teamActionUnattendedDenied},
		{name: "arguments invalid", tasks: []TaskDef{catalogRequest("runtime-engineer", "collect-debug-bundle", `{"service":1}`)}, want: teamActionArgumentsInvalid},
		{name: "arguments not redaction stable", tasks: []TaskDef{catalogRequest("runtime-engineer", "collect-debug-bundle", `{"service":"api_key=sk-abcdefghijklmnopqrstuvwxyz0123456789"}`)}, want: teamActionArgumentsNotStable},
		{name: "duplicate in batch", tasks: []TaskDef{
			catalogRequest("runtime-engineer", "collect-debug-bundle", `{"service":"api"}`),
			catalogRequest("runtime-engineer", "collect-debug-bundle", `{"service": "api"}`),
		}, want: teamActionDuplicateInBatch},
		{name: "proposal required", tasks: []TaskDef{catalogRequest("runtime-engineer", "restart-service", `{"service":"api"}`)}, want: teamActionProposalRequired},
		{name: "budget across the batch", tasks: []TaskDef{
			catalogRequest("runtime-engineer", "collect-debug-bundle", `{"service":"a"}`),
			catalogRequest("runtime-engineer", "collect-debug-bundle", `{"service":"b"}`),
			catalogRequest("runtime-engineer", "collect-debug-bundle", `{"service":"c"}`),
		}, want: teamActionBudgetExceeded},
		{name: "budget with admitted todos", prepare: func(c *Coordinator) {
			for range 2 {
				item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "runtime-engineer", Desc: "old"}})[0]
				item.CatalogAction = &CatalogActionBinding{ActionID: "collect-debug-bundle"}
			}
		}, tasks: []TaskDef{catalogRequest("runtime-engineer", "collect-debug-bundle", `{"service":"a"}`)}, want: teamActionBudgetExceeded},
		{name: "decision profile override", prepare: func(c *Coordinator) { c.decisionProfileOverride = "standard" }, tasks: []TaskDef{catalogRequest("runtime-engineer", "collect-debug-bundle", `{"service":"api"}`)}, want: teamActionDecisionProfileForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, provider := dispatchTestCoordinator(t, "", nil)
			if tt.prepare != nil {
				tt.prepare(c)
			}
			_, err := c.compileCatalogActionTasks(tt.tasks)
			var dispatchErr *teamActionDispatchError
			if !errors.As(err, &dispatchErr) || dispatchErr.Code != tt.want {
				t.Fatalf("compile error = %v, want %s", err, tt.want)
			}
			if provider.executed != 0 {
				t.Fatal("a rejected dispatch started the provider")
			}
		})
	}
}

func TestCompileCatalogActionTaskComesFromTheCatalog(t *testing.T) {
	c, _ := dispatchTestCoordinator(t, "", nil)
	entry, _ := c.session.ActionCatalog.Lookup("restart-service")
	argumentsHash := runInputHash([]byte(`{"service":"api"}`))
	c.actionProposals.mu.Lock()
	for i := range maxLinkedProposals + 3 {
		assessment := "candidate"
		if i == 1 {
			assessment = "recommended"
		}
		c.actionProposals.addLocked(TeamActionProposal{
			TeamActionProposedPayload: TeamActionProposedPayload{ProposalID: fmt.Sprintf("tap_%02d", i), ActionID: entry.ID, EntryHash: entry.Hash, ArgumentsHash: argumentsHash, Assessment: assessment},
			IdempotencyKey:            fmt.Sprintf("key-%d", i),
		})
	}
	c.actionProposals.mu.Unlock()
	request := catalogRequest("runtime-engineer", "restart-service", `{ "service" : "api" }`)
	request.DependsOn = []int{1}
	compiled, err := c.compileCatalogActionTasks([]TaskDef{request, {Agent: "network-engineer", Goal: "inspect"}})
	if err != nil {
		t.Fatal(err)
	}
	task := compiled[0]
	if task.Action == nil || task.Action.Capability != "diagnostics" || task.Action.Type != "restart_service" || task.Action.Payload != `{"service":"api"}` ||
		task.SideEffect != SideEffectWorkspaceWrite || task.Recovery != RecoveryManual || task.DecisionProfile != DecisionProfileOff ||
		task.MaxRetries != 0 || task.CatalogInvocation != nil || task.Agent != "runtime-engineer" || len(task.DependsOn) != 1 || compiled[1].Goal != "inspect" {
		t.Fatalf("compiled task = %#v", task)
	}
	binding := task.CatalogAction
	if binding.ActionID != entry.ID || binding.EntryHash != entry.Hash || binding.CatalogHash != c.session.ActionCatalog.Hash ||
		binding.ArgumentsHash != argumentsHash || len(binding.ProposalIDs) != maxLinkedProposals || binding.ProposalIDs[0] != "tap_00" {
		t.Fatalf("binding = %#v", binding)
	}
}

func TestCatalogActionSchemaProperty(t *testing.T) {
	c, _ := dispatchTestCoordinator(t, "", nil)
	property := c.catalogActionSchemaProperty()
	encoded, _ := json.Marshal(property)
	if !strings.Contains(string(encoded), `"enum":["collect-debug-bundle","restart-service"]`) || strings.Contains(string(encoded), "oneOf") {
		t.Fatalf("catalog_action property = %s", encoded)
	}
	info := (&runAgentsTool{coordinator: c}).Info()
	if !strings.Contains(fmt.Sprint(info.Parameters), "catalog_action") {
		t.Fatal("agent tool schema lacks catalog_action")
	}
	c.phaseWorkflow = &runtimeWorkflow{enabled: true, state: PhasePrepare}
	encoded, _ = json.Marshal(c.catalogActionSchemaProperty())
	if !strings.Contains(string(encoded), `"enum":["collect-debug-bundle"]`) {
		t.Fatalf("prepare-phase property = %s, want only the none entry", encoded)
	}
	c.phaseWorkflow = &runtimeWorkflow{enabled: true, state: PhaseAudit}
	if property := c.catalogActionSchemaProperty(); property != nil {
		t.Fatalf("audit phase exposes catalog_action %#v", property)
	}

	many := &ActionCatalogSnapshot{}
	for i := range maxCatalogSchemaEnum + 1 {
		many.Entries = append(many.Entries, ActionCatalogEntry{ID: fmt.Sprintf("action-%02d", i), SideEffect: SideEffectNone})
	}
	large := &Coordinator{session: &TeamSession{ActionCatalog: many}, phaseWorkflow: &runtimeWorkflow{}}
	encoded, _ = json.Marshal(large.catalogActionSchemaProperty())
	if strings.Contains(string(encoded), `"enum"`) || !strings.Contains(string(encoded), "team_action_list") {
		t.Fatalf("large catalog property = %s, want no enum", encoded)
	}
	plain := &Coordinator{session: &TeamSession{}}
	if plain.catalogActionSchemaProperty() != nil {
		t.Fatal("team without a catalog exposes catalog_action")
	}
}

func TestCatalogTasksSkipGoalContractsAndDuplicateSuppression(t *testing.T) {
	session := &TeamSession{
		Config:        agent.TeamConfig{Delegation: agent.DelegationPolicy{BindTaskGoalContracts: true}},
		ContractTasks: []TaskDef{{ID: "static", Agent: "runtime-engineer", WhenGoalContains: "collect", Verify: "true"}},
	}
	catalogTask := TaskDef{Agent: "runtime-engineer", Goal: "collect diagnostics", CatalogAction: &CatalogActionBinding{ActionID: "collect"}}
	bound, _, err := CompileTaskGoalContracts(session, []TaskDef{catalogTask})
	if err != nil || bound[0].ContractID != "" || bound[0].Verify != "" {
		t.Fatalf("goal contract took over a catalog task: %#v, err %v", bound, err)
	}

	c, _ := dispatchTestCoordinator(t, "", nil)
	existing := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "runtime-engineer", Desc: "collect diagnostics", Goal: "collect diagnostics"}})[0]
	existing.CatalogAction = &CatalogActionBinding{ActionID: "collect-debug-bundle"}
	if match := c.findExistingTodoDuplicate(context.Background(), "runtime-engineer", "collect diagnostics", nil, "", ""); match != nil {
		t.Fatalf("a catalog todo suppressed an ordinary task: %#v", match)
	}
	_, duplicates, _ := c.checkDuplicateTasks(context.Background(), []TaskDef{catalogTask, catalogTask})
	if len(duplicates) != 0 {
		t.Fatalf("catalog tasks with the same goal were deduplicated: %v", duplicates)
	}
}

func TestCatalogTasksSkipResultContractsAndRoutes(t *testing.T) {
	c, _ := dispatchTestCoordinator(t, "", func(session *TeamSession) {
		session.AgentResultContracts = map[string]ResultContractRef{"runtime-engineer": {ID: "report"}}
	})
	def := c.session.Agents["runtime-engineer"]
	ordinary, err := c.canonicalizeTaskOccurrence(TaskDef{Agent: "runtime-engineer", Goal: "g"}, def, "ollama/a")
	if err != nil || ordinary.ResultContract == nil {
		t.Fatalf("ordinary task result contract = %#v, err %v", ordinary.ResultContract, err)
	}
	catalog, err := c.canonicalizeTaskOccurrence(TaskDef{Agent: "runtime-engineer", Goal: "g", CatalogAction: &CatalogActionBinding{ActionID: "x"}}, def, "ollama/a")
	if err != nil || catalog.ResultContract != nil || catalog.ExecutionRoute != nil {
		t.Fatalf("catalog task contract/route = %#v / %#v, err %v", catalog.ResultContract, catalog.ExecutionRoute, err)
	}
}

func TestCatalogInvocationIDIsDeterministic(t *testing.T) {
	c, _ := dispatchTestCoordinator(t, "", nil)
	binding := &CatalogActionBinding{ActionID: "collect-debug-bundle", EntryHash: "sha256:e", ArgumentsHash: "sha256:a"}
	assign := func(id string) string {
		tasks := []TaskDef{{CatalogAction: binding.clone()}}
		specs := []TodoSpec{{}}
		c.assignCatalogInvocationIDs(tasks, specs, []string{id})
		if tasks[0].CatalogAction.InvocationID != specs[0].CatalogAction.InvocationID {
			t.Fatal("task and spec invocation IDs differ")
		}
		return tasks[0].CatalogAction.InvocationID
	}
	first := assign("7")
	if !strings.HasPrefix(first, "tai_") || len(first) != len("tai_")+32 || assign("7") != first || assign("8") == first {
		t.Fatalf("invocation IDs are not a stable function of the occurrence: %q", first)
	}
}

func TestCoordinatorListReportsDispatchability(t *testing.T) {
	c, _ := dispatchTestCoordinator(t, "", nil)
	for range 2 {
		item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "runtime-engineer", Desc: "old"}})[0]
		item.CatalogAction = &CatalogActionBinding{ActionID: "collect-debug-bundle"}
	}
	summaries := make(map[string]coordinatorTeamActionSummary)
	for _, entry := range c.session.ActionCatalog.Entries {
		summaries[entry.ID] = c.coordinatorTeamActionSummary(entry)
	}
	if got := summaries["collect-debug-bundle"]; got.InvocationsUsed != 2 || got.DispatchableNow || got.BlockedReason != "budget" {
		t.Fatalf("exhausted action summary = %#v", got)
	}
	if got := summaries["restart-service"]; got.InvocationsUsed != 0 || !got.DispatchableNow || got.BlockedReason != "" {
		t.Fatalf("available action summary = %#v", got)
	}
}

func TestCatalogDispatchFixtureIsIsolated(t *testing.T) {
	// Guards the fixture: every dispatch test writes into its own workspace.
	c, _ := dispatchTestCoordinator(t, "", nil)
	if _, err := os.Stat(filepath.Join(c.session.Workspace, "runtime")); err != nil {
		t.Fatalf("dynamic catalog team has no runtime workspace: %v", err)
	}
	if !slices.Contains(agentToolNames(c.buildOrchestratorToolsFor(nil)), teamActionGetToolName) {
		t.Fatal("dispatch fixture lacks coordinator catalog tools")
	}
}
