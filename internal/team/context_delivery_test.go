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

func TestContextManifestRecordsOnlyMaterializedItems(t *testing.T) {
	tests := []struct {
		name       string
		compiled   CompiledContext
		decision   ContextRouteDecision
		wantIn     bool
		wantReason ContextDecisionReason
	}{
		{
			name:       "compiled inclusion stays included",
			compiled:   CompiledContext{IncludedItems: []ContextItem{{ID: "context:m1", Kind: "observation", Source: "shared_session", TokenCount: 3}}},
			decision:   ContextRouteDecision{ContextItemID: "m1", Included: true, Reason: ContextIncludedRelevant},
			wantIn:     true,
			wantReason: ContextIncludedRelevant,
		},
		{
			name:       "compiler filter reason replaces route inclusion",
			compiled:   CompiledContext{FilteredItems: []FilteredContextItem{{ID: "context:m1", Kind: "progress", Source: "shared_session", Reason: ContextOmittedWorkerSTMOnly}}},
			decision:   ContextRouteDecision{ContextItemID: "m1", Included: true, Reason: ContextIncludedRelevant},
			wantReason: ContextOmittedWorkerSTMOnly,
		},
		{
			name:       "route inclusion absent from compiler output is not injected",
			decision:   ContextRouteDecision{ContextItemID: "m1", Included: true, Reason: ContextIncludedRequired},
			wantReason: ContextOmittedNotMaterialized,
		},
		{
			name:       "route omission keeps its reason",
			decision:   ContextRouteDecision{ContextItemID: "m1", Reason: ContextOmittedPhase},
			wantReason: ContextOmittedPhase,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest, err := BuildContextInjectionManifest(validTestContextRequest(), tt.compiled, []ContextRouteDecision{tt.decision}, "worker", time.Unix(100, 0), agent.DefaultMemoryLearningPolicy())
			if err != nil {
				t.Fatal(err)
			}
			if len(manifest.Items) != 1 {
				t.Fatalf("manifest items = %#v, want one", manifest.Items)
			}
			if got := manifest.Items[0]; got.Included != tt.wantIn || got.Reason != tt.wantReason {
				t.Fatalf("manifest item = included %v reason %q, want included %v reason %q", got.Included, got.Reason, tt.wantIn, tt.wantReason)
			}
		})
	}
}

func TestCompileWorkerContextReportsFilteredCanonicalItems(t *testing.T) {
	evaluatedAt := time.Now().UTC().Add(-time.Hour)
	expiredBeforeRoute := evaluatedAt.Add(-time.Minute)
	expiredAfterRoute := evaluatedAt.Add(30 * time.Minute)
	record := func(id string, kind contextstore.ContextKind, content string, expires *time.Time) contextstore.ContextItem {
		return contextstore.ContextItem{ID: id, Kind: kind, Content: content, ContentHash: "hash-" + id, Lifecycle: contextstore.LifecycleConfirmed, ExpiresAt: expires}
	}
	bundle := &CanonicalContextBundle{
		EvaluatedAt: evaluatedAt,
		SharedSession: []contextstore.ContextItem{
			record("finding", contextstore.ContextObservation, "session finding", nil),
			record("progress", contextstore.ContextProgress, "session progress", nil),
			record("blank", contextstore.ContextObservation, "  ", nil),
		},
		SharedPersistent: []contextstore.ContextItem{
			record("expired", contextstore.ContextPattern, "expired before routing", &expiredBeforeRoute),
			// Valid at route time; the compiler must judge it at the same instant.
			record("still-routed", contextstore.ContextPattern, "valid when routed", &expiredAfterRoute),
		},
	}
	compiled, err := CompileWorkerContext(context.Background(), WorkerContextInput{
		Goal:            "compile goal",
		CanonicalMemory: bundle,
		ModelContext:    ModelContextSpec{ModelID: "test", ContextWindow: 8192, MaxOutputTokens: 512, SafetyMarginTokens: 64},
	})
	if err != nil {
		t.Fatal(err)
	}
	included := map[string]bool{}
	for _, item := range compiled.IncludedItems {
		included[item.ID] = true
	}
	for _, id := range []string{"context:finding", "context:still-routed"} {
		if !included[id] {
			t.Errorf("%s not compiled; included = %v", id, included)
		}
	}
	want := map[string]ContextDecisionReason{
		"context:progress": ContextOmittedWorkerSTMOnly,
		"context:blank":    ContextOmittedEmptyContent,
		"context:expired":  ContextOmittedExpired,
	}
	got := map[string]ContextDecisionReason{}
	for _, filtered := range compiled.FilteredItems {
		got[filtered.ID] = filtered.Reason
	}
	if len(got) != len(want) {
		t.Fatalf("filtered = %v, want %v", got, want)
	}
	for id, reason := range want {
		if got[id] != reason || included[id] {
			t.Errorf("%s: filtered reason %q included %v, want %q and not included", id, got[id], included[id], reason)
		}
	}
}

