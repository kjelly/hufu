package team

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/semantic"
)

// semanticRouterFixture is a coordinator with Chinese shared memories and an
// English request goal: lexical retrieval cannot connect them, the concept
// encoder can.
func semanticRouterFixture(t *testing.T) (*Coordinator, *contextstore.SQLiteRepository, *conceptOllama) {
	t.Helper()
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	shared := contextstore.Scope{ProjectID: "project", TeamID: "team"}
	if err := repo.Append(t.Context(),
		semanticItem("zh-receipt", "協調者中斷後要用收據判斷已完成的副作用，避免重送。", shared),
		semanticItem("zh-theme", "深色背景下的顏色要跟著切換。", shared),
		semanticItem("en-lint", "Use golangci-lint with gocyclo threshold 40.", shared),
	); err != nil {
		t.Fatal(err)
	}
	policy := agent.DefaultMemoryLearningPolicy()
	policy.Mode = agent.MemoryLearningOff
	c := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team", MemoryLearning: policy}}, contextRepo: repo, projectDir: "project"}
	c.executionRunID = "run-1"
	c.memoryRankingPolicy = MemoryRuntimeRankingPolicy{CandidateTopK: 5, InjectTopK: 2, MinimumRelevance: 0}
	return c, repo, newConceptOllama(t)
}

func attachSemanticRuntime(t *testing.T, c *Coordinator, repo *contextstore.SQLiteRepository, fake *conceptOllama, mode semantic.Mode) *SemanticRetrievalRuntime {
	t.Helper()
	settings := conceptSettings(t, fake, mode, nil)
	runtime := readySemanticRuntime(t, settings, repo)
	hasher, sink, err := c.semanticTraceOptions()
	if err != nil {
		t.Fatal(err)
	}
	c.semanticRuntime, c.semanticSettings = runtime, settings
	c.semanticTraceHasher, c.semanticTraceSink = hasher, sink
	return runtime
}

const semanticRouteGoal = "avoid duplicate side effect after coordinator resume"

func semanticRouteRequest(goal string) ContextRequest {
	request := validTestContextRequest()
	request.Goal = goal
	request.Phase = PhaseExecute
	request.Trigger = ContextTriggerTaskDispatch
	request.AssignRequestID()
	return request
}

func routeSharedIDs(t *testing.T, c *Coordinator) ([]string, *SemanticRetrievalIdentity) {
	t.Helper()
	return routeSharedIDsForGoal(t, c, semanticRouteGoal)
}

func routeSharedIDsForGoal(t *testing.T, c *Coordinator, goal string) ([]string, *SemanticRetrievalIdentity) {
	t.Helper()
	route, err := c.contextRouter().Route(t.Context(), semanticRouteRequest(goal))
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(route.Bundle.SharedPersistent))
	for _, item := range route.Bundle.SharedPersistent {
		ids = append(ids, item.ID)
	}
	return ids, route.Bundle.SharedPersistentSemantic
}

