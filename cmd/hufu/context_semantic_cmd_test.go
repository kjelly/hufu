package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
)

// fakeSemanticOllama serves the subset of the Ollama API the semantic
// commands use, with a deterministic 4-dimensional encoder.
type fakeSemanticOllama struct {
	mu         sync.Mutex
	digest     string
	embedCalls int
}

func (f *fakeSemanticOllama) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/api/tags":
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"name": "embeddinggemma:latest", "digest": f.digest}}})
	case "/api/show":
		_ = json.NewEncoder(w).Encode(map[string]any{"model_info": map[string]any{"general.architecture": "gemma3", "gemma3.context_length": 2048, "gemma3.embedding_length": 4}})
	case "/api/embed":
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.embedCalls++
		embeddings := make([][]float64, len(req.Input))
		for i, input := range req.Input {
			embeddings[i] = []float64{float64(len(input)%7 + 1), 1, 2, 3}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": embeddings})
	default:
		http.NotFound(w, r)
	}
}

func setupSemanticCommandWorkspace(t *testing.T) (string, *fakeSemanticOllama) {
	t.Helper()
	fake := &fakeSemanticOllama{digest: strings.Repeat("a", 64)}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "hufu.yaml"), []byte("semantic-retrieval:\n  mode: shadow\n  endpoint: "+server.URL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	scope := contextstore.Scope{ProjectID: "p", TeamID: "team"}
	if err := repo.Append(t.Context(),
		contextstore.ContextItem{ID: "a", Kind: contextstore.ContextDecision, Content: "first memory", Scope: scope},
		contextstore.ContextItem{ID: "b", Kind: contextstore.ContextDecision, Content: "second memory", Scope: scope},
	); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	return workspace, fake
}

func runSemanticCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newContextSemanticCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func semanticJSONReport(t *testing.T, args ...string) semanticReport {
	t.Helper()
	output, err := runSemanticCommand(t, append(args, "--json")...)
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, output)
	}
	var report semanticReport
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("decode %v: %v\n%s", args, err, output)
	}
	return report
}

func TestContextSemanticCommandsLifecycle(t *testing.T) {
	workspace, fake := setupSemanticCommandWorkspace(t)

	status := semanticJSONReport(t, "status", "-w", workspace)
	if status.ProjectID != "p" || status.InventoryItems != 2 || status.Active != nil || status.Uncovered != 2 || status.Settings.Mode != "shadow" {
		t.Fatalf("initial status = %+v", status)
	}

	output, err := runSemanticCommand(t, "build", "-w", workspace, "--dry-run")
	if err != nil || !strings.Contains(output, "would reuse 0, embed 2") {
		t.Fatalf("dry run = %q, %v", output, err)
	}
	if fake.embedCalls != 0 {
		t.Fatal("dry run contacted the encoder")
	}

	output, err = runSemanticCommand(t, "build", "-w", workspace)
	if err != nil || !strings.Contains(output, "embedded 2") {
		t.Fatalf("build = %q, %v", output, err)
	}
	if output, err = runSemanticCommand(t, "build", "-w", workspace); err != nil || !strings.Contains(output, "is current") {
		t.Fatalf("second build = %q, %v", output, err)
	}

	verified := semanticJSONReport(t, "verify", "-w", workspace)
	if verified.Active == nil || verified.Covered != 2 || verified.ProfileCurrent == nil || !*verified.ProfileCurrent || len(verified.Problems) != 0 {
		t.Fatalf("verify = %+v", verified)
	}

	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Append(t.Context(), contextstore.ContextItem{ID: "c", Kind: contextstore.ContextDecision, Content: "third memory", Scope: contextstore.Scope{ProjectID: "p"}}); err != nil {
		t.Fatal(err)
	}
	_ = repo.Close()
	if lenient := semanticJSONReport(t, "verify", "-w", workspace, "--offline"); len(lenient.Warnings) != 1 || lenient.Uncovered != 1 {
		t.Fatalf("lenient verify = %+v", lenient)
	}
	if _, err = runSemanticCommand(t, "verify", "-w", workspace, "--offline", "--strict"); err == nil {
		t.Fatal("strict verify passed with an uncovered item")
	}
	if output, err = runSemanticCommand(t, "build", "-w", workspace); err != nil || !strings.Contains(output, "reused 2, embedded 1") {
		t.Fatalf("incremental build = %q, %v", output, err)
	}

	fake.mu.Lock()
	fake.digest = strings.Repeat("b", 64)
	fake.mu.Unlock()
	output, err = runSemanticCommand(t, "verify", "-w", workspace)
	if err == nil || !strings.Contains(output, "different encoder profile") {
		t.Fatalf("verify after digest drift = %q, %v", output, err)
	}
}
