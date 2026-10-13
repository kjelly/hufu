package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const fakeDigest = "85462619ee721b466c5927d109d4cb765861907d5417b9109caebc4e614679f1"

// fakeOllama is a minimal /api/tags, /api/show, /api/embed, /api/ps server.
type fakeOllama struct {
	mu            sync.Mutex
	dims          int
	contextLength int
	digest        string
	loadedCtx     int
	embedStatus   int
	embedError    string
	wrongCount    bool
	badValue      bool
	delay         time.Duration
	requests      []embedRequest
}

func newFakeOllama(t *testing.T) (*fakeOllama, *httptest.Server) {
	t.Helper()
	f := &fakeOllama{dims: 4, contextLength: 2048, digest: fakeDigest}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	return f, server
}

func (f *fakeOllama) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/api/tags":
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{
			"name": "embeddinggemma:latest", "model": "embeddinggemma:latest", "digest": f.digest,
			"details": map[string]string{"family": "gemma3", "quantization_level": "BF16"},
		}}})
	case "/api/show":
		_ = json.NewEncoder(w).Encode(map[string]any{"model_info": map[string]any{
			"general.architecture": "gemma3", "gemma3.context_length": f.contextLength, "gemma3.embedding_length": f.dims,
		}})
	case "/api/ps":
		models := []map[string]any{}
		if f.loadedCtx > 0 {
			models = append(models, map[string]any{"name": "embeddinggemma:latest", "digest": f.digest, "context_length": f.loadedCtx})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
	case "/api/embed":
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.requests = append(f.requests, req)
		if f.delay > 0 {
			select {
			case <-time.After(f.delay):
			case <-r.Context().Done():
				return
			}
		}
		if f.embedStatus != 0 {
			w.WriteHeader(f.embedStatus)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": f.embedError})
			return
		}
		f.loadedCtx = req.Options["num_ctx"]
		count := len(req.Input)
		if f.wrongCount && count > 0 {
			count--
		}
		embeddings := make([][]float64, count)
		for i := range embeddings {
			vector := make([]float64, f.dims)
			// Encode the input index so batch order can be checked.
			vector[i%f.dims] = 3
			vector[(i+1)%f.dims] = 4
			if f.badValue {
				vector[0] = 0
				vector[1] = 0
			}
			embeddings[i] = vector
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": embeddings, "prompt_eval_count": 7 * len(req.Input)})
	default:
		http.NotFound(w, r)
	}
}

func resolveFake(t *testing.T, server *httptest.Server, settings ProfileSettings) (*OllamaEmbedder, error) {
	t.Helper()
	client, err := NewOllamaClient(server.URL+"/api", nil)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Model == "" {
		settings.Model = "ollama/embeddinggemma:latest"
	}
	profile, query, err := ResolveProfile(context.Background(), client, settings)
	if err != nil {
		return nil, err
	}
	return NewOllamaEmbedder(client, profile, query, time.Minute)
}

func TestResolveProfilePinsDigestAndLimits(t *testing.T) {
	tests := []struct {
		name     string
		settings ProfileSettings
		ctxLen   int
		wantErr  string
		wantMax  int
	}{
		{name: "preset", ctxLen: 2048, wantMax: 2048},
		{name: "override below model limit", settings: ProfileSettings{MaxInputTokens: 1024, QueryMaxTokens: 256}, ctxLen: 2048, wantMax: 1024},
		{name: "override above model limit", settings: ProfileSettings{MaxInputTokens: 4096}, ctxLen: 2048, wantErr: "exceed"},
		{name: "query above input", settings: ProfileSettings{MaxInputTokens: 256, QueryMaxTokens: 512}, ctxLen: 2048, wantErr: "query max tokens"},
		{name: "unknown model", settings: ProfileSettings{Model: "nomic-embed-text"}, ctxLen: 2048, wantErr: "no measured preset"},
		{name: "missing model", settings: ProfileSettings{Model: "qwen3-embedding:0.6b"}, ctxLen: 2048, wantErr: "not present"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake, server := newFakeOllama(t)
			fake.contextLength = tc.ctxLen
			embedder, err := resolveFake(t, server, tc.settings)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			profile := embedder.Profile()
			if profile.ArtifactDigest != fakeDigest || profile.MaxInputTokens != tc.wantMax || profile.Dimensions != 4 {
				t.Fatalf("profile = %+v", profile)
			}
		})
	}
}

