package team

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/embedding"
	"github.com/kjelly/hufu/internal/semantic"
)

const conceptDigest = "85462619ee721b466c5927d109d4cb765861907d5417b9109caebc4e614679f1"

// conceptConcepts map words in either language onto one dimension, so the
// fake encoder behaves like a cross-lingual semantic model.
var conceptConcepts = [][]string{
	{"receipt", "side effect", "duplicate", "收據", "副作用", "重送", "重複"},
	{"dark", "theme", "深色", "背景", "顏色"},
	{"lint", "gocyclo", "靜態檢查"},
}

// conceptOllama is a fake Ollama server with a deterministic concept encoder.
type conceptOllama struct {
	mu          sync.Mutex
	missing     bool
	embedDelay  time.Duration
	showDelay   time.Duration
	embedCalls  int
	loadedCtx   int
	server      *httptest.Server
	queryInputs []string
}

func newConceptOllama(t *testing.T) *conceptOllama {
	t.Helper()
	f := &conceptOllama{}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func conceptVector(text string) []float64 {
	lower := strings.ToLower(text)
	vector := []float64{0, 0, 0, 0.05}
	for dim, words := range conceptConcepts {
		for _, word := range words {
			vector[dim] += float64(strings.Count(lower, word))
		}
	}
	return vector
}

func (f *conceptOllama) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.embedCalls
}

func (f *conceptOllama) set(update func(*conceptOllama)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	update(f)
}

