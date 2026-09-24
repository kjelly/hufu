package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
)

type executionRootFixture struct {
	canonical, control, worlds, root, sibling string
}

func newExecutionRootFixture(t *testing.T) executionRootFixture {
	t.Helper()
	canonical, control := t.TempDir(), t.TempDir()
	worlds := filepath.Join(control, "attempt-worlds")
	fixture := executionRootFixture{
		canonical: canonical, control: control, worlds: worlds,
		root:    filepath.Join(worlds, "aw-1", "root"),
		sibling: filepath.Join(worlds, "aw-2", "root"),
	}
	for _, dir := range []string{fixture.root, fixture.sibling} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(canonical, "a.txt"), "canonical marker\n")
	mustWrite(filepath.Join(fixture.root, "a.txt"), "isolated marker\n")
	mustWrite(filepath.Join(fixture.root, "isolated-only.txt"), "only here\n")
	return fixture
}

var executionRootTestTools = []string{"view", "grep", "glob", "ls", "bash", "write", "edit", "create_skill"}

func (f executionRootFixture) ctx() context.Context {
	ctx := context.WithValue(context.Background(), AgentToolsAllowedKey, executionRootTestTools)
	return context.WithValue(ctx, AgentExecutionRootKey, AgentExecutionRoot{
		Root: f.root, DeniedWriteRoots: []string{f.canonical, f.worlds},
	})
}

func (f executionRootFixture) options() []ToolOption {
	return []ToolOption{WithWorkDir(f.canonical), WithAllowedPaths([]string{f.canonical, f.control})}
}

func runToolForTest(t *testing.T, ctx context.Context, tool fantasy.AgentTool, input any) fantasy.ToolResponse {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	response, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Name: tool.Info().Name, Input: string(raw)})
	if err != nil {
		t.Fatalf("%s: %v", tool.Info().Name, err)
	}
	return response
}

func TestExecutionRootRebindsReadTools(t *testing.T) {
	f := newExecutionRootFixture(t)
	tests := []struct {
		name  string
		tool  fantasy.AgentTool
		input any
		want  string
	}{
		{name: "view", tool: NewViewTool(f.options()...), input: map[string]any{"file_path": "a.txt"}, want: "isolated marker"},
		{name: "grep", tool: NewGrepTool(f.options()...), input: map[string]any{"pattern": "marker"}, want: "isolated"},
		{name: "glob", tool: NewGlobTool(f.options()...), input: map[string]any{"pattern": "*.txt"}, want: "isolated-only.txt"},
		{name: "ls", tool: NewLsTool(f.options()...), input: map[string]any{}, want: "a.txt"},
		{name: "bash cwd", tool: NewBashTool(f.options()...), input: map[string]any{"command": "pwd && cat a.txt"}, want: "isolated marker"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := runToolForTest(t, f.ctx(), tt.tool, tt.input)
			if response.IsError || !strings.Contains(response.Content, tt.want) {
				t.Fatalf("%s response = %#v, want it to contain %q", tt.name, response, tt.want)
			}
			if strings.Contains(response.Content, "canonical marker") {
				t.Fatalf("%s read the canonical project: %q", tt.name, response.Content)
			}
		})
	}
	// Without an execution root nothing changes.
	plain := runToolForTest(t, context.WithValue(context.Background(), AgentToolsAllowedKey, executionRootTestTools), NewViewTool(f.options()...), map[string]any{"file_path": "a.txt"})
	if !strings.Contains(plain.Content, "canonical marker") {
		t.Fatalf("view without an execution root = %q", plain.Content)
	}
}

