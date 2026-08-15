package team

// WP-3 — WorkerMemoryService recall tests.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func wp3SetupRepo(t *testing.T) contextstore.Repository {
	t.Helper()
	repo, err := contextstore.OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { repo.Close() })
	return repo
}

func wp3AppendItems(t *testing.T, repo contextstore.Repository, items ...contextstore.ContextItem) {
	t.Helper()
	if err := repo.Append(context.Background(), items...); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

func wp3ContainsID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func TestWP3_ModeOffReturnsEmptyBundle(t *testing.T) {
	repo := wp3SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)
	bundle, err := svc.Recall(context.Background(), WorkerMemoryRecallRequest{
		WorkerID: "worker-a",
		Scope:    contextstore.Scope{ProjectID: "p", TeamID: "team", AgentID: "worker-a"},
		Query:    "test query",
		Policy:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryOff},
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(bundle.Items) != 0 {
		t.Errorf("mode=off should return 0 items, got %d", len(bundle.Items))
	}
	if !bundle.Trace.Skipped {
		t.Error("mode=off should set Skipped=true")
	}
	if bundle.Trace.SkipReason != "mode=off" {
		t.Errorf("SkipReason = %q, want mode=off", bundle.Trace.SkipReason)
	}
}

func TestWP3_NoRepoReturnsEmptyBundle(t *testing.T) {
	svc := NewWorkerMemoryService(nil, nil)
	bundle, err := svc.Recall(context.Background(), WorkerMemoryRecallRequest{
		WorkerID: "worker-a",
		Scope:    contextstore.Scope{ProjectID: "p", TeamID: "team", AgentID: "worker-a"},
		Query:    "test query",
		Policy:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession},
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if !bundle.Trace.Skipped {
		t.Error("no repo should set Skipped=true")
	}
}

func TestWP3_EmptyQueryReturnsEmptyBundle(t *testing.T) {
	repo := wp3SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)
	bundle, err := svc.Recall(context.Background(), WorkerMemoryRecallRequest{
		WorkerID: "worker-a",
		Scope:    contextstore.Scope{ProjectID: "p", TeamID: "team", AgentID: "worker-a"},
		Query:    "  ",
		Policy:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession},
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if !bundle.Trace.Skipped {
		t.Error("empty query should set Skipped=true")
	}
}

func TestWP3_SessionModeRetrievesOwnAndShared(t *testing.T) {
	repo := wp3SetupRepo(t)
	shared := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess"}
	agentA := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", AgentID: "worker-a"}
	agentB := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", AgentID: "worker-b"}
	wp3AppendItems(t, repo,
		contextstore.ContextItem{ID: "shared-decision", Kind: contextstore.ContextDecision, Content: "shared architecture decision", Scope: shared},
		contextstore.ContextItem{ID: "agent-a-finding", Kind: contextstore.ContextObservation, Content: "agent-a private architecture finding", Scope: agentA},
		contextstore.ContextItem{ID: "agent-b-finding", Kind: contextstore.ContextObservation, Content: "agent-b private architecture finding", Scope: agentB},
	)
	svc := NewWorkerMemoryService(repo, nil)
	bundle, err := svc.Recall(context.Background(), WorkerMemoryRecallRequest{
		WorkerID: "worker-a",
		Scope:    contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker-a"},
		Query:    "architecture",
		Policy:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, MaxItems: 10, MaxTokens: 5000},
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	ids := make([]string, 0, len(bundle.Items))
	for _, item := range bundle.Items {
		ids = append(ids, item.ID)
	}
	if !wp3ContainsID(ids, "shared-decision") {
		t.Errorf("should include shared ancestor: %v", ids)
	}
	if !wp3ContainsID(ids, "agent-a-finding") {
		t.Errorf("should include own private item: %v", ids)
	}
	if wp3ContainsID(ids, "agent-b-finding") {
		t.Errorf("should NOT include agent-b private item: %v", ids)
	}
}

func TestWP3_AgentCannotSeeOtherAgentPrivate(t *testing.T) {
	repo := wp3SetupRepo(t)
	agentA := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", AgentID: "worker-a"}
	agentB := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", AgentID: "worker-b"}
	wp3AppendItems(t, repo,
		contextstore.ContextItem{ID: "a-private", Kind: contextstore.ContextObservation, Content: "agent-a secret finding", Scope: agentA},
		contextstore.ContextItem{ID: "b-private", Kind: contextstore.ContextObservation, Content: "agent-b secret finding", Scope: agentB},
	)
	svc := NewWorkerMemoryService(repo, nil)
	// Agent B recalls.
	bundle, err := svc.Recall(context.Background(), WorkerMemoryRecallRequest{
		WorkerID: "worker-b",
		Scope:    contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker-b"},
		Query:    "secret finding",
		Policy:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, MaxItems: 10, MaxTokens: 5000},
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	for _, item := range bundle.Items {
		if item.ID == "a-private" {
			t.Errorf("Agent B should not see Agent A's private item: %s", item.ID)
		}
	}
}

func TestWP3_MaxItemsLimitEnforced(t *testing.T) {
	repo := wp3SetupRepo(t)
	scope := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", AgentID: "worker-a"}
	items := make([]contextstore.ContextItem, 10)
	for i := range items {
		items[i] = contextstore.ContextItem{
			ID:      string(rune('a'+i)) + "-item",
			Kind:    contextstore.ContextObservation,
			Content: "observation number " + string(rune('0'+i)),
			Scope:   scope,
		}
	}
	wp3AppendItems(t, repo, items...)
	svc := NewWorkerMemoryService(repo, nil)
	bundle, err := svc.Recall(context.Background(), WorkerMemoryRecallRequest{
		WorkerID: "worker-a",
		Scope:    contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker-a"},
		Query:    "observation",
		Policy:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, MaxItems: 3, MaxTokens: 50000},
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(bundle.Items) > 3 {
		t.Errorf("MaxItems=3 but got %d items", len(bundle.Items))
	}
}

func TestWP3_MaxTokensLimitEnforced(t *testing.T) {
	repo := wp3SetupRepo(t)
	scope := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", AgentID: "worker-a"}
	wp3AppendItems(t, repo,
		contextstore.ContextItem{ID: "big-1", Kind: contextstore.ContextObservation, Content: strings.Repeat("long content ", 100), Scope: scope},
		contextstore.ContextItem{ID: "big-2", Kind: contextstore.ContextObservation, Content: strings.Repeat("more content ", 100), Scope: scope},
	)
	svc := NewWorkerMemoryService(repo, nil)
	bundle, err := svc.Recall(context.Background(), WorkerMemoryRecallRequest{
		WorkerID: "worker-a",
		Scope:    contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker-a"},
		Query:    "content",
		Policy:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, MaxItems: 10, MaxTokens: 50},
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	// With MaxTokens=50, at most 1 item should fit (each is ~1300 chars / 4 = ~325 tokens).
	if len(bundle.Items) > 1 {
		t.Errorf("MaxTokens=50 but got %d items with total tokens %d", len(bundle.Items), bundle.Trace.Tokens)
	}
	if bundle.Trace.Tokens > 50 && len(bundle.Items) > 1 {
		t.Errorf("token budget exceeded: %d > 50", bundle.Trace.Tokens)
	}
}

func TestWP3_RenderWorkerMemorySectionHasDisclaimer(t *testing.T) {
	bundle := WorkerMemoryBundle{
		Items: []WorkerMemoryItem{
			{ContextItem: contextstore.ContextItem{ID: "mem-1", Content: "some prior finding"}, Tier: "session"},
		},
	}
	section := RenderWorkerMemorySection(bundle)
	if !strings.Contains(section, "## Your Prior Memory") {
		t.Errorf("section missing heading:\n%s", section)
	}
	if !strings.Contains(section, "background context") {
		t.Errorf("section missing background disclaimer:\n%s", section)
	}
	if !strings.Contains(section, "not current instructions") {
		t.Errorf("section missing instruction disclaimer:\n%s", section)
	}
	if !strings.Contains(section, "some prior finding") {
		t.Errorf("section missing item content:\n%s", section)
	}
	if !strings.Contains(section, "[session]") {
		t.Errorf("section missing tier label:\n%s", section)
	}
}

func TestWP3_RenderWorkerMemorySectionEmptyReturnsEmpty(t *testing.T) {
	section := RenderWorkerMemorySection(WorkerMemoryBundle{})
	if section != "" {
		t.Errorf("empty bundle should render empty string, got: %q", section)
	}
}

func TestWP3_TraceRecordsIDsNotContent(t *testing.T) {
	repo := wp3SetupRepo(t)
	scope := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", AgentID: "worker-a"}
	wp3AppendItems(t, repo,
		contextstore.ContextItem{ID: "trace-item", Kind: contextstore.ContextObservation, Content: "secret content in trace test", Scope: scope},
	)
	svc := NewWorkerMemoryService(repo, nil)
	bundle, err := svc.Recall(context.Background(), WorkerMemoryRecallRequest{
		WorkerID: "worker-a",
		Scope:    contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker-a"},
		Query:    "secret content",
		Policy:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, MaxItems: 5, MaxTokens: 5000},
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if !wp3ContainsID(bundle.Trace.ItemIDs, "trace-item") {
		t.Errorf("trace should record item ID: %v", bundle.Trace.ItemIDs)
	}
	// Trace should not contain the content string.
	traceStr := bundle.Trace.Query
	if strings.Contains(traceStr, "secret content in trace test") {
		t.Errorf("trace query should not contain item content: %q", traceStr)
	}
}

func TestWP3_ShouldRecallWorkerMemory(t *testing.T) {
	tests := []struct {
		name     string
		agentDef *agent.AgentDef
		profile  ExecutionProfile
		want     bool
	}{
		{
			name:     "nil agent def",
			agentDef: nil,
			profile:  ExecutionProfile{},
			want:     false,
		},
		{
			name:     "mode off",
			agentDef: &agent.AgentDef{Memory: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryOff}},
			profile:  ExecutionProfile{},
			want:     false,
		},
		{
			name:     "mode session, no profile disable",
			agentDef: &agent.AgentDef{Memory: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession}},
			profile:  ExecutionProfile{},
			want:     true,
		},
		{
			name:     "mode session, profile disables memory",
			agentDef: &agent.AgentDef{Memory: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession}},
			profile:  ExecutionProfile{DisableHistoricalMemory: true},
			want:     false,
		},
		{
			name:     "mode persistent, no profile disable",
			agentDef: &agent.AgentDef{Memory: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryPersistent}},
			profile:  ExecutionProfile{},
			want:     true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldRecallWorkerMemory(tc.agentDef, tc.profile)
			if got != tc.want {
				t.Errorf("shouldRecallWorkerMemory = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWP3_ResolveWorkerScope(t *testing.T) {
	base := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess"}
	agentDef := &agent.AgentDef{MemoryID: "worker-v1"}
	scope := resolveWorkerScope(base, agentDef, "")
	if scope.AgentID != "worker-v1" {
		t.Errorf("AgentID = %q, want worker-v1", scope.AgentID)
	}
	if scope.BranchID != "main" {
		t.Errorf("BranchID = %q, want main (default)", scope.BranchID)
	}
	if scope.ProjectID != "p" || scope.TeamID != "team" || scope.SessionID != "sess" {
		t.Errorf("base scope fields changed: %+v", scope)
	}
}

func TestWP3_PersistentModeRetrievesCrossSession(t *testing.T) {
	repo := wp3SetupRepo(t)
	// Session 1 items for agent-a.
	sess1 := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess-1", AgentID: "worker-a"}
	// Session 2 items for agent-a (current session).
	sess2 := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess-2", AgentID: "worker-a"}
	wp3AppendItems(t, repo,
		contextstore.ContextItem{ID: "sess1-item", Kind: contextstore.ContextObservation, Content: "finding from session 1", Scope: sess1},
		contextstore.ContextItem{ID: "sess2-item", Kind: contextstore.ContextObservation, Content: "finding from session 2", Scope: sess2},
	)
	svc := NewWorkerMemoryService(repo, nil)
	// Recall in session 2 with persistent mode.
	bundle, err := svc.Recall(context.Background(), WorkerMemoryRecallRequest{
		WorkerID: "worker-a",
		Scope:    contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess-2", BranchID: "main", AgentID: "worker-a"},
		Query:    "finding",
		Policy:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryPersistent, MaxItems: 10, MaxTokens: 5000},
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	ids := make([]string, 0, len(bundle.Items))
	for _, item := range bundle.Items {
		ids = append(ids, item.ID)
	}
	// Session 2 item should always be visible (same session).
	if !wp3ContainsID(ids, "sess2-item") {
		t.Errorf("persistent mode should include current session item: %v", ids)
	}
}

func TestWP3_LifecycleCandidateExcluded(t *testing.T) {
	repo := wp3SetupRepo(t)
	scope := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", AgentID: "worker-a"}
	wp3AppendItems(t, repo,
		contextstore.ContextItem{ID: "confirmed-mem", Kind: contextstore.ContextObservation, Content: "confirmed finding", Scope: scope, Lifecycle: contextstore.LifecycleConfirmed},
		contextstore.ContextItem{ID: "candidate-mem", Kind: contextstore.ContextObservation, Content: "candidate guess", Scope: scope, Lifecycle: contextstore.LifecycleCandidate},
	)
	svc := NewWorkerMemoryService(repo, nil)
	bundle, err := svc.Recall(context.Background(), WorkerMemoryRecallRequest{
		WorkerID: "worker-a",
		Scope:    contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker-a"},
		Query:    "finding guess",
		Policy:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, MaxItems: 10, MaxTokens: 5000},
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	for _, item := range bundle.Items {
		if item.ID == "candidate-mem" {
			t.Errorf("candidate item should be excluded from recall: %s", item.ID)
		}
	}
}

func TestWP3_DedupByContentHash(t *testing.T) {
	repo := wp3SetupRepo(t)
	scope := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", AgentID: "worker-a"}
	// Two items with the same content but different IDs.
	wp3AppendItems(t, repo,
		contextstore.ContextItem{ID: "dup-1", Kind: contextstore.ContextObservation, Content: "duplicate content here", Scope: scope},
		contextstore.ContextItem{ID: "dup-2", Kind: contextstore.ContextSummary, Content: "duplicate content here", Scope: scope},
	)
	svc := NewWorkerMemoryService(repo, nil)
	bundle, err := svc.Recall(context.Background(), WorkerMemoryRecallRequest{
		WorkerID: "worker-a",
		Scope:    contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker-a"},
		Query:    "duplicate content",
		Policy:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, MaxItems: 10, MaxTokens: 5000},
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	// Should only have 1 item (deduped by content hash).
	if len(bundle.Items) > 1 {
		t.Errorf("dedup should reduce to 1 item, got %d: %+v", len(bundle.Items), bundle.Trace.ItemIDs)
	}
}

func TestWP3_CompileWorkerContextIncludesWorkerMemory(t *testing.T) {
	bundle := &WorkerMemoryBundle{
		Items: []WorkerMemoryItem{
			{ContextItem: contextstore.ContextItem{ID: "wm-1", Content: "prior worker memory finding"}, Tier: "session"},
		},
	}
	compiled, err := CompileWorkerContext(context.Background(), WorkerContextInput{
		Goal:         "do the task",
		WorkerMemory: bundle,
		ModelContext: ModelContextSpec{ModelID: "test", ContextWindow: 4000, MaxOutputTokens: 500, SafetyMarginTokens: 100},
	})
	if err != nil {
		t.Fatalf("CompileWorkerContext: %v", err)
	}
	if !strings.Contains(compiled.Prompt, "Your Prior Memory") {
		t.Errorf("compiled prompt should include worker memory section:\n%s", compiled.Prompt)
	}
	if !strings.Contains(compiled.Prompt, "prior worker memory finding") {
		t.Errorf("compiled prompt should include memory content:\n%s", compiled.Prompt)
	}
	if !strings.Contains(compiled.Prompt, "background context") {
		t.Errorf("compiled prompt should include disclaimer:\n%s", compiled.Prompt)
	}
}

func TestWP3_CompileWorkerContextWithoutWorkerMemory(t *testing.T) {
	compiled, err := CompileWorkerContext(context.Background(), WorkerContextInput{
		Goal:         "do the task",
		WorkerMemory: nil,
		ModelContext: ModelContextSpec{ModelID: "test", ContextWindow: 4000, MaxOutputTokens: 500, SafetyMarginTokens: 100},
	})
	if err != nil {
		t.Fatalf("CompileWorkerContext: %v", err)
	}
	if strings.Contains(compiled.Prompt, "Your Prior Memory") {
		t.Errorf("prompt should not include worker memory section when bundle is nil:\n%s", compiled.Prompt)
	}
}
