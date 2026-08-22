package team

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestFinishRejectsUnresolvedPendingTasks(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker(), sessionData: NewSession(), session: &TeamSession{Config: agent.TeamConfig{Name: "pending-finish"}}}
	c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "still running"}})
	response, err := (&finishTool{coordinator: c}).Run(context.Background(), fantasy.ToolCall{Input: `{"response":"done"}`})
	if err != nil {
		t.Fatalf("finish tool error: %v", err)
	}
	if !response.IsError || !strings.Contains(response.Content, "unresolved or still running") {
		t.Fatalf("finish response = %#v, want pending-task rejection", response)
	}
	if c.finishCalled.Load() {
		t.Fatal("finish marked the run complete despite a pending task")
	}
}

func TestRunAgentsRejectsProviderWorksetStateBeforeTodoCreation(t *testing.T) {
	for _, field := range []string{"workset_binding", "workset_receipt"} {
		t.Run(field, func(t *testing.T) {
			c := &Coordinator{taskTracker: NewTaskTracker()}
			tool := &runAgentsTool{coordinator: c}
			input := `{"tasks":[{"agent":"worker","goal":"inspect","` + field + `":{}}]}`
			response, err := tool.Run(context.Background(), fantasy.ToolCall{Input: input})
			if err != nil {
				t.Fatalf("run_agents error: %v", err)
			}
			if !response.IsError || !strings.Contains(response.Content, "runtime-owned") {
				t.Fatalf("run_agents response = %#v, want runtime-owned rejection", response)
			}
			if got := len(c.taskTracker.TodoList().Items()); got != 0 {
				t.Fatalf("provider-authored %s created %d Todo items", field, got)
			}
		})
	}
}

func TestFinishWritesCanonicalSessionItemNotSTMInCanonicalMode(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	c := &Coordinator{
		contextRepo:  repo,
		projectDir:   "project",
		session:      &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
		sessionData:  NewSession(),
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
		sessionTime:  time.Now(),
	}
	tool := &finishTool{coordinator: c}
	response, err := tool.Run(context.Background(), fantasy.ToolCall{Input: `{"response":"all done"}`})
	if err != nil {
		t.Fatalf("finishTool.Run failed: %v", err)
	}
	if !c.finishCalled.Load() {
		t.Fatal("finish did not complete")
	}
	_ = response
	// The finish summary must be a typed canonical session item, not a direct
	// stm.md mutation that the next projection rebuild would discard.
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: c.contextScope(), Visibility: contextstore.VisibilityExact})
	if err != nil {
		t.Fatal(err)
	}
	foundProgress := false
	for _, item := range items {
		if item.Kind == contextstore.ContextProgress && item.Metadata["legacy_section"] == stmSectionProgress {
			foundProgress = true
		}
	}
	if !foundProgress {
		t.Fatalf("finish did not write a canonical session progress item: %#v", items)
	}
}

func TestCanonicalFinishedResponsePrefersSealedFinishResponse(t *testing.T) {
	c := &Coordinator{}
	c.finishCalled.Store(true)
	c.SetLastRunResult(&RunResult{Response: "# Evidence-backed final review"})

	if got := c.canonicalFinishedResponse("FINISHED:brief post-finish narration"); got != "# Evidence-backed final review" {
		t.Fatalf("canonical finished response = %q, want sealed finish response", got)
	}

	c.finishCalled.Store(false)
	if got := c.canonicalFinishedResponse("FINISHED:ordinary result"); got != "ordinary result" {
		t.Fatalf("unfinished response = %q, want fallback", got)
	}
}