func TestExecutionRootConfinesWrites(t *testing.T) {
	f := newExecutionRootFixture(t)
	write := NewWriteTool(f.options()...)

	created := runToolForTest(t, f.ctx(), write, map[string]any{"file_path": "b.txt", "content": "new\n"})
	if created.IsError {
		t.Fatalf("relative write = %#v", created)
	}
	if _, err := os.Stat(filepath.Join(f.root, "b.txt")); err != nil {
		t.Fatalf("relative write did not land in the execution root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.canonical, "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("relative write touched the canonical project: %v", err)
	}

	for name, target := range map[string]string{
		"canonical project": filepath.Join(f.canonical, "escape.txt"),
		"sibling world":     filepath.Join(f.sibling, "escape.txt"),
	} {
		t.Run(name, func(t *testing.T) {
			response := runToolForTest(t, f.ctx(), write, map[string]any{"file_path": target, "content": "escape\n"})
			if !response.IsError {
				t.Fatalf("write into the %s was accepted: %#v", name, response)
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("write into the %s created %s", name, target)
			}
		})
	}

	// Control-workspace writes outside the attempt worlds keep today's rule.
	note := filepath.Join(f.control, "notes.md")
	if response := runToolForTest(t, f.ctx(), write, map[string]any{"file_path": note, "content": "note\n"}); response.IsError {
		t.Fatalf("control workspace write = %#v", response)
	}

	// A symlinked directory inside the root that points at the canonical
	// project is judged by where the write lands.
	if err := os.Symlink(f.canonical, filepath.Join(f.root, "linked")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	response := runToolForTest(t, f.ctx(), write, map[string]any{"file_path": "linked/through.txt", "content": "escape\n"})
	if !response.IsError {
		t.Fatalf("write through a symlink into the canonical project was accepted: %#v", response)
	}
	if _, err := os.Stat(filepath.Join(f.canonical, "through.txt")); !os.IsNotExist(err) {
		t.Fatal("write through a symlink reached the canonical project")
	}
}

func TestExecutionRootKeepsBashEnabledAndScopesGit(t *testing.T) {
	f := newExecutionRootFixture(t)
	cfg := cfgWithMergedPaths(ToolConfig{WorkDir: f.canonical, AllowedPaths: []string{f.canonical}}, f.ctx())
	if len(cfg.AllowedWritePaths) != 0 || cfg.WorkDir != f.root || cfg.ExecutionRoot != f.root {
		t.Fatalf("merged config = %#v", cfg)
	}
	response := runToolForTest(t, f.ctx(), NewBashTool(f.options()...), map[string]any{"command": `echo "pwd=$PWD ceiling=$GIT_CEILING_DIRECTORIES"`})
	if response.IsError || !strings.Contains(response.Content, "pwd="+f.root) || !strings.Contains(response.Content, "ceiling="+filepath.Dir(f.root)) {
		t.Fatalf("bash environment = %#v", response)
	}
}

func TestExecutionRootRejectsBoundedTaskPathScope(t *testing.T) {
	f := newExecutionRootFixture(t)
	scope := AgentTaskPathScope{ReadBounded: true, ReadPaths: []string{"a.txt"}, WriteBounded: true, WritePaths: []string{"a.txt"}}
	ctx := context.WithValue(f.ctx(), AgentTaskPathScopeKey, scope)
	response := runToolForTest(t, ctx, NewWriteTool(f.options()...), map[string]any{"file_path": "a.txt", "content": "x\n"})
	if !response.IsError {
		t.Fatalf("bounded scope with an execution root was accepted: %#v", response)
	}
}

func TestExecutionRootRebindsCreateSkill(t *testing.T) {
	f := newExecutionRootFixture(t)
	response := runToolForTest(t, f.ctx(), NewCreateSkillTool(f.options()...), map[string]any{
		"name": "attempt-skill", "description": "made in an attempt", "content": "# Skill\n",
	})
	if response.IsError {
		t.Fatalf("create_skill = %#v", response)
	}
	if _, err := os.Stat(filepath.Join(f.root, "skills", "attempt-skill", "SKILL.md")); err != nil {
		t.Fatalf("skill not created in the execution root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.canonical, "skills")); !os.IsNotExist(err) {
		t.Fatal("create_skill wrote into the canonical project")
	}
}

func TestCanonicalWritePathResolvesDeepestExistingAncestor(t *testing.T) {
	base := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := canonicalWritePath(filepath.Join(link, "missing", "file.txt")); got != filepath.Join(resolvedTarget, "missing", "file.txt") {
		t.Fatalf("canonicalWritePath = %q", got)
	}
	tests := []struct {
		root, path string
		want       bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b", "/a/b/c", true},
		{"/a/b", "/a/bc", false},
		{"/a/b", "/a", false},
		{"/a/b", "/a/b/../c", false},
	}
	for _, tt := range tests {
		if got := pathWithinRoot(tt.root, filepath.Clean(tt.path)); got != tt.want {
			t.Fatalf("pathWithinRoot(%q, %q) = %v, want %v", tt.root, tt.path, got, tt.want)
		}
	}
}