func TestBudgetContextItemsNeverExceedsBudget(t *testing.T) {
	cjk := strings.Repeat("繁體中文的歷史記憶內容需要被正確截斷。", 40)
	ascii := strings.Repeat("historical memory content that must be truncated ", 40)
	tests := []struct {
		name          string
		item          ContextItem
		budget        int
		wantErr       bool
		wantCompress  bool
		wantSelection bool
	}{
		{name: "cjk compressible history is truncated", item: ContextItem{ID: "h1", Content: cjk, Compressible: true, Authority: ContextAuthorityHistorical}, budget: 120, wantCompress: true, wantSelection: true},
		{name: "ascii compressible history is truncated", item: ContextItem{ID: "h2", Content: ascii, Compressible: true, Authority: ContextAuthorityHistorical}, budget: 120, wantCompress: true, wantSelection: true},
		{name: "cjk prefix with ascii tail", item: ContextItem{ID: "mixed", Content: strings.Repeat("中", 1000) + strings.Repeat("a", 10000), Compressible: true, Authority: ContextAuthorityHistorical}, budget: 120, wantCompress: true, wantSelection: true},
		{name: "ascii prefix with cjk tail", item: ContextItem{ID: "mixed-reverse", Content: strings.Repeat("a", 1000) + strings.Repeat("中", 10000), Compressible: true, Authority: ContextAuthorityHistorical}, budget: 120, wantCompress: true, wantSelection: true},
		{name: "required history may be truncated", item: ContextItem{ID: "h3", Content: cjk, Compressible: true, Required: true, Authority: ContextAuthorityHistorical}, budget: 120, wantCompress: true, wantSelection: true},
		{name: "required normative fails closed", item: ContextItem{ID: "n1", Content: cjk, Compressible: true, Required: true, Authority: ContextAuthorityNormative}, budget: 120, wantErr: true},
		{name: "remainder too small is omitted", item: ContextItem{ID: "h4", Content: cjk, Compressible: true, Authority: ContextAuthorityHistorical}, budget: 10},
	}
	for _, estimator := range []string{"", "qwen", "tiktoken", "claude"} {
		for _, tt := range tests {
			t.Run(estimator+"/"+tt.name, func(t *testing.T) {
				items := []ContextItem{tt.item}
				assignTokenCounts(ModelContextSpec{ModelID: "test", Estimator: estimator}, items)
				selected, overBudget, err := BudgetContextItems(items, ContextBudget{Available: tt.budget})
				if (err != nil) != tt.wantErr {
					t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
				}
				if !overBudget {
					t.Fatal("overBudget = false, want true")
				}
				if tt.wantErr {
					var overflow *RequiredContextBudgetError
					if !errors.As(err, &overflow) {
						t.Fatalf("required overflow lost its typed error: %v", err)
					}
					return
				}
				if got := len(selected) == 1; got != tt.wantSelection {
					t.Fatalf("selected = %d items, want selection %v", len(selected), tt.wantSelection)
				}
				used := 0
				for _, item := range selected {
					used += item.TokenCount
					if item.Compressed != tt.wantCompress {
						t.Errorf("compressed = %v, want %v", item.Compressed, tt.wantCompress)
					}
					// The truncated text must also fit when re-estimated.
					if estimated := estimateTextTokens(item.Content, item.tokenEstimator); estimated > tt.budget {
						t.Errorf("re-estimated tokens = %d, want <= %d", estimated, tt.budget)
					} else if item.TokenCount != estimated {
						t.Errorf("reported tokens = %d, re-estimated = %d", item.TokenCount, estimated)
					}
				}
				if used > tt.budget {
					t.Fatalf("used tokens = %d, want <= %d", used, tt.budget)
				}
			})
		}
	}
}

