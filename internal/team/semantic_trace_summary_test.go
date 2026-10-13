package team

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/semantic"
)

func TestContextRouterTracesCarryAttributionAndRoute(t *testing.T) {
	c, repo, fake := semanticRouterFixture(t)
	attachSemanticRuntime(t, c, repo, fake, semantic.ModeAuto)
	routeSharedIDs(t, c)
	routeSharedIDsForGoal(t, c, "協調者恢復後要避免重送副作用")

	traces, skipped, err := ReadSemanticRetrievalTraces(c.session.Workspace, "run-1")
	if err != nil || skipped != 0 {
		t.Fatalf("read traces: %v (skipped %d)", err, skipped)
	}
	var lexical, routed []contextstore.SemanticRetrievalTrace
	for _, trace := range traces {
		if trace.TaskID != "task-1" || trace.Attempt != 1 || trace.Phase != string(PhaseExecute) || trace.Trigger != string(ContextTriggerTaskDispatch) ||
			trace.Role != "operator" || trace.Agent != "operator-1" || trace.Source != semanticSourceSharedPersistent || trace.ContextRequestID == "" {
			t.Fatalf("trace attribution = %+v", trace)
		}
		switch trace.Route {
		case contextstore.SemanticRouteLexical:
			lexical = append(lexical, trace)
		case contextstore.SemanticRouteSemantic:
			routed = append(routed, trace)
		default:
			t.Fatalf("trace route = %q", trace.Route)
		}
	}
	if len(lexical) != 1 || lexical[0].Mode != contextstore.RetrievalOff || lexical[0].RouteShare != 0 || lexical[0].QueryTokens != 0 {
		t.Fatalf("lexical route traces = %+v", lexical)
	}
	if len(routed) == 0 {
		t.Fatal("no semantic route trace")
	}
	served := routed[0]
	if served.Mode != contextstore.RetrievalActive || served.FallbackReason != contextstore.SemanticFallbackNone || served.RouteShare < semantic.AutoRouteMinShare ||
		served.QueryTokens == 0 || served.CoveredCandidateCount != 3 || served.UncoveredCandidateCount != 0 {
		t.Fatalf("semantic route trace = %+v", served)
	}
	assertTracesContentFree(t, c.session.Workspace)
}

// memoryTraceSink collects traces in memory for worker recall tests.
type memoryTraceSink struct {
	mu     sync.Mutex
	traces []contextstore.SemanticRetrievalTrace
}

func (s *memoryTraceSink) write(trace contextstore.SemanticRetrievalTrace) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.traces = append(s.traces, trace)
}

func (s *memoryTraceSink) take() []contextstore.SemanticRetrievalTrace {
	s.mu.Lock()
	defer s.mu.Unlock()
	traces := s.traces
	s.traces = nil
	return traces
}

func TestWorkerRecallTracesAttributionAndEmbedsGoalOnly(t *testing.T) {
	fake := newConceptOllama(t)
	repo := openSemanticTestRepo(t)
	scope := contextstore.Scope{ProjectID: "project", TeamID: "team", SessionID: "session", BranchID: "main", AgentID: "worker"}
	if err := repo.Append(t.Context(), semanticItem("private-zh", "上次用收據檢查副作用避免重送", scope)); err != nil {
		t.Fatal(err)
	}
	runtime := readySemanticRuntime(t, conceptSettings(t, fake, semantic.ModeAuto, nil), repo)
	hasher, err := contextstore.NewHMACTraceHasher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	sink := &memoryTraceSink{}
	service := NewWorkerMemoryServiceWithOptions(repo, WorkerMemoryOptions{
		Semantic: runtime, Mode: runtime.Mode(), Router: runtime.router(), Fusion: runtime.Fusion(),
		TraceHasher: hasher, TraceSink: sink.write,
	})
	attribution := SemanticTraceAttribution{RunID: "run-1", TaskID: "task-9", Attempt: 2, Agent: "researcher", Source: semanticSourceWorkerPrivate}
	recall := func(goal string) []contextstore.SemanticRetrievalTrace {
		t.Helper()
		if _, err := service.Recall(t.Context(), WorkerMemoryRecallRequest{
			WorkerID: "worker", BranchID: "main", Scope: scope,
			Query: goal + "\nphase: execute\nrole: worker", SemanticQuery: goal, Trace: attribution,
			Policy: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, MaxItems: 3, MaxTokens: 500},
		}); err != nil {
			t.Fatal(err)
		}
		return sink.take()
	}

	english := recall("check duplicate side effect")
	if len(english) != 1 || english[0].Route != contextstore.SemanticRouteLexical || english[0].TaskID != "task-9" || english[0].Source != semanticSourceWorkerPrivate {
		t.Fatalf("english recall traces = %+v", english)
	}
	chinese := recall("檢查收據避免重送")
	if len(chinese) != 1 || chinese[0].Route != contextstore.SemanticRouteSemantic || chinese[0].Attempt != 2 || chinese[0].Agent != "researcher" ||
		chinese[0].FallbackReason != contextstore.SemanticFallbackNone {
		t.Fatalf("chinese recall traces = %+v", chinese)
	}
	fake.mu.Lock()
	lastQuery := fake.queryInputs[len(fake.queryInputs)-1]
	fake.mu.Unlock()
	if lastQuery != "檢查收據避免重送" {
		t.Fatalf("semantic lane embedded %q, want the goal without metadata lines", lastQuery)
	}
}