func TestContextRouterSemanticLaneByMode(t *testing.T) {
	c, repo, fake := semanticRouterFixture(t)
	offIDs, offSemantic := routeSharedIDs(t, c)
	if slices.Contains(offIDs, "zh-receipt") || offSemantic != nil {
		t.Fatalf("off selection = %v semantic = %+v; lexical cannot reach the Chinese memory", offIDs, offSemantic)
	}
	if fake.requests() != 0 {
		t.Fatal("off mode contacted the encoder")
	}

	attachSemanticRuntime(t, c, repo, fake, semantic.ModeShadow)
	shadowIDs, shadowSemantic := routeSharedIDs(t, c)
	if !slices.Equal(shadowIDs, offIDs) || shadowSemantic != nil {
		t.Fatalf("shadow changed selection: %v vs off %v (semantic %+v)", shadowIDs, offIDs, shadowSemantic)
	}
	traces := readSemanticTraces(t, c.session.Workspace)
	if len(traces) == 0 || traces[len(traces)-1].Mode != contextstore.RetrievalShadow || traces[len(traces)-1].SemanticResultCount == 0 {
		t.Fatalf("shadow traces = %+v", traces)
	}
	assertTracesContentFree(t, c.session.Workspace)

	c.semanticRuntime.Close()
	attachSemanticRuntime(t, c, repo, fake, semantic.ModeActive)
	activeIDs, activeSemantic := routeSharedIDs(t, c)
	if len(activeIDs) == 0 || activeIDs[0] != "zh-receipt" {
		t.Fatalf("active selection = %v, want zh-receipt first", activeIDs)
	}
	if activeSemantic == nil || activeSemantic.FallbackReason != contextstore.SemanticFallbackNone || activeSemantic.GenerationID == "" ||
		activeSemantic.ModelID != "ollama/embeddinggemma:latest" || activeSemantic.ModelHash != conceptDigest {
		t.Fatalf("active semantic identity = %+v", activeSemantic)
	}

	// A worker's private write in the same run does not stale the lane (B1).
	if err := repo.Append(t.Context(), semanticItem("worker-stm", "private progress", contextstore.Scope{ProjectID: "project", TeamID: "team", SessionID: "s", AgentID: "worker"})); err != nil {
		t.Fatal(err)
	}
	if _, identity := routeSharedIDs(t, c); identity == nil || identity.FallbackReason != contextstore.SemanticFallbackNone {
		t.Fatalf("identity after unrelated write = %+v", identity)
	}
	// A new shared memory falls back to lexical until it is embedded.
	if err := repo.Append(t.Context(), semanticItem("zh-new", "新的收據規則", contextstore.Scope{ProjectID: "project", TeamID: "team"})); err != nil {
		t.Fatal(err)
	}
	ids, identity := routeSharedIDs(t, c)
	if identity == nil || identity.FallbackReason != contextstore.SemanticFallbackProjectionIncomplete || slices.Contains(ids, "zh-receipt") {
		t.Fatalf("uncovered candidate: ids=%v identity=%+v", ids, identity)
	}
}

func TestWorkerRecallUsesSemanticLaneForPrivateMemory(t *testing.T) {
	fake := newConceptOllama(t)
	repo := openSemanticTestRepo(t)
	scope := contextstore.Scope{ProjectID: "project", TeamID: "team", SessionID: "session", BranchID: "main", AgentID: "worker"}
	if err := repo.Append(t.Context(), semanticItem("private-zh", "上次用收據檢查副作用避免重送", scope)); err != nil {
		t.Fatal(err)
	}
	request := WorkerMemoryRecallRequest{
		WorkerID: "worker", BranchID: "main", Scope: scope, Query: "check duplicate side effect",
		Policy: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, MaxItems: 3, MaxTokens: 500},
	}
	off, err := NewWorkerMemoryService(repo, nil).Recall(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(off.Items) != 0 {
		t.Fatalf("lexical recall unexpectedly reached the Chinese memory: %v", off.Trace.ItemIDs)
	}
	runtime := readySemanticRuntime(t, conceptSettings(t, fake, semantic.ModeActive, nil), repo)
	active, err := NewWorkerMemoryServiceWithOptions(repo, WorkerMemoryOptions{Semantic: runtime, Mode: runtime.Mode()}).Recall(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(active.Items) != 1 || active.Items[0].ID != "private-zh" {
		t.Fatalf("semantic recall = %v", active.Trace.ItemIDs)
	}
	if active.Semantic == nil || active.Semantic.FallbackReason != contextstore.SemanticFallbackNone {
		t.Fatalf("worker semantic identity = %+v", active.Semantic)
	}
}

func TestExecutionPolicySnapshotPinsOnlySelectingSemanticRetrieval(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	c := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 4, 3)
	base, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	if base.snapshot.SemanticRetrievalHash != "" {
		t.Fatalf("default snapshot pins semantic retrieval: %s", base.snapshot.SemanticRetrievalHash)
	}
	for _, tc := range []struct {
		mode    semantic.Mode
		pinned  bool
		changed bool
	}{{semantic.ModeOff, false, false}, {semantic.ModeShadow, false, false}, {semantic.ModeActive, true, true}, {semantic.ModeAuto, true, true}} {
		settings, err := semantic.Config{Mode: tc.mode}.Resolve()
		if err != nil {
			t.Fatal(err)
		}
		c.semanticSettings = settings
		state, err := newExecutionPolicyState(c)
		if err != nil {
			t.Fatal(err)
		}
		if (state.snapshot.SemanticRetrievalHash != "") != tc.pinned || (state.snapshot.ConfigurationHash != base.snapshot.ConfigurationHash) != tc.changed {
			t.Fatalf("mode %s: hash=%q configuration changed=%v", tc.mode, state.snapshot.SemanticRetrievalHash, state.snapshot.ConfigurationHash != base.snapshot.ConfigurationHash)
		}
		if err := validateExecutionPolicySnapshot(state.snapshot); err != nil {
			t.Fatal(err)
		}
	}
}