type scopeFailingSearchRepo struct {
	contextstore.Repository
	fail func(contextstore.Scope) bool
	err  error
}

func (r scopeFailingSearchRepo) SearchExact(ctx context.Context, req contextstore.SearchRequest) ([]contextstore.SearchResult, error) {
	if r.fail(req.Scope) {
		return nil, r.err
	}
	return r.Repository.SearchExact(ctx, req)
}

func (r scopeFailingSearchRepo) SearchLexical(ctx context.Context, req contextstore.SearchRequest) ([]contextstore.SearchResult, error) {
	if r.fail(req.Scope) {
		return nil, r.err
	}
	return r.Repository.SearchLexical(ctx, req)
}

func TestWorkerMemoryRecallDegradesPerSource(t *testing.T) {
	sessionScope := func(scope contextstore.Scope) bool { return scope.SessionID != "" }
	persistentScope := func(scope contextstore.Scope) bool { return scope.SessionID == "" }
	tests := []struct {
		name         string
		fail         func(contextstore.Scope) bool
		err          error
		wantErr      error
		wantDegraded string
		wantIDs      []string
	}{
		{name: "session failure keeps persistent results", fail: sessionScope, err: errors.New("disk error at /secret/path"), wantDegraded: "session:main:retrieval_error", wantIDs: []string{"persistent-1"}},
		// Session recall sees ancestor scopes, so it still finds the persistent row.
		{name: "persistent failure keeps session results", fail: persistentScope, err: errors.New("disk error"), wantDegraded: "persistent:retrieval_error", wantIDs: []string{"session-1", "persistent-1"}},
		{name: "cancellation is returned", fail: sessionScope, err: context.Canceled, wantErr: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, err := contextstore.OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer repo.Close()
			session := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker-a"}
			persistent := contextstore.Scope{ProjectID: "p", TeamID: "team", AgentID: "worker-a"}
			for _, item := range []contextstore.ContextItem{
				{ID: "session-1", Kind: contextstore.ContextObservation, Scope: session, Content: "calibrate beacon session note", Lifecycle: contextstore.LifecycleConfirmed},
				{ID: "persistent-1", Kind: contextstore.ContextPattern, Scope: persistent, Content: "calibrate beacon persistent note", Lifecycle: contextstore.LifecycleConfirmed},
			} {
				if err := repo.Append(t.Context(), item); err != nil {
					t.Fatal(err)
				}
			}
			svc := NewWorkerMemoryService(scopeFailingSearchRepo{Repository: repo, fail: tt.fail, err: tt.err}, nil)
			bundle, err := svc.Recall(t.Context(), WorkerMemoryRecallRequest{
				WorkerID: "worker-a", BranchID: "main", Scope: session, Query: "calibrate beacon",
				Policy: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryPersistent, MaxItems: 10, MaxTokens: 4000},
			})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if bundle.Trace.Skipped {
				t.Fatalf("degraded recall reported as skipped: %#v", bundle.Trace)
			}
			if strings.Join(bundle.Trace.Degraded, ",") != tt.wantDegraded {
				t.Fatalf("degraded = %v, want %q", bundle.Trace.Degraded, tt.wantDegraded)
			}
			if strings.Contains(strings.Join(bundle.Trace.Degraded, ","), "/secret/path") {
				t.Fatalf("degraded reason leaked error text: %v", bundle.Trace.Degraded)
			}
			if strings.Join(bundle.Trace.ItemIDs, ",") != strings.Join(tt.wantIDs, ",") {
				t.Fatalf("items = %v, want %v", bundle.Trace.ItemIDs, tt.wantIDs)
			}
		})
	}
}

type countingWorkerMemoryService struct {
	noopWorkerMemoryService
	calls int
}

func (s *countingWorkerMemoryService) Recall(context.Context, WorkerMemoryRecallRequest) (WorkerMemoryBundle, error) {
	s.calls++
	return WorkerMemoryBundle{}, nil
}

