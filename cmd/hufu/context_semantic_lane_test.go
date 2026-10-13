package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
)

func (f *fakeSemanticOllama) counts() (requests, inputs int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests, f.embedInputs
}

// runContextCommandForTest runs one context subcommand with the shared
// context flags reset, returning stdout and stderr separately.
func runContextCommandForTest(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	reset := func() {
		contextWorkspace, contextProject, contextTeam, contextAgent, contextTier, contextLifecycle = "", "", "", "", "", ""
		contextQueryJSON, contextShowContent, contextRebuildVector, contextRebuildAggregates = false, false, false, false
		contextQuerySemanticMode = ""
	}
	reset()
	t.Cleanup(reset)
	root := newRootCommand()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(args)
	err := root.Execute()
	return stdout.String(), stderr.String(), err
}

func TestContextQuerySemanticLaneFollowsMode(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		flag     string
		query    string
		wantLane bool
	}{
		{name: "off stays lexical", mode: "off", query: "memory"},
		{name: "shadow never changes selection", mode: "shadow", query: "memory"},
		{name: "auto keeps English lexical", mode: "auto", query: "memory"},
		{name: "auto routes Chinese to the lane", mode: "auto", query: "記憶體內容", wantLane: true},
		{name: "active uses the lane", mode: "active", query: "memory", wantLane: true},
		{name: "flag overrides hufu.yaml", mode: "off", flag: "active", query: "memory", wantLane: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			workspace, fake := setupSemanticWorkspace(t, tc.mode)
			args := []string{"context", "query", "--workspace", workspace, "--project", "p", "--team", "team", "--json"}
			if tc.flag != "" {
				args = append(args, "--semantic-mode", tc.flag)
			}
			_, stderr, err := runContextCommandForTest(t, append(args, tc.query)...)
			if err != nil || stderr != "" {
				t.Fatalf("context query: %v\n%s", err, stderr)
			}
			requests, inputs := fake.counts()
			switch {
			case !tc.wantLane && requests != 0:
				t.Fatalf("lexical-only query contacted the encoder %d times", requests)
			case tc.wantLane && inputs != 3:
				t.Fatalf("lane query embedded %d texts, want 2 memories and the query", inputs)
			}
		})
	}
}

func TestContextQueryEmbedsOnlyNewMemories(t *testing.T) {
	workspace, fake := setupSemanticWorkspace(t, "active")
	steps := []struct {
		name       string
		add        *contextstore.ContextItem
		wantInputs int
	}{
		{name: "first query embeds every memory", wantInputs: 3},
		{name: "unchanged store embeds only the query", wantInputs: 1},
		{name: "a new memory is embedded once", add: &contextstore.ContextItem{ID: "c", Kind: contextstore.ContextDecision, Content: "third memory", Scope: contextstore.Scope{ProjectID: "p", TeamID: "team"}}, wantInputs: 2},
	}
	for _, step := range steps {
		if step.add != nil {
			repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			if err = repo.Append(t.Context(), *step.add); err != nil {
				t.Fatal(err)
			}
			if err = repo.Close(); err != nil {
				t.Fatal(err)
			}
		}
		_, before := fake.counts()
		if _, stderr, err := runContextCommandForTest(t, "context", "query", "--workspace", workspace, "--project", "p", "--team", "team", "--json", "memory"); err != nil || stderr != "" {
			t.Fatalf("%s: %v\n%s", step.name, err, stderr)
		}
		if _, after := fake.counts(); after-before != step.wantInputs {
			t.Fatalf("%s: embedded %d texts, want %d", step.name, after-before, step.wantInputs)
		}
	}
}

func TestContextQueryFallsBackWhenTheEncoderIsDown(t *testing.T) {
	workspace, _ := setupSemanticWorkspace(t, "active")
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	if err := os.WriteFile("hufu.yaml", []byte("semantic-retrieval:\n  mode: active\n  endpoint: "+closed.URL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runContextCommandForTest(t, "context", "query", "--workspace", workspace, "--project", "p", "--team", "team", "--json", "memory")
	if err != nil {
		t.Fatalf("query must fall back to exact and lexical retrieval: %v", err)
	}
	if !strings.Contains(stderr, "semantic lane unavailable") || !strings.Contains(stdout, `"a"`) {
		t.Fatalf("stdout=%s stderr=%s", stdout, stderr)
	}
}

func TestContextRebuildVectorRefreshesTheSemanticGeneration(t *testing.T) {
	// rebuild --vector is an explicit build, like `context semantic build`,
	// so it does not depend on the configured mode.
	workspace, fake := setupSemanticWorkspace(t, "off")
	for _, want := range []string{"covers 2 items (reused 0, embedded 2, excluded 0)", "covers 2 items (reused 2, embedded 0, excluded 0)"} {
		stdout, stderr, err := runContextCommandForTest(t, "context", "rebuild", "--workspace", workspace, "--project", "p", "--vector")
		if err != nil || !strings.Contains(stdout, want) {
			t.Fatalf("rebuild --vector = %q, %v\n%s; want %q", stdout, err, stderr, want)
		}
	}
	if _, inputs := fake.counts(); inputs != 2 {
		t.Fatalf("rebuilds embedded %d texts, want each memory once", inputs)
	}
}

func TestConflictsScanVectorUsesStoredVectors(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		wantVector bool
	}{
		{name: "shadow leaves --vector unavailable", mode: "shadow"},
		{name: "active embeds memories but never a query", mode: "active", wantVector: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := serveFakeSemanticOllama(t, tc.mode)
			workspace, search := t.TempDir(), t.TempDir()
			helperSetupTeam(t, search, "demo")
			seedConflictMemories(t, workspace)
			out, err := helperRunConflictsCLI("context", "conflicts", "scan", "--dry-run", "--vector", "--team-search-path", search, "--workspace", workspace, "--project", "proj1", "--team", "demo")
			if err != nil {
				t.Fatalf("scan: %v\n%s", err, out)
			}
			requests, inputs := fake.counts()
			if tc.wantVector {
				if strings.Contains(out, "vector=unavailable") || inputs != 2 {
					t.Fatalf("vector scan embedded %d texts, want the 2 memories only: %s", inputs, out)
				}
				return
			}
			if !strings.Contains(out, "vector=unavailable") || !strings.Contains(out, "--vector needs active or auto") || requests != 0 {
				t.Fatalf("scan with mode %s contacted the encoder %d times: %s", tc.mode, requests, out)
			}
		})
	}
}