func TestSummarizeSemanticRetrievalTraces(t *testing.T) {
	trace := func(run, task, source, route string, mode contextstore.RetrievalMode, fallback contextstore.SemanticFallbackReason, latency, queue, embed int64, truncated bool) contextstore.SemanticRetrievalTrace {
		return contextstore.SemanticRetrievalTrace{
			RunID: run, TaskID: task, Attempt: 1, Source: source, Route: route, Mode: mode, FallbackReason: fallback,
			LatencyMillis: latency, QueueWaitMillis: queue, EmbedMillis: embed, QueryTruncated: truncated,
		}
	}
	const (
		shared  = semanticSourceSharedPersistent
		private = semanticSourceWorkerPrivate
		sem     = contextstore.SemanticRouteSemantic
		lex     = contextstore.SemanticRouteLexical
	)
	traces := []contextstore.SemanticRetrievalTrace{
		trace("run-1", "t1", shared, sem, contextstore.RetrievalActive, contextstore.SemanticFallbackNone, 100, 0, 90, false),
		trace("run-1", "t1", shared, sem, contextstore.RetrievalActive, contextstore.SemanticFallbackNone, 300, 50, 250, true),
		trace("run-1", "t2", shared, sem, contextstore.RetrievalActive, contextstore.SemanticFallbackEmbeddingWarming, 1, 0, 0, false),
		trace("run-2", "t3", shared, lex, contextstore.RetrievalOff, "", 0, 0, 0, false),
		trace("run-2", "t3", private, sem, contextstore.RetrievalActive, contextstore.SemanticFallbackProjectionIncomplete, 2, 0, 0, false),
		{Mode: contextstore.RetrievalShadow, FallbackReason: contextstore.SemanticFallbackNone, LatencyMillis: 5},
	}
	summary := SummarizeSemanticRetrievalTraces(traces)
	if summary.Traces != 6 || summary.Runs != 2 {
		t.Fatalf("summary counts = %+v", summary)
	}
	wantRoutes := []SemanticRouteSummary{
		{Source: shared, Route: sem, Mode: "active", Requests: 3, Served: 2, Fallbacks: 1, Truncated: 1, P50LatencyMS: 100, P95LatencyMS: 300, P95QueueWaitMS: 50, P50EmbedMS: 90, P95EmbedMS: 250},
		{Source: shared, Route: lex, Mode: "off", Requests: 1},
		{Source: private, Route: sem, Mode: "active", Requests: 1, Fallbacks: 1, P50LatencyMS: 2, P95LatencyMS: 2},
		{Source: "", Route: sem, Mode: "shadow", Requests: 1, Served: 1, P50LatencyMS: 5, P95LatencyMS: 5},
	}
	if !slices.Equal(summary.Routes, wantRoutes) {
		t.Fatalf("routes =\n%+v\nwant\n%+v", summary.Routes, wantRoutes)
	}
	if summary.Fallbacks["embedding_warming"] != 1 || summary.Fallbacks["projection_incomplete"] != 1 || len(summary.Fallbacks) != 2 {
		t.Fatalf("fallbacks = %v", summary.Fallbacks)
	}
	if len(summary.Tasks) != 5 || summary.Tasks[0].TaskID != "t1" || summary.Tasks[0].Requests != 2 || summary.Tasks[0].Served != 2 ||
		summary.Tasks[1].Fallbacks["embedding_warming"] != 1 || summary.Tasks[2].Route != lex {
		t.Fatalf("tasks = %+v", summary.Tasks)
	}
}