func TestRecallWorkerMemorySkipsVerifyPhase(t *testing.T) {
	agentDef := &agent.AgentDef{Name: "worker", MemoryID: "worker-mem", Memory: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession}}
	tests := []struct {
		phase     Phase
		wantCalls int
	}{
		{phase: PhaseVerify, wantCalls: 0},
		{phase: PhaseExecute, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(string(tt.phase), func(t *testing.T) {
			svc := &countingWorkerMemoryService{}
			c := &Coordinator{workerMemorySvc: svc, session: &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "team"}}}
			bundle, err := c.recallWorkerMemory(t.Context(), agentDef, ContextRequest{Phase: tt.phase, Goal: "goal"})
			if err != nil || bundle != nil {
				t.Fatalf("recall = %v, %v; want nil bundle and nil error", bundle, err)
			}
			if svc.calls != tt.wantCalls {
				t.Fatalf("recall calls = %d, want %d", svc.calls, tt.wantCalls)
			}
		})
	}
}

func TestCandidateRunIDFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		executing  string
		requestRun string
		want       string
	}{
		{name: "matching run", executing: "run-1", requestRun: "run-1", want: "run-1"},
		{name: "request without run", executing: "run-1", want: "run-1"},
		{name: "no execution run", requestRun: "run-context-abc"},
		{name: "request from another run", executing: "run-1", requestRun: "run-2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Coordinator{executionRunID: tt.executing}
			if got := c.candidateRunID(ContextRequest{RunID: tt.requestRun}); got != tt.want {
				t.Fatalf("candidateRunID = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCandidateRoutingAndGetUseTheExecutionRun(t *testing.T) {
	for _, tt := range []struct {
		name, execution, requested string
		allowed                    bool
	}{
		{name: "current execution", execution: "run-current", requested: "run-current", allowed: true},
		{name: "mismatched request", execution: "run-other", requested: "run-current"},
		{name: "no execution", requested: "run-current"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, repo := rankingTestCoordinator(t, agent.MemoryLearningOff)
			c.executionRunID = tt.execution
			item := contextstore.ContextItem{ID: "candidate", Kind: contextstore.ContextObservation, Content: "current candidate sentinel", Scope: c.contextScope(), Lifecycle: contextstore.LifecycleCandidate, Metadata: map[string]string{"run_id": "run-current"}}
			if err := repo.Append(t.Context(), item); err != nil {
				t.Fatal(err)
			}
			request := validTestContextRequest()
			request.RunID, request.Goal = tt.requested, "current candidate sentinel"
			request.AssignRequestID()
			route, err := c.contextRouter().Route(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			included := false
			for _, row := range route.Bundle.SharedSession {
				included = included || row.ID == item.ID
			}
			_, _, getErr := c.GetAuthorizedContextItem(t.Context(), request, item.ID)
			if included != tt.allowed || (getErr == nil) != tt.allowed {
				t.Fatalf("route included=%v get error=%v, want allowed=%v", included, getErr, tt.allowed)
			}
		})
	}
}

func TestContextGetKeepsReplayTimeThroughManifest(t *testing.T) {
	c, repo := rankingTestCoordinator(t, agent.MemoryLearningOff)
	c.executionRunID = "run-current"
	c.sessionData = NewSession()
	instant := time.Now().UTC().Add(-time.Hour)
	expires := instant.Add(30 * time.Minute)
	item := rankingItem("replay-visible", 10)
	item.ExpiresAt = &expires
	if err := repo.Append(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	ctx := contextstore.WithEvaluationTime(t.Context(), instant)
	response, err := (&contextGetTool{coordinator: c}).Run(ctx, fantasy.ToolCall{ID: "get-replay", Input: `{"id":"replay-visible"}`})
	if err != nil || response.IsError || response.Content != item.Content {
		t.Fatalf("replay context_get = %+v err=%v", response, err)
	}
	manifests := c.sessionData.CoordinatorContextManifests
	if len(manifests) != 1 || len(manifests[0].Items) != 1 || !manifests[0].Items[0].Included {
		t.Fatalf("returned context was not attributed at the replay instant: %+v", manifests)
	}
}