func TestOllamaEmbedderRequestsAndValidation(t *testing.T) {
	fake, server := newFakeOllama(t)
	embedder, err := resolveFake(t, server, ProfileSettings{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	vectors, err := embedder.EmbedDocuments(ctx, []string{"first", "second", "third"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 3 || vectors[0][0] != 0.6 || vectors[1][1] != 0.6 || vectors[2][2] != 0.6 {
		t.Fatalf("batch order or normalization wrong: %v", vectors)
	}
	query, err := embedder.EmbedQueryDetailed(ctx, "check   receipts")
	if err != nil {
		t.Fatal(err)
	}
	if query.Shaped.Text != "check receipts" || query.PromptTokens != 7 {
		t.Fatalf("query = %+v", query)
	}
	if err := embedder.Preload(ctx); err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	requests := append([]embedRequest(nil), fake.requests...)
	fake.mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("got %d embed requests", len(requests))
	}
	for i, req := range requests {
		if req.Truncate || req.Options["num_ctx"] != 2048 || req.KeepAlive != "1m0s" || req.Model != "embeddinggemma:latest" {
			t.Fatalf("request %d = %+v", i, req)
		}
	}
	if requests[0].Input[0] != "title: none | text: first" {
		t.Fatalf("document prefix missing: %q", requests[0].Input[0])
	}
	if requests[1].Input[0] != "task: search result | query: check receipts" {
		t.Fatalf("query prefix missing: %q", requests[1].Input[0])
	}
	if len(requests[2].Input) != 0 {
		t.Fatalf("preload sent input %v", requests[2].Input)
	}
	if err := embedder.VerifyLoaded(ctx); err != nil {
		t.Fatalf("VerifyLoaded after preload: %v", err)
	}
}

func TestOllamaEmbedderTypedFailures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fakeOllama)
		want  error
	}{
		{name: "too long", setup: func(f *fakeOllama) {
			f.embedStatus, f.embedError = http.StatusBadRequest, "the input length exceeds the context length"
		}, want: ErrEmbeddingInputTooLong},
		{name: "model gone", setup: func(f *fakeOllama) {
			f.embedStatus, f.embedError = http.StatusNotFound, `model "embeddinggemma:latest" not found`
		}, want: ErrModelNotFound},
		{name: "server error", setup: func(f *fakeOllama) { f.embedStatus = http.StatusInternalServerError }, want: ErrOllamaUnavailable},
		{name: "count mismatch", setup: func(f *fakeOllama) { f.wrongCount = true }, want: ErrEmbeddingResponse},
		{name: "zero vector", setup: func(f *fakeOllama) { f.badValue = true }, want: ErrInvalidEmbedding},
		{name: "wrong dimensions", setup: func(f *fakeOllama) { f.dims = 5 }, want: ErrEmbeddingResponse},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake, server := newFakeOllama(t)
			embedder, err := resolveFake(t, server, ProfileSettings{})
			if err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			tc.setup(fake)
			fake.mu.Unlock()
			_, err = embedder.EmbedDocuments(context.Background(), []string{"a", "b"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestOllamaEmbedderPreservesCancellation(t *testing.T) {
	fake, server := newFakeOllama(t)
	embedder, err := resolveFake(t, server, ProfileSettings{})
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.delay = time.Second
	fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = embedder.EmbedQuery(ctx, "slow")
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrOllamaUnavailable) {
		t.Fatalf("err = %v, want deadline exceeded only", err)
	}
}

func TestOllamaEndpointUnavailableAndProfileChecks(t *testing.T) {
	fake, server := newFakeOllama(t)
	embedder, err := resolveFake(t, server, ProfileSettings{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := embedder.VerifyLoaded(ctx); !errors.Is(err, ErrModelProfileMismatch) {
		t.Fatalf("VerifyLoaded before load = %v", err)
	}
	fake.mu.Lock()
	fake.loadedCtx = 4096
	fake.mu.Unlock()
	if err := embedder.VerifyLoaded(ctx); !errors.Is(err, ErrModelProfileMismatch) {
		t.Fatalf("VerifyLoaded with other context = %v", err)
	}
	fake.mu.Lock()
	fake.digest = strings.Repeat("c", 64)
	fake.mu.Unlock()
	if err := embedder.CheckDigest(ctx); !errors.Is(err, ErrModelProfileMismatch) {
		t.Fatalf("CheckDigest after tag drift = %v", err)
	}
	server.Close()
	if _, err := embedder.EmbedQuery(ctx, "x"); !errors.Is(err, ErrOllamaUnavailable) {
		t.Fatalf("closed server err = %v", err)
	}
}

func TestRedactEndpoint(t *testing.T) {
	tests := map[string]string{
		"http://user:secret@127.0.0.1:11434/api?token=x": "http://127.0.0.1:11434/api",
		"not a url": "<invalid>",
	}
	for in, want := range tests {
		if got := RedactEndpoint(in); got != want {
			t.Fatalf("RedactEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}
