package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

// recordingTool reports whether its Run was reached, so a denial can be
// distinguished from an execution.
type recordingTool struct {
	name      string
	ran       bool
	calls     int
	lastInput string
	resp      fantasy.ToolResponse
}

func (t *recordingTool) Info() fantasy.ToolInfo { return fantasy.ToolInfo{Name: t.name} }

func (t *recordingTool) ProviderOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{}
}

func (t *recordingTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (t *recordingTool) Run(_ context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	t.ran = true
	t.calls++
	t.lastInput = call.Input
	if t.resp.IsError {
		return t.resp, nil
	}
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

func TestPolicyGateCoordinatorPolicyDenialsAreTerminal(t *testing.T) {
	tests := []struct {
		name    string
		tool    string
		allowed []string
		policy  agent.DelegationPolicy
	}{
		{
			name:    "coordinator authorization",
			tool:    "bash",
			allowed: []string{"view"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := gateTestCoordinator()
			c.session.Config.Delegation = tt.policy
			c.taskTracker = NewTaskTracker()
			inner := &recordingTool{name: tt.tool}
			gated := c.gatePolicyTools([]fantasy.AgentTool{inner})[0]
			ctx := tools.SetToolsAllowed(context.Background(), tt.allowed)
			ctx = context.WithValue(ctx, todoIDKey{}, CoordTodoID)

			_, err := gated.Run(ctx, fantasy.ToolCall{ID: "coordinator-policy-denial", Name: tt.tool})
			if !errors.Is(err, errCoordinatorToolFailure) {
				t.Fatalf("denial error = %v, want terminal coordinator tool failure", err)
			}
			if inner.ran {
				t.Fatal("denied coordinator tool must not execute")
			}
		})
	}
}

func TestPolicyGateInitialCoordinatorToolGetsOneCorrection(t *testing.T) {
	c := gateTestCoordinator()
	c.session.Config.Delegation = agent.DelegationPolicy{InitialCoordinatorTool: "agent"}
	c.taskTracker = NewTaskTracker()
	inner := &recordingTool{name: "team_info"}
	gated := c.gatePolicyTools([]fantasy.AgentTool{inner})[0]
	ctx := tools.SetToolsAllowed(context.Background(), []string{"team_info", "agent"})
	ctx = context.WithValue(ctx, todoIDKey{}, CoordTodoID)

	resp, err := gated.Run(ctx, fantasy.ToolCall{ID: "first-wrong-tool", Name: "team_info"})
	if err != nil || !resp.IsError || !strings.Contains(resp.Content, `Call the required tool now`) {
		t.Fatalf("first ordering denial = response=%+v err=%v, want recoverable correction", resp, err)
	}
	if inner.ran {
		t.Fatal("denied initial tool must not execute")
	}

	_, err = gated.Run(ctx, fantasy.ToolCall{ID: "second-wrong-tool", Name: "team_info"})
	if !errors.Is(err, errCoordinatorToolFailure) {
		t.Fatalf("second ordering denial error = %v, want terminal coordinator failure", err)
	}
	if inner.ran {
		t.Fatal("repeated denied initial tool must not execute")
	}
}

func TestPolicyGateCoordinatorToolErrorResponseIsTerminal(t *testing.T) {
	c := gateTestCoordinator()
	c.taskTracker = NewTaskTracker()
	inner := &recordingTool{name: "agent", resp: fantasy.NewTextErrorResponse("first delegation must contain exactly the configured initial batch")}
	gated := c.gatePolicyTools([]fantasy.AgentTool{inner})[0]
	ctx := tools.SetToolsAllowed(context.Background(), []string{"agent"})
	ctx = context.WithValue(ctx, todoIDKey{}, CoordTodoID)

	_, err := gated.Run(ctx, fantasy.ToolCall{ID: "invalid-initial-batch", Name: "agent"})
	if !errors.Is(err, errCoordinatorToolFailure) {
		t.Fatalf("tool error = %v, want terminal coordinator tool failure", err)
	}
	if !inner.ran {
		t.Fatal("inner tool should have run before its rejected delegation response")
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
	ctx = context.WithValue(ctx, taskToolSequenceKey{}, newTaskToolSequence([]string{"bash", "bash", "submit_result"}, nil, "", nil))
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

	if resp, err := byName["submit_result"].Run(ctx, fantasy.ToolCall{Name: "submit_result", Input: `{"status":"success","summary":"done"}`}); err != nil || !resp.IsError {
		t.Fatalf("success after an out-of-order tool must be denied: response=%+v err=%v", resp, err)
	}
	if resp, err := byName["submit_result"].Run(ctx, fantasy.ToolCall{Name: "submit_result", Input: `{"status":"failed","summary":"sequence violation"}`}); err != nil || resp.IsError {
		t.Fatalf("failed terminal result must be admitted after a sequence violation: response=%+v err=%v", resp, err)
	}
	resp, err := byName["bash"].Run(ctx, fantasy.ToolCall{Name: "bash"})
	if err != nil || !resp.IsError || bash.calls != 2 {
		t.Fatalf("post-result tool must be denied without another execution: response=%+v err=%v bash.calls=%d", resp, err, bash.calls)
	}
}

func TestPolicyGateEnforcesExactToolInputSequence(t *testing.T) {
	c := gateTestCoordinator()
	bash := &recordingTool{name: "bash"}
	result := &recordingTool{name: "submit_result"}
	gated := c.gatePolicyTools([]fantasy.AgentTool{bash, result})
	byName := map[string]fantasy.AgentTool{}
	for _, tool := range gated {
		byName[tool.Info().Name] = tool
	}

	ctx := tools.SetToolsAllowed(context.Background(), []string{"bash", "submit_result"})
	ctx = context.WithValue(ctx, taskToolSequenceKey{}, newTaskToolSequence(
		[]string{"bash", "submit_result"},
		[]map[string]any{{"command": "pwd"}, {}},
		"",
		nil,
	))

	wrong := fantasy.ToolCall{Name: "bash", Input: `{"command":"go version"}`}
	if resp, err := byName["bash"].Run(ctx, wrong); err != nil || !resp.IsError {
		t.Fatalf("wrong constrained input must be denied: response=%+v err=%v", resp, err)
	}
	if bash.ran {
		t.Fatal("mismatched input must not reach the underlying tool")
	}
	failed := fantasy.ToolCall{Name: "submit_result", Input: `{"status":"failed","summary":"input mismatch"}`}
	if resp, err := byName["submit_result"].Run(ctx, failed); err != nil || resp.IsError {
		t.Fatalf("failed terminal result must be admitted after an input violation: response=%+v err=%v", resp, err)
	}

	ctx = tools.SetToolsAllowed(context.Background(), []string{"bash", "submit_result"})
	ctx = context.WithValue(ctx, taskToolSequenceKey{}, newTaskToolSequence(
		[]string{"bash", "submit_result"},
		[]map[string]any{{"command": "pwd"}, {}},
		"",
		nil,
	))
	matching := fantasy.ToolCall{Name: "bash", Input: `{"command":"pwd"}`}
	if resp, err := byName["bash"].Run(ctx, matching); err != nil || resp.IsError {
		t.Fatalf("matching constrained input must run: response=%+v err=%v", resp, err)
	}
}

func TestPolicyGateFinalTerminalWriteMustWaitBeforeFinalRead(t *testing.T) {
	c := gateTestCoordinator()
	terminal := &recordingTool{name: "terminal"}
	result := &recordingTool{name: "submit_result"}
	gated := c.gatePolicyTools([]fantasy.AgentTool{terminal, result})
	byName := map[string]fantasy.AgentTool{}
	for _, tool := range gated {
		byName[tool.Info().Name] = tool
	}

	inputs := []map[string]any{
		{"action": "start"}, {"action": "read"}, {"action": "write"},
		{"action": "read"}, {"action": "write"}, {"action": "read"},
		{"action": "write"}, {"action": "wait", "target": "exit"},
		{"action": "read"}, {},
	}
	ctx := tools.SetToolsAllowed(context.Background(), []string{"terminal", "submit_result"})
	ctx = context.WithValue(ctx, taskToolSequenceKey{}, newTaskToolSequence(
		[]string{"terminal", "terminal", "terminal", "terminal", "terminal", "terminal", "terminal", "terminal", "terminal", "submit_result"},
		inputs, "", nil,
	))

	for index := 0; index < 7; index++ {
		payload, err := json.Marshal(inputs[index])
		if err != nil {
			t.Fatal(err)
		}
		if resp, err := byName["terminal"].Run(ctx, fantasy.ToolCall{Name: "terminal", Input: string(payload)}); err != nil || resp.IsError {
			t.Fatalf("terminal slot %d = response=%+v err=%v", index+1, resp, err)
		}
	}

	if resp, err := byName["terminal"].Run(ctx, fantasy.ToolCall{Name: "terminal", Input: `{"action":"read"}`}); err != nil || !resp.IsError {
		t.Fatalf("read between final write and wait = response=%+v err=%v, want denial", resp, err)
	}
	if resp, err := byName["terminal"].Run(ctx, fantasy.ToolCall{Name: "terminal", Input: `{"action":"wait","target":"exit"}`}); err != nil || !resp.IsError {
		t.Fatalf("wait after closed-sequence denial = response=%+v err=%v, want denial", resp, err)
	}
	if resp, err := byName["submit_result"].Run(ctx, fantasy.ToolCall{Name: "submit_result", Input: `{"status":"failed","summary":"final write wait barrier violated"}`}); err != nil || resp.IsError {
		t.Fatalf("failed terminal result after barrier violation = response=%+v err=%v", resp, err)
	}
	if terminal.calls != 7 {
		t.Fatalf("terminal calls = %d, want seven admitted calls before barrier", terminal.calls)
	}
}

func TestPolicyGateCanonicalizesTemplateBoundToolInput(t *testing.T) {
	events := []StatusEvent{}
	c := gateTestCoordinator()
	c.reportStatus = func(event StatusEvent) { events = append(events, event) }
	bash := &recordingTool{name: "bash"}
	result := &recordingTool{name: "submit_result"}
	gated := c.gatePolicyTools([]fantasy.AgentTool{bash, result})
	byName := map[string]fantasy.AgentTool{}
	for _, tool := range gated {
		byName[tool.Info().Name] = tool
	}

	ctx := tools.SetToolsAllowed(context.Background(), []string{"bash", "submit_result"})
	ctx = context.WithValue(ctx, todoIDKey{}, "template-bound-task")
	ctx = context.WithValue(ctx, taskToolSequenceKey{}, newTaskToolSequenceWithCanonicalInputs(
		[]string{"bash", "submit_result"},
		[]map[string]any{{"command": "pwd"}, {}},
		"",
		nil,
		nil,
		[]bool{true, false},
	))

	wrong := fantasy.ToolCall{ID: "canonicalized-call", Name: "bash", Input: `{"command":"go version"}`}
	if resp, err := byName["bash"].Run(ctx, wrong); err != nil || resp.IsError {
		t.Fatalf("canonical template input must execute: response=%+v err=%v", resp, err)
	}
	if got, want := bash.lastInput, `{"command":"pwd"}`; got != want {
		t.Fatalf("underlying tool input = %q, want runtime-selected %q", got, want)
	}
	if got := c.takeToolPolicyVerdict("canonicalized-call"); got != "canonicalized" {
		t.Fatalf("policy verdict = %q, want canonicalized", got)
	}
	if len(events) != 1 || events[0].Type != "policy_decision" || !strings.Contains(events[0].Message, "template-owned input selected") {
		t.Fatalf("canonicalization event = %#v, want one policy decision", events)
	}
}

func TestPolicyGateCanonicalizesNestedMenuPreambleBeforeWrite(t *testing.T) {
	events := []StatusEvent{}
	c := gateTestCoordinator()
	c.reportStatus = func(event StatusEvent) { events = append(events, event) }
	write := &recordingTool{name: "write"}
	result := &recordingTool{name: "submit_result"}
	gated := c.gatePolicyTools([]fantasy.AgentTool{write, result})
	byName := map[string]fantasy.AgentTool{}
	for _, tool := range gated {
		byName[tool.Info().Name] = tool
	}

	content := strings.Join([]string{
		"# HUFU_NESTED_MENU_V1 parent_menu_anchor=parent menu",
		"# HUFU_NESTED_MENU_V1 parent_menu_selector=parent item",
		"# HUFU_NESTED_MENU_V1 child_menu_anchor=child menu",
		"# HUFU_NESTED_MENU_V1 child_menu_selector=child item",
		"# HUFU_NESTED_MENU_V1 post_action_guard=child editor",
		"EXPECT parent menu",
		"ACTIVATE parent item WITH ENTER",
		"EXPECT child menu",
		"SPACE",
		"ACTIVATE child item WITH ENTER",
		"EXPECT child editor",
		"CHECKLIST_DOWN 1",
		"SPACE",
	}, "\n")
	ctx := tools.SetToolsAllowed(context.Background(), []string{"write", "submit_result"})
	ctx = context.WithValue(ctx, todoIDKey{}, "ui-probe")
	ctx = context.WithValue(ctx, taskToolSequenceKey{}, newTaskToolSequenceWithBindings(
		[]string{"write", "submit_result"},
		[]map[string]any{{}, {}},
		"",
		nil,
		nil,
		nil,
		[]string{nestedMenuPreambleTransform, ""},
	))
	call := fantasy.ToolCall{ID: "nested-menu-write", Name: "write", Input: `{"file_path":"probe.trec","content":` + strconv.Quote(content) + `}`}
	if resp, err := byName["write"].Run(ctx, call); err != nil || resp.IsError {
		t.Fatalf("structural transform must permit the write: response=%+v err=%v", resp, err)
	}
	var written map[string]any
	if err := json.Unmarshal([]byte(write.lastInput), &written); err != nil {
		t.Fatalf("decode canonical write input: %v", err)
	}
	writtenContent, _ := written["content"].(string)
	for _, want := range []string{
		"EXPECT parent menu\nACTIVATE parent item WITH ENTER\nEXPECT child menu\nACTIVATE child item WITH ENTER\nEXPECT child editor\nCHECKLIST_DOWN 1",
	} {
		if !strings.Contains(writtenContent, want) {
			t.Fatalf("canonical write content missing %q: %s", want, writtenContent)
		}
	}
	if got := written["file_path"]; got != "probe.trec" {
		t.Fatalf("canonical write path = %#v, want probe.trec", got)
	}
	if strings.Contains(writtenContent, "EXPECT child menu\nSPACE\nACTIVATE child item") {
		t.Fatalf("canonical write retained misplaced preamble SPACE: %s", writtenContent)
	}
	if got := c.takeToolPolicyVerdict("nested-menu-write"); got != "transformed" {
		t.Fatalf("policy verdict = %q, want transformed", got)
	}
	if len(events) != 1 || events[0].Type != "policy_decision" || !strings.Contains(events[0].Message, nestedMenuPreambleTransform) {
		t.Fatalf("transform event = %#v, want one policy decision", events)
	}
}

func TestPolicyGateCanonicalizesTerminalOrderedBatchAcknowledgement(t *testing.T) {
	events := []StatusEvent{}
	c := gateTestCoordinator()
	c.reportStatus = func(event StatusEvent) { events = append(events, event) }
	result := &recordingTool{name: "submit_result"}
	gated := c.gatePolicyTools([]fantasy.AgentTool{result})[0]

	findings := make([]map[string]string, 7)
	for i := range findings {
		findings[i] = map[string]string{"summary": fmt.Sprintf("slot %d current-run evidence", i+1)}
	}
	findings[6]["summary"] = "pilot_sha256=trec-value trec_sha256=pilot-value"
	payload, err := json.Marshal(map[string]any{"status": "success", "summary": "freeze complete", "findings": findings})
	if err != nil {
		t.Fatal(err)
	}
	ctx := tools.SetToolsAllowed(context.Background(), []string{"submit_result"})
	ctx = context.WithValue(ctx, todoIDKey{}, "freeze")
	ctx = context.WithValue(ctx, taskToolSequenceKey{}, newTaskToolSequenceWithBindings(
		[]string{"submit_result"}, []map[string]any{{}}, "", nil, nil, nil,
		[]string{terminalLastFindingTranscriptAckTransform},
	))
	call := fantasy.ToolCall{ID: "freeze-terminal", Name: "submit_result", Input: string(payload)}
	if resp, err := gated.Run(ctx, call); err != nil || resp.IsError {
		t.Fatalf("ordered acknowledgement must permit terminal submission: response=%+v err=%v", resp, err)
	}
	var submitted struct {
		Findings []struct {
			Summary string `json:"summary"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(result.lastInput), &submitted); err != nil {
		t.Fatalf("decode transformed submit_result: %v", err)
	}
	if got, want := submitted.Findings[6].Summary, "slot_7 ordered evidence retained in sealed transcript"; got != want {
		t.Fatalf("last summary = %q, want %q", got, want)
	}
	if got := c.takeToolPolicyVerdict("freeze-terminal"); got != "transformed" {
		t.Fatalf("policy verdict = %q, want transformed", got)
	}
	if len(events) != 1 || events[0].Type != "policy_decision" || !strings.Contains(events[0].Message, terminalLastFindingTranscriptAckTransform) {
		t.Fatalf("transform event = %#v, want one policy decision", events)
	}
}

func TestPolicyGateEnforcesScalarToolInputSequence(t *testing.T) {
	c := gateTestCoordinator()
	bash := &recordingTool{name: "bash"}
	result := &recordingTool{name: "submit_result"}
	gated := c.gatePolicyTools([]fantasy.AgentTool{bash, result})
	byName := map[string]fantasy.AgentTool{}
	for _, tool := range gated {
		byName[tool.Info().Name] = tool
	}

	ctx := tools.SetToolsAllowed(context.Background(), []string{"bash", "submit_result"})
	ctx = context.WithValue(ctx, taskToolSequenceKey{}, newTaskToolSequence(
		[]string{"bash", "submit_result"},
		nil,
		"command",
		[]string{"pwd", ""},
	))
	wrong := fantasy.ToolCall{Name: "bash", Input: `{"command":"go version"}`}
	if resp, err := byName["bash"].Run(ctx, wrong); err != nil || !resp.IsError {
		t.Fatalf("wrong scalar constrained input must be denied: response=%+v err=%v", resp, err)
	}
	if bash.ran {
		t.Fatal("mismatched scalar input must not reach the underlying tool")
	}

	ctx = tools.SetToolsAllowed(context.Background(), []string{"bash", "submit_result"})
	ctx = context.WithValue(ctx, taskToolSequenceKey{}, newTaskToolSequence(
		[]string{"bash", "submit_result"},
		nil,
		"command",
		[]string{"pwd", ""},
	))
	matching := fantasy.ToolCall{Name: "bash", Input: `{"command":"pwd"}`}
	if resp, err := byName["bash"].Run(ctx, matching); err != nil || resp.IsError {
		t.Fatalf("matching scalar constrained input must run: response=%+v err=%v", resp, err)
	}
}

// TestPolicyGateInputViolationReportsFieldMismatch covers the diagnostic
// detail added to a closed-sequence input violation. Before this, the
// message named only the sequence position ("input violation at position 1
// of 2"), giving a coordinator or worker no way to tell a field mismatch
// from a typo'd tool name — its only recovery was an early-terminal
// submit_result that discarded the whole attempt. The field-level expected
// vs. actual summary lets it correct the very next call instead.
func TestPolicyGateInputViolationReportsFieldMismatch(t *testing.T) {
	c := gateTestCoordinator()
	bash := &recordingTool{name: "bash"}
	result := &recordingTool{name: "submit_result"}
	gated := c.gatePolicyTools([]fantasy.AgentTool{bash, result})
	byName := map[string]fantasy.AgentTool{}
	for _, tool := range gated {
		byName[tool.Info().Name] = tool
	}

	ctx := tools.SetToolsAllowed(context.Background(), []string{"bash", "submit_result"})
	ctx = context.WithValue(ctx, taskToolSequenceKey{}, newTaskToolSequence(
		[]string{"bash", "submit_result"},
		[]map[string]any{{"command": "pwd"}, {}},
		"",
		nil,
	))
	wrong := fantasy.ToolCall{Name: "bash", Input: `{"command":"go version"}`}
	resp, err := byName["bash"].Run(ctx, wrong)
	if err != nil || !resp.IsError {
		t.Fatalf("wrong constrained input must be denied: response=%+v err=%v", resp, err)
	}
	for _, want := range []string{`field "command"`, `expected "pwd"`, `got "go version"`} {
		if !strings.Contains(resp.Content, want) {
			t.Fatalf("input violation message %q missing %q", resp.Content, want)
		}
	}
}

// TestPolicyGateInputViolationRedactsSecretFields ensures the new
// field-level diagnostic never leaks a pinned credential: a mismatch on a
// secret-shaped field name must name the field but mask both values,
// reusing the same key-name redaction rule the rest of the runtime already
// trusts (utils.RedactJSON) rather than a bespoke check local to this gate.
func TestPolicyGateInputViolationRedactsSecretFields(t *testing.T) {
	c := gateTestCoordinator()
	bash := &recordingTool{name: "bash"}
	result := &recordingTool{name: "submit_result"}
	gated := c.gatePolicyTools([]fantasy.AgentTool{bash, result})
	byName := map[string]fantasy.AgentTool{}
	for _, tool := range gated {
		byName[tool.Info().Name] = tool
	}

	ctx := tools.SetToolsAllowed(context.Background(), []string{"bash", "submit_result"})
	ctx = context.WithValue(ctx, taskToolSequenceKey{}, newTaskToolSequence(
		[]string{"bash", "submit_result"},
		[]map[string]any{{"password": "s3cr3t-expected-value"}, {}},
		"",
		nil,
	))
	wrong := fantasy.ToolCall{Name: "bash", Input: `{"password":"s3cr3t-actual-value"}`}
	resp, err := byName["bash"].Run(ctx, wrong)
	if err != nil || !resp.IsError {
		t.Fatalf("wrong constrained input must be denied: response=%+v err=%v", resp, err)
	}
	if !strings.Contains(resp.Content, `field "password"`) {
		t.Fatalf("input violation message %q should still name the mismatched field", resp.Content)
	}
	if strings.Contains(resp.Content, "s3cr3t-expected-value") || strings.Contains(resp.Content, "s3cr3t-actual-value") {
		t.Fatalf("input violation message must not leak a secret-shaped field's value: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "[REDACTED]") {
		t.Fatalf("input violation message should mark the secret-shaped field as redacted: %q", resp.Content)
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
	ctx = context.WithValue(ctx, taskToolSequenceKey{}, newTaskToolSequence([]string{"bash", "bash", "bash", "submit_result"}, nil, "", nil))

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

func TestPolicyGateFailureOnlyAllowsEarlyTerminalResult(t *testing.T) {
	c := gateTestCoordinator()
	bash := &recordingTool{name: "bash", resp: fantasy.NewTextErrorResponse("command failed\nExit code: 1")}
	write := &recordingTool{name: "write"}
	result := &recordingTool{name: "submit_result"}
	gated := c.gatePolicyTools([]fantasy.AgentTool{bash, write, result})
	byName := map[string]fantasy.AgentTool{}
	for _, tool := range gated {
		byName[tool.Info().Name] = tool
	}

	ctx := tools.SetToolsAllowed(context.Background(), []string{"bash", "write", "submit_result"})
	ctx = context.WithValue(ctx, taskToolSequenceKey{}, newTaskToolSequence([]string{"bash", "write", "submit_result"}, nil, "", nil))
	if resp, err := byName["bash"].Run(ctx, fantasy.ToolCall{Name: "bash"}); err != nil || !resp.IsError {
		t.Fatalf("failed command should return an error response: response=%+v err=%v", resp, err)
	}

	if resp, err := byName["write"].Run(ctx, fantasy.ToolCall{Name: "write"}); err != nil || !resp.IsError {
		t.Fatalf("repair after failed command must be rejected: response=%+v err=%v", resp, err)
	}
	if write.ran {
		t.Fatal("write must not execute after the sequence has failed")
	}

	failedCall := fantasy.ToolCall{Name: "submit_result", Input: `{"status":"failed","summary":"command failed"}`}
	if resp, err := byName["submit_result"].Run(ctx, failedCall); err != nil || resp.IsError {
		t.Fatalf("early failed submit_result must be admitted: response=%+v err=%v", resp, err)
	}
	if !result.ran {
		t.Fatal("failed submit_result should execute")
	}
}

func TestPolicyGateExpectedExitCodeAllowsClosedSequenceToContinue(t *testing.T) {
	c := gateTestCoordinator()
	// timeout intentionally ends the observation window with exit 124. Its
	// output remains useful evidence, so the closed sequence must advance to
	// the evidence-preserving ls and final result rather than forcing failed.
	bash := &recordingTool{name: "bash", resp: fantasy.NewTextErrorResponse("TUI menu captured\nExit code: 124")}
	ls := &recordingTool{name: "ls"}
	result := &recordingTool{name: "submit_result"}
	gated := c.gatePolicyTools([]fantasy.AgentTool{bash, ls, result})
	byName := map[string]fantasy.AgentTool{}
	for _, tool := range gated {
		byName[tool.Info().Name] = tool
	}

	ctx := tools.SetToolsAllowed(context.Background(), []string{"bash", "ls", "submit_result"})
	ctx = context.WithValue(ctx, taskToolSequenceKey{}, newTaskToolSequence(
		[]string{"bash", "ls", "submit_result"}, nil, "", nil, [][]int{{124, 137}, {}, {}},
	))
	resp, err := byName["bash"].Run(ctx, fantasy.ToolCall{Name: "bash"})
	if err != nil || resp.IsError {
		t.Fatalf("declared expected exit must be a normal evidence result: response=%+v err=%v", resp, err)
	}
	if !strings.Contains(resp.Content, "Exit code: 124") {
		t.Fatalf("expected result must preserve observation evidence: %q", resp.Content)
	}
	if resp, err := byName["ls"].Run(ctx, fantasy.ToolCall{Name: "ls"}); err != nil || resp.IsError {
		t.Fatalf("next closed-sequence tool must be admitted: response=%+v err=%v", resp, err)
	}
	if resp, err := byName["submit_result"].Run(ctx, fantasy.ToolCall{Name: "submit_result", Input: `{"status":"success","summary":"observation recorded"}`}); err != nil || resp.IsError {
		t.Fatalf("successful terminal result must be admitted: response=%+v err=%v", resp, err)
	}
	if !bash.ran || !ls.ran || !result.ran {
		t.Fatalf("expected all declared tools to run: bash=%t ls=%t result=%t", bash.ran, ls.ran, result.ran)
	}
}

func TestPolicyGateExpectedPredicateExitAllowsNextBashSlot(t *testing.T) {
	c := gateTestCoordinator()
	// A negative predicate commonly exits 1. When the closed contract declares
	// that code as an observation, it must remain visible as evidence and must
	// not prevent the next required probe from running.
	first := &recordingTool{name: "bash", resp: fantasy.NewTextErrorResponse("predicate is false\nExit code: 1")}
	second := &recordingTool{name: "bash"}
	result := &recordingTool{name: "submit_result"}
	gated := c.gatePolicyTools([]fantasy.AgentTool{first, second, result})

	ctx := tools.SetToolsAllowed(context.Background(), []string{"bash", "submit_result"})
	ctx = context.WithValue(ctx, taskToolSequenceKey{}, newTaskToolSequence(
		[]string{"bash", "bash", "submit_result"}, nil, "", nil, [][]int{{1}, {}, {}},
	))

	resp, err := gated[0].Run(ctx, fantasy.ToolCall{Name: "bash"})
	if err != nil || resp.IsError || !strings.Contains(resp.Content, "Exit code: 1") {
		t.Fatalf("expected predicate exit must remain normal evidence: response=%+v err=%v", resp, err)
	}
	if resp, err := gated[1].Run(ctx, fantasy.ToolCall{Name: "bash"}); err != nil || resp.IsError {
		t.Fatalf("next required bash slot must be admitted: response=%+v err=%v", resp, err)
	}
	if resp, err := gated[2].Run(ctx, fantasy.ToolCall{Name: "submit_result", Input: `{"status":"success","summary":"probes completed"}`}); err != nil || resp.IsError {
		t.Fatalf("terminal result must be admitted: response=%+v err=%v", resp, err)
	}
	if first.calls != 1 || second.calls != 1 || result.calls != 1 {
		t.Fatalf("expected each declared call once: first=%d second=%d result=%d", first.calls, second.calls, result.calls)
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