func (f *conceptOllama) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	missing, embedDelay, showDelay := f.missing, f.embedDelay, f.showDelay
	f.mu.Unlock()
	switch r.URL.Path {
	case "/api/tags":
		models := []map[string]any{}
		if !missing {
			models = append(models, map[string]any{"name": "embeddinggemma:latest", "digest": conceptDigest, "details": map[string]string{"family": "gemma3", "quantization_level": "BF16"}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
	case "/api/show":
		if showDelay > 0 {
			select {
			case <-time.After(showDelay):
			case <-r.Context().Done():
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model_info": map[string]any{
			"general.architecture": "gemma3", "gemma3.context_length": 2048, "gemma3.embedding_length": 4,
		}})
	case "/api/ps":
		f.mu.Lock()
		loaded := f.loadedCtx
		f.mu.Unlock()
		models := []map[string]any{}
		if loaded > 0 {
			models = append(models, map[string]any{"name": "embeddinggemma:latest", "digest": conceptDigest, "context_length": loaded})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
	case "/api/embed":
		var req struct {
			Input   []string       `json:"input"`
			Options map[string]int `json:"options"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.embedCalls++
		f.loadedCtx = req.Options["num_ctx"]
		for _, input := range req.Input {
			if strings.HasPrefix(input, "task: search result | query: ") {
				f.queryInputs = append(f.queryInputs, strings.TrimPrefix(input, "task: search result | query: "))
			}
		}
		f.mu.Unlock()
		if embedDelay > 0 && len(req.Input) > 0 {
			select {
			case <-time.After(embedDelay):
			case <-r.Context().Done():
				return
			}
		}
		embeddings := make([][]float64, len(req.Input))
		for i, input := range req.Input {
			embeddings[i] = conceptVector(input)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": embeddings, "prompt_eval_count": 9 * len(req.Input)})
	default:
		http.NotFound(w, r)
	}
}

func conceptSettings(t *testing.T, fake *conceptOllama, mode semantic.Mode, tweak func(*semantic.Config)) semantic.Settings {
	t.Helper()
	cfg := semantic.Config{Mode: mode, Endpoint: fake.server.URL}
	if tweak != nil {
		tweak(&cfg)
	}
	settings, err := cfg.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

func openSemanticTestRepo(t *testing.T) *contextstore.SQLiteRepository {
	t.Helper()
	repo := openMigratedContextTestDB(t, filepath.Join(t.TempDir(), "context.sqlite"))
	return repo
}

func startSemanticRuntime(t *testing.T, settings semantic.Settings, repo contextstore.Repository) *SemanticRetrievalRuntime {
	t.Helper()
	// Revision polling is disabled so tests decide when the index is stale;
	// incomplete queries still nudge the refresher.
	runtime, err := newSemanticRetrievalRuntimeWithPoll(settings, repo, "project", false, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if runtime == nil {
		t.Fatal("runtime is nil for an enabled mode")
	}
	t.Cleanup(runtime.Close)
	return runtime
}

func readySemanticRuntime(t *testing.T, settings semantic.Settings, repo contextstore.Repository) *SemanticRetrievalRuntime {
	t.Helper()
	runtime := startSemanticRuntime(t, settings, repo)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := runtime.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	// Becoming ready nudges one background refresh; wait for it so later
	// writes in a test are not raced by that initial refresh.
	for runtime.Status().Refreshes == 0 {
		if ctx.Err() != nil {
			t.Fatalf("initial refresh did not run: %+v", runtime.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	return runtime
}

func semanticItem(id, content string, scope contextstore.Scope) contextstore.ContextItem {
	return contextstore.ContextItem{
		ID: id, Kind: contextstore.ContextPattern, Content: content, Scope: scope,
		Lifecycle: contextstore.LifecycleConfirmed, TrustLevel: contextstore.TrustTrusted, Priority: 10, Confidence: 1,
		CreatedAt: time.Unix(100, 0), UpdatedAt: time.Unix(100, 0),
	}
}

func TestSemanticRuntimeOffStartsNothing(t *testing.T) {
	fake := newConceptOllama(t)
	settings := conceptSettings(t, fake, semantic.ModeOff, nil)
	runtime, err := newSemanticRetrievalRuntime(settings, openSemanticTestRepo(t), "project", false)
	if err != nil || runtime != nil {
		t.Fatalf("off runtime = %v, %v", runtime, err)
	}
	if fake.requests() != 0 {
		t.Fatalf("off mode sent %d embedding requests", fake.requests())
	}
}

func TestSemanticRuntimeRefusesRemoteEndpointUnderNoNet(t *testing.T) {
	settings, err := semantic.Config{Mode: semantic.ModeShadow, Endpoint: "http://192.168.11.117:11434", AllowRemoteEndpoint: new(true)}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = newSemanticRetrievalRuntime(settings, openSemanticTestRepo(t), "project", true); err == nil || !strings.Contains(err.Error(), "--no-net") {
		t.Fatalf("no-net error = %v", err)
	}
}

func TestSemanticRuntimeNeverWaitsForModelLoad(t *testing.T) {
	fake := newConceptOllama(t)
	fake.set(func(f *conceptOllama) { f.showDelay = 2 * time.Second })
	repo := openSemanticTestRepo(t)
	runtime := startSemanticRuntime(t, conceptSettings(t, fake, semantic.ModeActive, nil), repo)
	started := time.Now()
	_, err := runtime.SearchVector(t.Context(), contextstore.SearchRequest{Query: "receipt", Scope: contextstore.Scope{ProjectID: "project"}})
	if !errors.Is(err, contextstore.ErrSemanticWarming) {
		t.Fatalf("cold query err = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("cold query waited %s for the model", elapsed)
	}
	if got := runtime.Status().Fallbacks[string(contextstore.SemanticFallbackEmbeddingWarming)]; got != 1 {
		t.Fatalf("warming fallbacks = %d", got)
	}
}

func TestSemanticRuntimeMissingModelFailsFast(t *testing.T) {
	fake := newConceptOllama(t)
	fake.set(func(f *conceptOllama) { f.missing = true })
	runtime := startSemanticRuntime(t, conceptSettings(t, fake, semantic.ModeActive, nil), openSemanticTestRepo(t))
	deadline := time.Now().Add(5 * time.Second)
	for runtime.Status().State != semanticStateFailed {
		if time.Now().After(deadline) {
			t.Fatalf("state = %s, want failed", runtime.Status().State)
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, err := runtime.SearchVector(t.Context(), contextstore.SearchRequest{Query: "receipt", Scope: contextstore.Scope{ProjectID: "project"}})
	if !errors.Is(err, embedding.ErrOllamaUnavailable) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing model err = %v", err)
	}
	if status := runtime.Status(); status.Failure != string(contextstore.SemanticFallbackEmbeddingUnavailable) {
		t.Fatalf("status failure = %q", status.Failure)
	}
	if fake.requests() != 0 {
		t.Fatalf("missing model sent %d embedding requests (must never pull or embed)", fake.requests())
	}
}

func TestSemanticRuntimeSearchAndPerItemRefresh(t *testing.T) {
	fake := newConceptOllama(t)
	repo := openSemanticTestRepo(t)
	shared := contextstore.Scope{ProjectID: "project", TeamID: "team"}
	if err := repo.Append(t.Context(),
		semanticItem("zh-receipt", "協調者中斷後要用收據判斷已完成的副作用，避免重送。", shared),
		semanticItem("en-lint", "Use golangci-lint with gocyclo threshold 40.", shared),
	); err != nil {
		t.Fatal(err)
	}
	runtime := readySemanticRuntime(t, conceptSettings(t, fake, semantic.ModeActive, nil), repo)
	request := contextstore.SearchRequest{Query: "avoid duplicate side effect after resume", Scope: shared}
	results, err := runtime.SearchVector(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Item.ID != "zh-receipt" {
		t.Fatalf("results = %v", resultIDs(results))
	}

	// A private worker write is outside the shared request: no fallback.
	if err := repo.Append(t.Context(), semanticItem("worker-note", "dark theme note", contextstore.Scope{ProjectID: "project", TeamID: "team", SessionID: "s", AgentID: "worker"})); err != nil {
		t.Fatal(err)
	}
	if _, err = runtime.SearchVector(t.Context(), request); err != nil {
		t.Fatalf("unrelated write invalidated the index: %v", err)
	}

	// A new shared memory is uncovered until the refresher catches up.
	if err := repo.Append(t.Context(), semanticItem("zh-theme", "深色背景下的顏色要跟著切換。", shared)); err != nil {
		t.Fatal(err)
	}
	if _, err = runtime.SearchVector(t.Context(), request); !errors.Is(err, contextstore.ErrSemanticProjectionIncomplete) {
		t.Fatalf("uncovered candidate err = %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		results, err = runtime.SearchVector(t.Context(), request)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("refresher never caught up: %v (status %+v)", err, runtime.Status())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(results) != 3 || results[0].Item.ID != "zh-receipt" {
		t.Fatalf("results after refresh = %v", resultIDs(results))
	}
	if status := runtime.Status(); status.Refreshes == 0 || status.Fallbacks[string(contextstore.SemanticFallbackProjectionIncomplete)] == 0 {
		t.Fatalf("status = %+v", status)
	}
}

func TestSemanticRuntimeTimeoutBusyAndCancellation(t *testing.T) {
	fake := newConceptOllama(t)
	repo := openSemanticTestRepo(t)
	shared := contextstore.Scope{ProjectID: "project", TeamID: "team"}
	if err := repo.Append(t.Context(), semanticItem("zh-receipt", "收據與副作用", shared)); err != nil {
		t.Fatal(err)
	}
	runtime := readySemanticRuntime(t, conceptSettings(t, fake, semantic.ModeActive, func(c *semantic.Config) {
		c.QueryTimeout = 200 * time.Millisecond
	}), repo)
	request := contextstore.SearchRequest{Query: "receipt", Scope: shared}

	// Busy: every slot is held, so the query gives up at its deadline.
	runtime.slot <- struct{}{}
	_, err := runtime.SearchVector(t.Context(), request)
	<-runtime.slot
	if !errors.Is(err, contextstore.ErrSemanticBusy) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy err = %v", err)
	}

	// Caller cancellation is returned as itself, never as a fallback.
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = runtime.SearchVector(canceled, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled err = %v", err)
	}

	// A slow encoder hits the query deadline and triggers a re-warm.
	warmups := runtime.Status().Warmups
	fake.set(func(f *conceptOllama) { f.embedDelay = time.Second })
	if _, err = runtime.SearchVector(t.Context(), request); !errors.Is(err, contextstore.ErrSemanticTimeout) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout err = %v", err)
	}
	fake.set(func(f *conceptOllama) { f.embedDelay = 0 })
	deadline := time.Now().Add(5 * time.Second)
	for runtime.Status().Warmups == warmups || runtime.Status().State != semanticStateReady {
		if time.Now().After(deadline) {
			t.Fatalf("no re-warm after timeout: %+v", runtime.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = runtime.SearchVector(t.Context(), request); err != nil {
		t.Fatalf("query after re-warm: %v", err)
	}
}

func TestSemanticRuntimeTruncatesLongQueries(t *testing.T) {
	fake := newConceptOllama(t)
	repo := openSemanticTestRepo(t)
	shared := contextstore.Scope{ProjectID: "project", TeamID: "team"}
	if err := repo.Append(t.Context(), semanticItem("zh-receipt", "收據與副作用", shared)); err != nil {
		t.Fatal(err)
	}
	runtime := readySemanticRuntime(t, conceptSettings(t, fake, semantic.ModeActive, func(c *semantic.Config) {
		c.QueryMaxTokens = 40
	}), repo)
	long := "Check the receipt first. " + strings.Repeat("unrelated words about something else entirely ", 40)
	if _, err := runtime.SearchVector(t.Context(), contextstore.SearchRequest{Query: long, Scope: shared}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	sent := append([]string(nil), fake.queryInputs...)
	fake.mu.Unlock()
	if len(sent) != 1 || embedding.EstimateTokens(sent[0]) > 30 || !strings.HasPrefix(sent[0], "Check the receipt first.") {
		t.Fatalf("query sent = %q", sent)
	}
	if runtime.Status().TruncatedQueries != 1 {
		t.Fatalf("truncated queries = %d", runtime.Status().TruncatedQueries)
	}
}

func resultIDs(results []contextstore.SearchResult) []string {
	ids := make([]string, len(results))
	for i, result := range results {
		ids[i] = result.Item.ID
	}
	return ids
}
