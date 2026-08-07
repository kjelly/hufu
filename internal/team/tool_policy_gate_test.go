package team

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	name  string
	ran   bool
	calls int
}

func (t *recordingTool) Info() fantasy.ToolInfo { return fantasy.ToolInfo{Name: t.name} }

func (t *recordingTool) ProviderOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{}
}

func (t *recordingTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (t *recordingTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	t.ran = true
	t.calls++
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

func TestPolicyGateEnforcesClosedTaskToolSequence(t *testing.T) {
	c := gateTestCoordinator()
	bash := &recordingTool{name: "bash"}
	ls := &recordingTool{name: "ls"}
	delegate := &recordingTool{name: "request_agent"}
	result := &recordingTool{name: "submit_result"}
	gated := c.gatePolicyTools([]fantasy.AgentTool{bash, ls, delegate, result})
	byName := map[string]fantasy.AgentTool{}
	for _, tool := range gated {
		byName[tool.Info().Name] = tool
	}

	ctx := tools.SetToolsAllowed(context.Background(), []string{"bash", "ls", "request_agent", "submit_result"})
	ctx = context.WithValue(ctx, taskToolSequenceKey{}, newTaskToolSequence([]string{"bash", "bash", "submit_result"}))
	for _, name := range []string{"bash", "bash"} {
		if resp, err := byName[name].Run(ctx, fantasy.ToolCall{Name: name}); err != nil || resp.IsError {
			t.Fatalf("expected sequence tool %q to run: response=%+v err=%v", name, resp, err)
		}
	}

	for _, name := range []string{"ls", "request_agent"} {
		resp, err := byName[name].Run(ctx, fantasy.ToolCall{Name: name})
		if err != nil || !resp.IsError {
			t.Fatalf("extra tool %q must be denied: response=%+v err=%v", name, resp, err)
		}
	}
	if ls.ran || delegate.ran {
		t.Fatalf("closed sequence allowed extra tools: ls=%t request_agent=%t", ls.ran, delegate.ran)
	}

	if resp, err := byName["submit_result"].Run(ctx, fantasy.ToolCall{Name: "submit_result"}); err != nil || resp.IsError {
		t.Fatalf("terminal result must be admitted: response=%+v err=%v", resp, err)
	}
	resp, err := byName["bash"].Run(ctx, fantasy.ToolCall{Name: "bash"})
	if err != nil || !resp.IsError || bash.calls != 2 {
		t.Fatalf("post-result tool must be denied without another execution: response=%+v err=%v bash.calls=%d", resp, err, bash.calls)
	}
}

// TestPolicyGateAdmitsEarlyBlockedSubmitResultButNotSuccess covers the fix
// for a worker that discovers, partway through a closed sequence, that the
// checkpoint cannot proceed (e.g. a prerequisite step's inputs don't exist).
// An honest out-of-position submit_result reporting blocked/failed/partial
// must be admitted immediately rather than rejected as a sequence
// violation — the rejection previously surfaced upstream as "protocol
// incomplete: missing required result" even though the worker had tried to
// report exactly what happened. A success claim must still be rejected
// out of position: the escape hatch is for honest early termination, not a
// shortcut around the remaining evidence-gathering steps.
func TestPolicyGateAdmitsEarlyBlockedSubmitResultButNotSuccess(t *testing.T) {
	c := gateTestCoordinator()
	bash := &recordingTool{name: "bash"}
	result := &recordingTool{name: "submit_result"}
	gated := c.gatePolicyTools([]fantasy.AgentTool{bash, result})
	byName := map[string]fantasy.AgentTool{}
	for _, tool := range gated {
		byName[tool.Info().Name] = tool
	}

	ctx := tools.SetToolsAllowed(context.Background(), []string{"bash", "submit_result"})
	ctx = context.WithValue(ctx, taskToolSequenceKey{}, newTaskToolSequence([]string{"bash", "bash", "bash", "submit_result"}))

	if resp, err := byName["bash"].Run(ctx, fantasy.ToolCall{Name: "bash"}); err != nil || resp.IsError {
		t.Fatalf("expected first sequence bash call to run: response=%+v err=%v", resp, err)
	}

	// Only 1 of 3 required bash calls has run. A success claim here is a
	// shortcut around the remaining steps and must still be denied.
	successCall := fantasy.ToolCall{Name: "submit_result", Input: `{"status":"success","summary":"done"}`}
	if resp, err := byName["submit_result"].Run(ctx, successCall); err != nil || !resp.IsError {
		t.Fatalf("out-of-position success claim must be denied: response=%+v err=%v", resp, err)
	}
	if result.ran {
		t.Fatal("denied success claim must not execute")
	}

	// A blocked report, by contrast, is an honest admission the checkpoint
	// cannot proceed and must be admitted despite being out of position.
	blockedCall := fantasy.ToolCall{Name: "submit_result", Input: `{"status":"blocked","summary":"prerequisite files are missing"}`}
	resp, err := byName["submit_result"].Run(ctx, blockedCall)
	if err != nil || resp.IsError {
		t.Fatalf("early blocked submit_result must be admitted: response=%+v err=%v", resp, err)
	}
	if !result.ran {
		t.Fatal("admitted blocked submit_result should have executed")
	}

	// The escape hatch closes the sequence like any other terminal
	// submit_result — nothing may run after it.
	if resp, err := byName["bash"].Run(ctx, fantasy.ToolCall{Name: "bash"}); err != nil || !resp.IsError {
		t.Fatalf("post-result tool must be denied: response=%+v err=%v", resp, err)
	}
	if bash.calls != 1 {
		t.Fatalf("bash should have run exactly once, ran %d times", bash.calls)
	}
}

func TestFilterToolsForSequenceHidesUnrelatedTools(t *testing.T) {
	bash := &recordingTool{name: "bash"}
	ls := &recordingTool{name: "ls"}
	delegate := &recordingTool{name: "request_agent"}
	result := &recordingTool{name: "submit_result"}
	filtered := filterToolsForSequence(
		[]fantasy.AgentTool{bash, ls, delegate, result},
		[]string{"bash", "bash", "submit_result"},
	)
	if got, want := agentToolNames(filtered), []string{"bash", "submit_result"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("visible sequence tools = %v, want %v", got, want)
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