func readSemanticTraces(t *testing.T, workspace string) []contextstore.SemanticRetrievalTrace {
	t.Helper()
	f, err := os.Open(filepath.Join(workspace, semanticTraceFile))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var traces []contextstore.SemanticRetrievalTrace
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var trace contextstore.SemanticRetrievalTrace
		if err := json.Unmarshal(scanner.Bytes(), &trace); err != nil {
			t.Fatal(err)
		}
		traces = append(traces, trace)
	}
	return traces
}

func assertTracesContentFree(t *testing.T, workspace string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workspace, semanticTraceFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"zh-receipt", "收據", "duplicate side effect", "golangci"} {
		if strings.Contains(string(data), leak) {
			t.Fatalf("semantic trace leaked %q", leak)
		}
	}
	info, err := os.Stat(filepath.Join(workspace, semanticTraceKeyFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("trace key mode = %v, %v", info, err)
	}
}

func TestSemanticRetrievalParsesInBothManifestForms(t *testing.T) {
	block := "semantic-retrieval:\n  mode: auto\n  fusion: rrf_normalized\n  query-max-tokens: 256\n"
	indented := "  " + strings.ReplaceAll(strings.TrimSuffix(block, "\n"), "\n", "\n  ") + "\n"
	manifests := map[string]string{
		"flat":     "name: semantic-team\n" + block,
		"v1alpha1": "apiVersion: hufu.io/v1alpha1\nkind: AgentTeam\nmetadata:\n  name: semantic-team\nspec:\n" + indented,
	}
	for name, manifest := range manifests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeTeamManifest(t, dir, manifest)
			cfg, err := parseTeamYML(dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.SemanticRetrieval; got.Mode != semantic.ModeAuto || got.Fusion != semantic.FusionRRFNormalized || got.QueryMaxTokens != 256 {
				t.Fatalf("semantic-retrieval = %#v", got)
			}
		})
	}
	for name, invalid := range map[string]string{
		"unknown mode":   "semantic-retrieval:\n  mode: always\n",
		"unknown fusion": "semantic-retrieval:\n  fusion: bm25\n",
		"unknown field":  "semantic-retrieval:\n  provider: ollama\n",
		"remote no opt":  "semantic-retrieval:\n  endpoint: http://10.0.0.5:11434\n",
		"unknown model":  "semantic-retrieval:\n  model: nomic-embed-text\n",
		"negative value": "semantic-retrieval:\n  max-concurrent: -1\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeTeamManifest(t, dir, "name: semantic-team\n"+invalid)
			if _, err := parseTeamYML(dir, nil); err == nil {
				t.Fatal("parseTeamYML accepted an invalid semantic-retrieval block")
			}
		})
	}
}
