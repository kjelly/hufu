//go:build linux || darwin
// +build linux darwin

package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

func TestUnboundArtifactScopeDeniesLuaBeforeExecution(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "logs", "artifacts", "data")
	metaRoot := filepath.Join(root, "logs", "artifacts", "meta")
	if err := os.MkdirAll(dataRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(metaRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataRoot, "blocked"), []byte("blocked lua data"), 0o600); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(metaRoot, "blocked.json")
	if err := os.WriteFile(metaPath, []byte("original metadata"), 0o600); err != nil {
		t.Fatal(err)
	}

	inner := tools.NewLuaTool(tools.WithWorkDir(root), tools.WithAllowedPaths([]string{root}))
	gated := (&policyGatedTool{inner: inner, coordinator: gateTestCoordinator()})
	ctx := tools.SetToolsAllowed(context.Background(), []string{"lua"})
	ctx = context.WithValue(ctx, tools.ArtifactPathPolicyKey, tools.ArtifactPathPolicy{
		BlockedPaths:                 []string{dataRoot, metaRoot},
		DenyUnsupportedDeclaredTools: true,
	})
	code := `local f = assert(io.open("logs/artifacts/data/blocked", "r")); local content = f:read("*a"); f:close(); local m = assert(io.open("logs/artifacts/meta/blocked.json", "w")); m:write("lua executed"); m:close(); print(content)`
	input, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		t.Fatal(err)
	}

	response, runErr := gated.Run(ctx, fantasy.ToolCall{ID: "unbound-lua", Name: "lua", Input: string(input)})
	if runErr != nil || !response.IsError {
		t.Fatalf("unbound lua response=%#v err=%v, want pre-execution denial", response, runErr)
	}
	if strings.Contains(response.Content, "blocked lua data") {
		t.Fatalf("lua executed against blocked data: %q", response.Content)
	}
	contents, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "original metadata" {
		t.Fatalf("lua modified blocked metadata: %q", contents)
	}
}

func TestUnboundArtifactScopeAllowsSupportedBuiltInOnNonblockedPath(t *testing.T) {
	root := t.TempDir()
	ordinaryPath := filepath.Join(root, "ordinary.txt")
	if err := os.WriteFile(ordinaryPath, []byte("ordinary content"), 0o600); err != nil {
		t.Fatal(err)
	}

	inner := tools.NewViewTool(tools.WithWorkDir(root), tools.WithAllowedPaths([]string{root}))
	gated := (&policyGatedTool{inner: inner, coordinator: gateTestCoordinator()})
	ctx := tools.SetToolsAllowed(context.Background(), []string{"view"})
	ctx = context.WithValue(ctx, tools.ArtifactPathPolicyKey, tools.ArtifactPathPolicy{
		BlockedPaths:                 []string{filepath.Join(root, "logs", "artifacts", "data"), filepath.Join(root, "logs", "artifacts", "meta")},
		DenyUnsupportedDeclaredTools: true,
	})
	response, runErr := gated.Run(ctx, fantasy.ToolCall{
		ID:    "unbound-view",
		Name:  "view",
		Input: `{"file_path":"ordinary.txt"}`,
	})
	if runErr != nil || response.IsError || !strings.Contains(response.Content, "ordinary content") {
		t.Fatalf("supported built-in response=%#v err=%v, want nonblocked file content", response, runErr)
	}
}

func TestCoordinatorDeclaredToolRunnerDeniesUnboundLua(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "logs", "artifacts", "data")
	metaRoot := filepath.Join(root, "logs", "artifacts", "meta")
	if err := os.MkdirAll(dataRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(metaRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataRoot, "blocked"), []byte("blocked structured lua data"), 0o600); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(metaRoot, "blocked.json")
	if err := os.WriteFile(metaPath, []byte("original structured metadata"), 0o600); err != nil {
		t.Fatal(err)
	}

	def := &agent.AgentDef{Name: "worker", Role: "worker", Tools: "lua"}
	c := &Coordinator{
		session: &TeamSession{
			Workspace: root,
			Config:    agent.TeamConfig{Name: "unbound-lua"},
			Agents:    map[string]*agent.AgentDef{"worker": def},
		},
		projectDir:   root,
		coreTools:    []fantasy.AgentTool{tools.NewLuaTool(tools.WithWorkDir(root), tools.WithAllowedPaths([]string{root}))},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "unbound lua"}})[0]
	code := `local f = assert(io.open("logs/artifacts/data/blocked", "r")); local content = f:read("*a"); f:close(); local m = assert(io.open("logs/artifacts/meta/blocked.json", "w")); m:write("lua executed"); m:close(); print(content)`
	result, runErr := (&coordinatorDeclaredToolRunner{c: c}).RunStructuredStep(context.Background(), StructuredStepRequest{
		TaskID:  item.ID,
		Attempt: 1,
		Step:    ExecutionStep{ID: "inspect", Tool: "lua"},
		ResolvedInput: map[string]any{
			"code": code,
		},
	})
	if runErr != nil || result.ExitCode == 0 || !strings.Contains(result.Stderr, "unbound task") {
		t.Fatalf("structured unbound lua result=%#v err=%v, want pre-execution denial", result, runErr)
	}
	contents, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "original structured metadata" {
		t.Fatalf("structured lua modified blocked metadata: %q", contents)
	}
}
