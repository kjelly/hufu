package team

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/tools"
)

// recordingTool reports whether its Run was reached, so a denial can be
// distinguished from an execution.
type recordingTool struct {
	name string
	ran  bool
}

func (t *recordingTool) Info() fantasy.ToolInfo { return fantasy.ToolInfo{Name: t.name} }

func (t *recordingTool) ProviderOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{}
}

func (t *recordingTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (t *recordingTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	t.ran = true
	return fantasy.NewTextResponse("ran"), nil
}

func gateTestCoordinator() *Coordinator {
	return &Coordinator{
		session:      &TeamSession{Config: agent.TeamConfig{Name: "team"}},
		reportStatus: func(StatusEvent) {},
	}
}

// TestPolicyGateDenialIsRecoverable is the core of root cause 5: a denial must
// reach the model as a tool error it can adapt to. Enforcing in OnToolCall could
// only return an error, and an error there aborts the entire model round —
// discarding every tool call the worker had already completed and burning a
// retry over one ungranted call.
func TestPolicyGateDenialIsRecoverable(t *testing.T) {
	c := gateTestCoordinator()
	inner := &recordingTool{name: "bash"}
	gated := c.gatePolicyTools([]fantasy.AgentTool{inner})[0]

	ctx := tools.SetToolsAllowed(context.Background(), []string{"view"})
	resp, err := gated.Run(ctx, fantasy.ToolCall{Name: "bash"})
	if err != nil {
		t.Fatalf("a denial must not surface as a stream-aborting error: %v", err)
	}
	if !resp.IsError {
		t.Fatalf("denial must be an error response the model can read, got %+v", resp)
	}
	if !strings.Contains(resp.Content, "bash") {
		t.Errorf("denial should name the tool: %q", resp.Content)
	}
	if inner.ran {
		t.Error("denied tool must not execute")
	}
}

func TestPolicyGateAllowsGrantedTool(t *testing.T) {
	c := gateTestCoordinator()
	inner := &recordingTool{name: "bash"}
	gated := c.gatePolicyTools([]fantasy.AgentTool{inner})[0]

	ctx := tools.SetToolsAllowed(context.Background(), []string{"bash", "view"})
	resp, err := gated.Run(ctx, fantasy.ToolCall{Name: "bash"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.IsError {
		t.Fatalf("granted tool must not be denied: %q", resp.Content)
	}
	if !inner.ran {
		t.Error("granted tool should execute")
	}
}

// TestPolicyGateWithoutAllowlistDefersToToolAdapter preserves the behaviour that
// keeps unconstrained teams working: with no allowlist attached, the tool adapter
// in internal/tools remains the source of truth.
func TestPolicyGateWithoutAllowlistDefersToToolAdapter(t *testing.T) {
	c := gateTestCoordinator()
	inner := &recordingTool{name: "bash"}
	gated := c.gatePolicyTools([]fantasy.AgentTool{inner})[0]

	if _, err := gated.Run(context.Background(), fantasy.ToolCall{Name: "bash"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !inner.ran {
		t.Error("with no allowlist attached the call must reach the tool")
	}
}

// TestPolicyGateHonoursSessionPermissions unifies the two gates: an operator's
// explicit session decision is honoured by the tool adapter, so the policy gate
// must honour it too rather than reaching a different verdict on the same tool.
func TestPolicyGateHonoursSessionPermissions(t *testing.T) {
	c := gateTestCoordinator()

	t.Run("session allow overrides a missing grant", func(t *testing.T) {
		inner := &recordingTool{name: "bash"}
		gated := c.gatePolicyTools([]fantasy.AgentTool{inner})[0]
		ctx := tools.SetToolsAllowed(context.Background(), []string{"view"})
		ctx = context.WithValue(ctx, tools.AgentToolsSessionPermissionsKey, map[string]bool{"bash": true})
		if _, err := gated.Run(ctx, fantasy.ToolCall{Name: "bash"}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !inner.ran {
			t.Error("a session-level allow must be honoured by the policy gate")
		}
	})

	t.Run("session deny overrides a grant", func(t *testing.T) {
		inner := &recordingTool{name: "bash"}
		gated := c.gatePolicyTools([]fantasy.AgentTool{inner})[0]
		ctx := tools.SetToolsAllowed(context.Background(), []string{"bash"})
		ctx = context.WithValue(ctx, tools.AgentToolsSessionPermissionsKey, map[string]bool{"bash": false})
		resp, err := gated.Run(ctx, fantasy.ToolCall{Name: "bash"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !resp.IsError || inner.ran {
			t.Errorf("a session-level deny must stop the call: err=%t ran=%t", resp.IsError, inner.ran)
		}
	})
}

// TestPolicyGateCancelledContextIsFatal keeps the one case the model cannot work
// around distinct from an ordinary denial.
func TestPolicyGateCancelledContextIsFatal(t *testing.T) {
	c := gateTestCoordinator()
	inner := &recordingTool{name: "bash"}
	gated := c.gatePolicyTools([]fantasy.AgentTool{inner})[0]

	ctx, cancel := context.WithCancel(tools.SetToolsAllowed(context.Background(), []string{"bash"}))
	cancel()
	if _, err := gated.Run(ctx, fantasy.ToolCall{Name: "bash"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation must abort rather than become a tool error, got %v", err)
	}
}

func TestGatePolicyToolsIsIdempotent(t *testing.T) {
	c := gateTestCoordinator()
	once := c.gatePolicyTools([]fantasy.AgentTool{&recordingTool{name: "bash"}})
	twice := c.gatePolicyTools(once)
	if twice[0] != once[0] {
		t.Fatal("re-gating an already gated tool must not double-wrap it")
	}
}

// TestAgentsAreCreatedThroughTheGatedConstructor is an architectural fitness
// check. Now that OnToolCall no longer aborts on a denial, policyGatedTool is the
// only authorization boundary for agent tool calls — so a tool set that reaches
// agent.CreateAgent without passing through gatePolicyTools would have none at
// all. Funnelling every construction through createGatedAgent is what makes the
// boundary complete, and this test is what keeps the funnel intact.
func TestAgentsAreCreatedThroughTheGatedConstructor(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	call := regexp.MustCompile(`\bagent\.CreateAgent\(`)

	var offenders []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, readErr := os.ReadFile(filepath.Join(".", name))
		if readErr != nil {
			t.Fatalf("ReadFile %s: %v", name, readErr)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if !call.MatchString(line) {
				continue
			}
			// The funnel itself is the one legitimate caller.
			if name == "tool_policy_gate.go" {
				continue
			}
			offenders = append(offenders, fmt.Sprintf("%s:%d", name, i+1))
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("agent.CreateAgent called outside createGatedAgent at %s — those agents would run with no authorization boundary; use c.createGatedAgent instead", strings.Join(offenders, ", "))
	}
}

// TestUnauthorizedExposedTools covers the invariant helper directly, including
// the empty-allowlist case where no policy is attached.
func TestUnauthorizedExposedTools(t *testing.T) {
	tests := []struct {
		name    string
		exposed []string
		allowed []string
		want    []string
	}{
		{
			name:    "no allowlist means nothing is unauthorized",
			exposed: []string{"bash", "submit_result"},
			allowed: nil,
		},
		{
			name:    "fully covered",
			exposed: []string{"bash", "submit_result"},
			allowed: []string{"bash", "submit_result", "view"},
		},
		{
			name:    "reports the gap once",
			exposed: []string{"bash", "submit_result", "submit_result"},
			allowed: []string{"bash"},
			want:    []string{"submit_result"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := unauthorizedExposedTools(tc.exposed, tc.allowed)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("unauthorizedExposedTools = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestValidateToolGrantsAcceptsDefaultTeam checks the startup gate passes for the
// team a --default run uses, so the fail-fast cannot become a false alarm.
func TestValidateToolGrantsAcceptsDefaultTeam(t *testing.T) {
	for _, helperTools := range []string{"", "bash", "bash,terminal", "all"} {
		session, err := LoadDefaultTeam(t.TempDir(), nil, helperTools)
		if err != nil {
			t.Fatalf("LoadDefaultTeam(%q): %v", helperTools, err)
		}
		c := &Coordinator{session: session, coreTools: workerInvariantCoreTools(t)}
		if err := c.validateToolGrants(); err != nil {
			t.Errorf("helperTools=%q: validateToolGrants = %v, want nil", helperTools, err)
		}
	}
}
