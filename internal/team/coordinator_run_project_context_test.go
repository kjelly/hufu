package team

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/sidecar"
)

type projectContextTextCompacter struct {
	calls       int
	text        string
	instruction string
	result      string
	deadline    time.Time
}

func (c *projectContextTextCompacter) Compact(ctx context.Context, text, instruction string) (string, error) {
	c.calls++
	c.deadline, _ = ctx.Deadline()
	c.text, c.instruction = text, instruction
	return c.result, nil
}

func (c *projectContextTextCompacter) CompactStructured(context.Context, string, string, string) (string, error) {
	panic("project instructions must never use conversation JSON compaction")
}

func TestCoordinatorCompilerCompactsProjectOnceWithBoundedPlainTextCall(t *testing.T) {
	compacter := &projectContextTextCompacter{result: "condensed project conventions"}
	compiled, err := CompileCoordinatorContext(t.Context(), CoordinatorContextInput{
		CorePrompt: "coordinate", ProjectContext: strings.Repeat("convention ", 600), SidecarCompacter: compacter,
	})
	if err != nil || compacter.calls != 1 || !strings.Contains(compiled.Prompt, compacter.result) {
		t.Fatalf("project compaction calls=%d err=%v prompt=%s", compacter.calls, err, compiled.Prompt)
	}
	if remaining := time.Until(compacter.deadline); remaining <= 0 || remaining > auxiliarySummaryTimeout {
		t.Fatalf("project auxiliary call was not bounded: %s", remaining)
	}
}

func TestBuildSystemPromptDoesNotCompactShadowProjectAgain(t *testing.T) {
	var calls atomic.Int32
	server := newIPv4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"project-summary","object":"chat.completion","created":1,"model":"project-test","choices":[{"index":0,"message":{"role":"assistant","content":"condensed project conventions"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	provider, err := agent.NewOpenAICompatibleProvider(server.URL, "", "local")
	if err != nil {
		t.Fatal(err)
	}
	sc, err := sidecar.NewSidecar(t.Context(), provider, "project-test")
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "AGENTS.md"), []byte(strings.Repeat("project convention ", 300)), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newBudgetCoordinator(t)
	c.projectDir = project
	c.session.Workspace = t.TempDir()
	c.SetAgentPool(&mockAgentPool{sidecar: sc})
	orch := &agent.AgentDef{Name: "coordinator", Role: "coordinator", System: "coordinate safely", Generation: agent.GenerationParams{Model: "test"}}
	c.session.Agents = map[string]*agent.AgentDef{"coordinator": orch}
	prompt, err := c.buildSystemPrompt(t.Context(), orch, "audit", false)
	if err != nil || calls.Load() != 1 || !strings.Contains(prompt, "condensed project conventions") {
		t.Fatalf("canonical project assembly: calls=%d err=%v prompt=%s", calls.Load(), err, prompt)
	}
}

func TestSmallRoleSummaryDoesNotCallSidecar(t *testing.T) {
	// No pool is installed: reaching the sidecar path would fail this test.
	c := &Coordinator{}
	if got := c.summarizeSystem(t.Context(), "short worker role"); got != "short worker role" {
		t.Fatalf("unnecessary role summary: %q", got)
	}
}

func TestCancelledRoleSummaryFallsBackLocally(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c := &Coordinator{}
	c.SetAgentPool(&mockAgentPool{})
	if got := c.summarizeSystem(ctx, strings.Repeat("x", 600)); len(got) != 503 {
		t.Fatalf("cancelled summary did not use bounded local preview: %q", got)
	}
}

func TestCompactProjectContextUsesTextCompactionForLargeAgentsMD(t *testing.T) {
	agentsMD := "# Project Instructions\n" + strings.Repeat("Keep this convention. ", 250)
	compacter := &projectContextTextCompacter{result: "# Project Instructions\n\nCondensed convention."}

	got := compactProjectContext(t.Context(), compacter, agentsMD)
	if got != compacter.result {
		t.Fatalf("legacy project context = %q, want plain-text compacted result %q", got, compacter.result)
	}
	if compacter.calls != 1 || compacter.text != agentsMD {
		t.Fatalf("Compact calls/text = %d/%q, want 1/original project context", compacter.calls, compacter.text)
	}
	if !strings.Contains(compacter.instruction, "project context") || !strings.Contains(compacter.instruction, "all key facts") {
		t.Fatalf("unexpected legacy compact instruction: %q", compacter.instruction)
	}
	if strings.Contains(got, `"goal"`) || strings.Contains(got, `"completed_tasks"`) {
		t.Fatalf("legacy project context must not be a structured conversation summary: %q", got)
	}
}