func TestDelegationChain(t *testing.T) {
	tests := []struct {
		name       string
		ctxChain   string
		callerName string
		want       []string
	}{
		{"root call seeds chain from caller", "", "planner", []string{"planner"}},
		{"root call with no caller yields empty chain", "", "", nil},
		{"propagated chain from context wins over caller", "A/B", "B", []string{"A", "B"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.ctxChain != "" {
				ctx = context.WithValue(ctx, delegationChainKey{}, tt.ctxChain)
			}
			got := delegationChain(ctx, tt.callerName)
			if strings.Join(got, "/") != strings.Join(tt.want, "/") {
				t.Errorf("delegationChain() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRunAgentsToolRendersTerminalWorkerFailureAsCoordinatorEvidence(t *testing.T) {
	response := renderRunAgentsToolResponse("## Agent: worker\n**Status**: ERROR\n**Todo ID**: 4", errAllWorkerTasksFailed)
	if response.IsError {
		t.Fatalf("terminal worker outcome was rendered as agent tool error: %#v", response)
	}
	for _, want := range []string{"**Todo ID**: 4", "Replan, reconcile"} {
		if !strings.Contains(response.Content, want) {
			t.Fatalf("response omitted %q: %q", want, response.Content)
		}
	}
}

func TestTerminalUnresolvedWorkerResponseStopsCoordinatorToolTurn(t *testing.T) {
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "non-replayable checkpoint"}})[0]
	tracker.TodoList().UpdateStatus(item.ID, TaskBlocked, "closed task contract failed")
	coord := &Coordinator{taskTracker: tracker}
	coord.SetWrapUp()

	if !coord.terminalUnresolvedRun() {
		t.Fatal("blocked worker in wrap-up must be terminal")
	}
	response := terminalUnresolvedWorkerResponse(coord)
	if !response.IsError || !strings.Contains(response.Content, "non-replayable checkpoint") {
		t.Fatalf("terminal worker response = %#v, want error containing failed task", response)
	}
	result := coord.LastRunResult()
	if result == nil || IsRunOutcomeSuccess(result.Outcome) || result.ExitCode == 0 {
		t.Fatalf("terminal run result = %#v, want nonzero unresolved outcome", result)
	}

	// The policy gate turns an error response from a coordinator tool into the
	// hard stream boundary used by a live orchestrator.
	gated := coord.gatePolicyTools([]fantasy.AgentTool{&recordingTool{name: "agent", resp: response}})[0]
	ctx := context.WithValue(context.Background(), todoIDKey{}, CoordTodoID)
	_, err := gated.Run(ctx, fantasy.ToolCall{Name: "agent"})
	if !errors.Is(err, errCoordinatorToolFailure) {
		t.Fatalf("terminal worker tool response error = %v, want coordinator boundary", err)
	}
}

func TestRunAgentsDescriptionForbidsSuccessfulRedispatch(t *testing.T) {
	tool := &runAgentsTool{coordinator: &Coordinator{
		session: &TeamSession{Agents: map[string]*agent.AgentDef{
			"worker": {Name: "worker"},
		}},
	}}
	description := tool.Info().Description
	for _, want := range []string{"Never redispatch", "team_info", "task_result"} {
		if !strings.Contains(description, want) {
			t.Fatalf("agent tool description missing %q: %s", want, description)
		}
	}
}

func TestRunAgentsToolInfoPinsFreshInitialDelegationSchema(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{Config: agent.TeamConfig{
			Delegation: agent.DelegationPolicy{
				InitialBatch:             []string{"surface", "reader"},
				RequireExactInitialBatch: true,
				BindInitialTaskContracts: true,
			},
		}, Agents: map[string]*agent.AgentDef{
			"surface": {Name: "surface", Role: "worker"},
			"reader":  {Name: "reader", Role: "worker"},
			"planner": {Name: "planner", Role: "worker"},
		}},
		taskTracker: NewTaskTracker(),
		sessionData: NewSession(),
	}

	info := (&runAgentsTool{coordinator: c}).Info()
	tasks, ok := info.Parameters["tasks"].(map[string]any)
	if !ok {
		t.Fatalf("tasks schema = %#v, want object", info.Parameters["tasks"])
	}
	if got := tasks["minItems"]; got != 2 {
		t.Fatalf("initial minItems = %#v, want 2", got)
	}
	if got := tasks["maxItems"]; got != 2 {
		t.Fatalf("initial maxItems = %#v, want 2", got)
	}
	items, ok := tasks["items"].(map[string]any)
	if !ok {
		t.Fatalf("task item schema = %#v, want object", tasks["items"])
	}
	properties, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("task properties = %#v, want object", items["properties"])
	}
	agentSchema, ok := properties["agent"].(map[string]any)
	if !ok {
		t.Fatalf("agent schema = %#v, want object", properties["agent"])
	}
	agents, ok := agentSchema["enum"].([]string)
	if !ok || strings.Join(agents, ",") != "surface,reader" {
		t.Fatalf("fresh agent enum = %#v, want only ordered initial workers", agentSchema["enum"])
	}
	for _, forbidden := range []string{"execution", "output_mode", "context_files"} {
		if _, exists := properties[forbidden]; exists {
			t.Fatalf("fresh initial task schema exposed runtime-bound field %q", forbidden)
		}
	}
	prefix, ok := tasks["prefixItems"].([]map[string]any)
	if !ok || len(prefix) != 2 {
		t.Fatalf("prefixItems = %#v, want ordered schemas for both initial workers", tasks["prefixItems"])
	}
	if !strings.Contains(info.Description, "initial_pending") {
		t.Fatalf("fresh initial agent tool description omitted canonical phase: %q", info.Description)
	}
}