func TestPercentileMillis(t *testing.T) {
	tests := []struct {
		values     []int64
		percentile int
		want       int64
	}{
		{nil, 95, 0},
		{[]int64{7}, 50, 7},
		{[]int64{4, 1, 3, 2}, 50, 2},
		{[]int64{4, 1, 3, 2}, 95, 4},
		{[]int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}, 95, 100},
		{[]int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}, 50, 50},
	}
	for _, tc := range tests {
		if got := percentileMillis(tc.values, tc.percentile); got != tc.want {
			t.Fatalf("percentileMillis(%v, %d) = %d, want %d", tc.values, tc.percentile, got, tc.want)
		}
	}
}

func TestFinishSemanticRunRecordsTheRunOnly(t *testing.T) {
	c, repo, fake := semanticRouterFixture(t)
	c.finishSemanticRun("run-0", SemanticRuntimeStatus{})
	if _, ok := c.SemanticRetrievalReport(); ok {
		t.Fatal("report exists with semantic retrieval off")
	}
	attachSemanticRuntime(t, c, repo, fake, semantic.ModeAuto)
	// A first run on the same runtime: its routes must not count below.
	routeSharedIDsForGoal(t, c, "協調者恢復後要避免重送副作用")
	baseline, _ := c.SemanticRetrievalStatus()
	c.executionRunID = "run-1"
	routeSharedIDsForGoal(t, c, "協調者恢復後要避免重送副作用")
	routeSharedIDs(t, c)
	c.finishSemanticRun("run-1", baseline)

	records, skipped, err := ReadSemanticRuntimeRecords(c.session.Workspace, "")
	if err != nil || skipped != 0 || len(records) != 1 {
		t.Fatalf("records = %+v skipped=%d err=%v", records, skipped, err)
	}
	status := records[0].Status
	if records[0].RunID != "run-1" || records[0].RecordedAt.IsZero() || status.RoutedSemantic != 1 || status.RoutedLexical != 1 ||
		status.Queries != 1 || status.Warmups != 0 || status.State != "ready" || status.GenerationID == "" {
		t.Fatalf("record = %+v", records[0])
	}
	info, err := os.Stat(filepath.Join(c.session.Workspace, semanticRuntimeStatusFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("status file mode = %v, %v", info, err)
	}
	report, ok := c.SemanticRetrievalReport()
	if !ok || report.RunID != "run-1" || report.Status.RoutedSemantic != 1 || report.Summary.Traces == 0 || report.Summary.Runs != 1 {
		t.Fatalf("report = %+v ok=%v", report, ok)
	}
}

func TestSemanticRuntimeStatusSince(t *testing.T) {
	base := SemanticRuntimeStatus{Queries: 4, SemanticResults: 3, RoutedLexical: 2, Warmups: 1, Fallbacks: map[string]int{"embedding_warming": 1}}
	now := SemanticRuntimeStatus{State: "ready", Queries: 7, SemanticResults: 5, RoutedLexical: 2, RoutedSemantic: 3, Warmups: 1, Refreshes: 2,
		Fallbacks: map[string]int{"embedding_warming": 1, "embedding_busy": 1}}
	got := now.since(base)
	if got.Queries != 3 || got.SemanticResults != 2 || got.RoutedLexical != 0 || got.RoutedSemantic != 3 || got.Warmups != 0 || got.Refreshes != 2 ||
		got.State != "ready" || len(got.Fallbacks) != 1 || got.Fallbacks["embedding_busy"] != 1 {
		t.Fatalf("since = %+v", got)
	}
}

func TestReadSemanticRetrievalTracesSkipsBrokenLines(t *testing.T) {
	workspace := t.TempDir()
	lines := strings.Join([]string{
		`{"trace_id":"a","run_id":"run-1","mode":"active","route":"semantic","fallback_reason":"none"}`,
		`{"trace_id":"b","run_id":"run-2"`,
		`{"trace_id":"c","run_id":"run-2","mode":"off","route":"lexical"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(workspace, semanticTraceFile), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	all, skipped, err := ReadSemanticRetrievalTraces(workspace, "")
	if err != nil || skipped != 1 || len(all) != 2 {
		t.Fatalf("all = %+v skipped=%d err=%v", all, skipped, err)
	}
	run2, _, _ := ReadSemanticRetrievalTraces(workspace, "run-2")
	if len(run2) != 1 || run2[0].TraceID != "c" {
		t.Fatalf("run-2 = %+v", run2)
	}
	if missing, skipped, err := ReadSemanticRetrievalTraces(t.TempDir(), ""); err != nil || skipped != 0 || len(missing) != 0 {
		t.Fatalf("missing file = %+v %d %v", missing, skipped, err)
	}
}
