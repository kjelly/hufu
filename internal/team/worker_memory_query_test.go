package team

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/tools"
)

func newWorkerMemoryQueryCoordinator(t *testing.T, criticMode agent.WorkerMemoryMode) (*Coordinator, string) {
	t.Helper()
	c, repo := wp4SetupCoordinator(t, t.TempDir())
	c.session.Agents = map[string]*agent.AgentDef{
		"critic":   {Name: "critic", Role: "worker", MemoryID: "critic", Memory: agent.WorkerMemoryPolicy{Mode: criticMode, MaxItems: 10, MaxTokens: 5000}},
		"reviewer": {Name: "reviewer", Role: "worker", MemoryID: "reviewer", Memory: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession}},
	}
	base := c.contextScope()
	scoped := func(branch, agentID string) contextstore.Scope {
		scope := base
		scope.BranchID, scope.AgentID = branch, agentID
		return scope
	}
	wp3AppendItems(t, repo,
		contextstore.ContextItem{ID: "shared", Kind: contextstore.ContextDecision, Content: "shared review finding", Scope: base},
		contextstore.ContextItem{ID: "critic-own", Kind: contextstore.ContextObservation, Content: "critic private review finding", Scope: scoped("main", "critic")},
		contextstore.ContextItem{ID: "reviewer-own", Kind: contextstore.ContextObservation, Content: "reviewer private review finding", Scope: scoped("main", "reviewer")},
		contextstore.ContextItem{ID: "critic-other-branch", Kind: contextstore.ContextObservation, Content: "critic other branch review finding", Scope: scoped("other", "critic")},
	)
	critic := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "critic", Desc: "confirm finding"}})[0]
	return c, critic.ID
}

func runMemoryQuery(t *testing.T, c *Coordinator, todoID string) fantasy.ToolResponse {
	t.Helper()
	ctx := context.WithValue(context.Background(), todoIDKey{}, todoID)
	response, err := (&canonicalMemoryQueryTool{coordinator: c}).Run(ctx, fantasy.ToolCall{Input: `{"query":"review finding","n":10}`})
	if err != nil {
		t.Fatalf("memory_query: %v", err)
	}
	return response
}

// The coordinator-wide scope returned only "shared" here: the worker's own
// private memory was unreachable.
func TestWorkerMemoryQueryUsesTheWorkerScope(t *testing.T) {
	c, criticID := newWorkerMemoryQueryCoordinator(t, agent.WorkerMemorySession)
	response := runMemoryQuery(t, c, criticID)
	if response.IsError {
		t.Fatalf("worker memory_query failed: %s", response.Content)
	}
	for _, want := range []string{"critic-own", "shared"} {
		if !strings.Contains(response.Content, "(id: "+want+")") {
			t.Fatalf("worker result lacks %s:\n%s", want, response.Content)
		}
	}
	for _, leaked := range []string{"reviewer-own", "critic-other-branch"} {
		if strings.Contains(response.Content, "(id: "+leaked+")") {
			t.Fatalf("worker result leaked %s, outside the worker's agent and branch scope:\n%s", leaked, response.Content)
		}
	}
}

func TestWorkerMemoryQueryHonorsMemoryPolicyAndIdentity(t *testing.T) {
	cases := []struct {
		name    string
		mode    agent.WorkerMemoryMode
		todoID  func(string) string
		want    string
		wantErr bool
	}{
		{name: "memory off for the agent", mode: agent.WorkerMemoryOff, todoID: func(id string) string { return id }, want: "Memory recall is disabled for this agent."},
		{name: "unknown calling task", mode: agent.WorkerMemorySession, todoID: func(string) string { return "missing" }, want: "requires the calling task", wantErr: true},
		{name: "coordinator keeps the coordinator scope", mode: agent.WorkerMemorySession, todoID: func(string) string { return CoordTodoID }, want: "(id: shared)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, criticID := newWorkerMemoryQueryCoordinator(t, tc.mode)
			response := runMemoryQuery(t, c, tc.todoID(criticID))
			if response.IsError != tc.wantErr || !strings.Contains(response.Content, tc.want) {
				t.Fatalf("response = error %v %q, want error %v containing %q", response.IsError, response.Content, tc.wantErr, tc.want)
			}
		})
	}
}

func TestScopedMemoryQueryIsAllowedOnlyForUnboundWorkers(t *testing.T) {
	memoryQuery := &canonicalMemoryQueryTool{}
	def := &agent.AgentDef{Name: "critic", Tools: "view"}
	cases := []struct {
		name       string
		bound      bool
		wantDenied bool
	}{
		{name: "unbound worker", bound: false},
		{name: "workset-bound worker", bound: true, wantDenied: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), tools.ArtifactPathPolicyKey, workerArtifactPathPolicy(def, tc.bound, nil))
			denial := artifactScopeToolDenial(ctx, "memory_query", memoryQuery)
			if (denial != "") != tc.wantDenied {
				t.Fatalf("denial = %q, want denied %v", denial, tc.wantDenied)
			}
		})
	}
}