func TestRunAgentsToolInfoHidesForbiddenContextFiles(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{
			ForbidContextFiles: true,
		}}, Agents: map[string]*agent.AgentDef{
			"planner": {Name: "planner", Role: "worker"},
		}},
		taskTracker: NewTaskTracker(),
		sessionData: NewSession(),
	}

	info := (&runAgentsTool{coordinator: c}).Info()
	tasks := info.Parameters["tasks"].(map[string]any)
	items := tasks["items"].(map[string]any)
	properties := items["properties"].(map[string]any)
	if _, exists := properties["context_files"]; exists {
		t.Fatal("context_files exposed despite forbid-context-files delegation policy")
	}
}

func TestCheckDelegationLimits(t *testing.T) {
	tests := []struct {
		name     string
		chain    []string
		selected string
		wantErr  bool
	}{
		{"first hop is fine", nil, "A", false},
		{"distinct chain under the depth cap is fine", []string{"A", "B", "C"}, "D", false},
		{"direct self-delegation is circular", []string{"A"}, "A", true},
		{"case-insensitive self-delegation is circular", []string{"A"}, "a", true},
		{"re-entering an ancestor is circular", []string{"A", "B"}, "A", true},
		{"chain at the depth cap is blocked", []string{"A", "B", "C", "D", "E"}, "F", true},
		{"chain just under the depth cap is allowed", []string{"A", "B", "C", "D"}, "E", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkDelegationLimits(tt.chain, tt.selected)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkDelegationLimits(%v, %q) error = %v, wantErr %v", tt.chain, tt.selected, err, tt.wantErr)
			}
		})
	}
}

// TestDelegationChainPropagatesAcrossHops verifies the fix for the bug where
// the chain used to reset to a single agent name on every nested
// request_agent call (because it was read from the coordinator's mutable
// snapshot, which only ever holds the immediate agent's flat name). It must
// instead accumulate across hops so depth and cycle checks are meaningful.
func TestDelegationChainPropagatesAcrossHops(t *testing.T) {
	// Hop 1: top-level agent "A" delegates to "B".
	ctx := context.Background()
	chain := delegationChain(ctx, "A")
	if err := checkDelegationLimits(chain, "B"); err != nil {
		t.Fatalf("A -> B should be allowed: %v", err)
	}
	subLabel := strings.Join(append(chain, "B"), "/")
	if subLabel != "A/B" {
		t.Fatalf("subLabel = %q, want %q", subLabel, "A/B")
	}

	// Hop 2: "B" (now carrying the propagated chain) tries to delegate back
	// to "A". This must be caught as a cycle.
	ctx2 := context.WithValue(context.Background(), delegationChainKey{}, subLabel)
	chain2 := delegationChain(ctx2, "B")
	if strings.Join(chain2, "/") != "A/B" {
		t.Fatalf("propagated chain = %v, want [A B]", chain2)
	}
	if err := checkDelegationLimits(chain2, "A"); err == nil {
		t.Fatal("expected B -> A to be blocked as a circular delegation, got nil error")
	}
}
