package team

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"charm.land/fantasy"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/tools"
)

func defaultContextArtifactPolicy() ExecutionContextArtifactPolicySnapshot {
	return ExecutionContextArtifactPolicySnapshot{MinBytes: 51200, PreviewBytes: 8192, MaxArtifactBytes: 1 << 20, MaxReadBytes: 32768, MaxArtifactsPerAttempt: 32}
}

// rebuildThroughView reads a published artifact back in byte ranges with the
// production view tool and opener.
func rebuildThroughView(t *testing.T, c *Coordinator, ctx context.Context, id string) (string, int) {
	t.Helper()
	view := tools.NewViewTool(tools.WithArtifactOpener(c.openArtifactRef))
	header := regexp.MustCompile(`^<artifact:[^ ]+ bytes=\[\d+,\d+\) total=\d+>\n`)
	var rebuilt strings.Builder
	reads := 0
	for offset := 0; offset >= 0; reads++ {
		result, err := view.Run(ctx, fantasy.ToolCall{ID: fmt.Sprintf("view-%d", reads), Name: "view", Input: fmt.Sprintf(`{"artifact_ref":%q,"byte_offset":%d}`, id, offset)})
		if err != nil || result.IsError {
			t.Fatalf("view at %d: %q %v", offset, result.Content, err)
		}
		body := strings.TrimPrefix(result.Content, header.FindString(result.Content))
		rebuilt.WriteString(body[:strings.LastIndex(body, "\n</artifact:")])
		offset = -1
		if next := strings.LastIndex(result.Content, "[next byte_offset="); next >= 0 {
			offset, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(result.Content[next:], "[next byte_offset="), "]"))
		}
	}
	return rebuilt.String(), reads
}

func TestBashOutputOffloadEndToEnd(t *testing.T) {
	const privateKey = "-----BEGIN RSA PRIVATE KEY-----\nMIIEoffloadsecret\n-----END RSA PRIVATE KEY-----"
	f := newContextArtifactFixture(t, defaultContextArtifactPolicy())
	svc := f.c.newContextArtifactService(f.task.ID, TaskDef{})
	ctx := f.attemptCtx(t, svc, f.task.ID, 1)
	bash := tools.NewBashTool()

	// seq 1 20000 is about 108 KB, past 20,000 estimated tokens at bytes/4.
	command := `printf '%s\n' "` + strings.ReplaceAll(privateKey, "\n", `\n`) + `"; seq 1 20000; exit 4`
	callCtx := svc.withToolOffloader(ctx, "bash", "call-bash")
	response, err := bash.Run(callCtx, fantasy.ToolCall{ID: "call-bash", Name: "bash", Input: fmt.Sprintf(`{"command":%q}`, command)})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(response.Content, "\n")
	if !response.IsError || !strings.HasPrefix(lines[0], "[context artifact] artifact_ref=") || lines[len(lines)-1] != "Exit code: 4" {
		t.Fatalf("preview framing: first=%q last=%q isError=%v", lines[0], lines[len(lines)-1], response.IsError)
	}
	if exitCode, ok := transcriptExitCode("bash", response.Content); !ok || exitCode != 4 {
		t.Fatalf("transcriptExitCode = %d, %v", exitCode, ok)
	}
	if len(response.Content) > 8192+512 {
		t.Fatalf("preview is %d bytes", len(response.Content))
	}
	id := contextArtifactRefPattern.FindStringSubmatch(response.Content)[1]
	stored, reads := rebuildThroughView(t, f.c, ctx, id)
	if !strings.HasSuffix(stored, "\n19999\n20000\n\nExit code: 4") || !strings.Contains(stored, "[REDACTED]") || strings.Contains(stored, "MIIEoffloadsecret") {
		t.Fatalf("stored output is not the complete redacted result (%d bytes)", len(stored))
	}
	for _, event := range contextArtifactEvents(t, f.c) {
		if strings.Contains(string(event.Payload), "MIIEoffloadsecret") || strings.Contains(string(event.Payload), "19999") {
			t.Fatalf("event payload carries output: %s", event.Payload)
		}
	}
	t.Logf("original=%d bytes preview=%d bytes view reads=%d rebuilt=%d bytes", len(stored), len(response.Content), reads, len(stored))

	// The reference header survives step squeeze and compaction evidence.
	if squeezed := compactToolResultText(response.Content, 750); !strings.Contains(squeezed, "artifact_ref="+id) {
		t.Fatalf("squeeze dropped the reference: %q", squeezed)
	}
	evidence := contextstore.NormalizeToolResultEvidence(contextstore.ToolResultEvidence{Tool: "bash", Stdout: response.Content})
	if !strings.Contains(evidence.StdoutHead, "artifact_ref="+id) {
		t.Fatalf("compaction head dropped the reference: %q", evidence.StdoutHead)
	}

	// Without an offloader the same command returns the legacy bounded text.
	legacy, err := bash.Run(ctx, fantasy.ToolCall{ID: "call-legacy", Name: "bash", Input: fmt.Sprintf(`{"command":%q}`, command)})
	if err != nil || !strings.HasPrefix(legacy.Content, "[output truncated by hufu:") || strings.Contains(legacy.Content, "artifact_ref=") {
		t.Fatalf("legacy response=%q err=%v", legacy.Content[:min(len(legacy.Content), 120)], err)
	}

	// A result above max-artifact-bytes honestly stays on the legacy path.
	huge, err := bash.Run(svc.withToolOffloader(ctx, "bash", "call-huge"), fantasy.ToolCall{ID: "call-huge", Name: "bash", Input: `{"command":"seq 1 300000"}`})
	if err != nil || !strings.HasPrefix(huge.Content, "[output truncated by hufu:") || strings.Contains(huge.Content, "artifact_ref=") {
		t.Fatalf("oversized response=%q err=%v", huge.Content[:min(len(huge.Content), 120)], err)
	}
}

type offloadProbeTool struct {
	name    string
	content string
	ran     int
}

func (p *offloadProbeTool) Info() fantasy.ToolInfo                   { return fantasy.ToolInfo{Name: p.name} }
func (p *offloadProbeTool) ProviderOptions() fantasy.ProviderOptions { return nil }
func (p *offloadProbeTool) SetProviderOptions(fantasy.ProviderOptions) {
}
func (p *offloadProbeTool) Run(ctx context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	p.ran++
	if response, ok := tools.OffloadToolOutput(ctx, tools.ToolOutputCapture{ToolName: p.name, Content: p.content}); ok {
		return response, nil
	}
	return fantasy.NewTextResponse("legacy"), nil
}

func TestPolicyGateBindsOffloaderOnlyAfterAuthorization(t *testing.T) {
	c := newDirectTypedCoordinator(t, "bash,view,grep", nil, nil)
	store, err := NewEventStore(c.session.Workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	policy := defaultContextArtifactPolicy()
	c.eventStore, c.executionRunID = store, "run-1"
	c.executionPolicy = &executionPolicyState{snapshot: &ExecutionPolicySnapshot{ContextArtifacts: &policy}}
	todoID := resolverTodoID(t, c, "gate-offload")
	svc := c.newContextArtifactService(todoID, TaskDef{})
	content := strings.Repeat("gate output line\n", 5000)

	baseCtx := func(allowed ...string) context.Context {
		ctx := gatewayTestContext(t, c, allowed...)
		ctx = context.WithValue(ctx, todoIDKey{}, todoID)
		ctx = context.WithValue(ctx, executionAttemptKey{}, 1)
		return svc.install(ctx, ResolvedWorkerTools{Names: []string{"bash", "view", "grep"}, AuthorizedNames: []string{"bash", "view", "grep"}})
	}
	cases := []struct {
		name        string
		tool        string
		allowed     []string
		wantRan     int
		wantOffload bool
	}{
		{name: "authorized bash", tool: "bash", allowed: []string{"bash", "view"}, wantRan: 1, wantOffload: true},
		{name: "denied bash never runs", tool: "bash", allowed: []string{"view"}, wantRan: 0},
		{name: "ineligible tool", tool: "grep", allowed: []string{"grep", "view"}, wantRan: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := &offloadProbeTool{name: tc.tool, content: content}
			gate := &policyGatedTool{inner: probe, coordinator: c}
			response, err := gate.Run(baseCtx(tc.allowed...), fantasy.ToolCall{ID: "call-" + tc.tool + strconv.Itoa(len(tc.allowed)), Name: tc.tool, Input: `{"command":"true"}`})
			if err != nil {
				t.Fatal(err)
			}
			if probe.ran != tc.wantRan || strings.Contains(response.Content, "artifact_ref=") != tc.wantOffload {
				t.Fatalf("ran=%d response=%q", probe.ran, response.Content[:min(len(response.Content), 80)])
			}
		})
	}
}

func TestDynamicGatewayOutputOffload(t *testing.T) {
	c := newDirectTypedCoordinator(t, "view", nil, nil)
	store, err := NewEventStore(c.session.Workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	policy := defaultContextArtifactPolicy()
	c.eventStore, c.executionRunID = store, "run-1"
	c.executionPolicy = &executionPolicyState{snapshot: &ExecutionPolicySnapshot{ContextArtifacts: &policy}}
	todoID := resolverTodoID(t, c, "gateway-offload")
	svc := c.newContextArtifactService(todoID, TaskDef{})
	target := gatewayTestTarget()
	valid := `{"action":"call","target":"github__get_issue","arguments":{"owner":"kjelly","issue_number":1,"labels":[],"metadata":{"open":true}}}`

	for _, tc := range []struct {
		name  string
		bytes int
	}{{name: "100 KB", bytes: 100_000}, {name: "above the 256 KiB gateway bound", bytes: 300_000}} {
		t.Run(tc.name, func(t *testing.T) {
			text := strings.Repeat("issue body line\n", tc.bytes/16)
			gateway := newDynamicToolGateway(c, &gatewayTestExecutor{text: text}, []DynamicToolTarget{target})
			accumulator := newDynamicInvocationAccumulator(4)
			ctx := gatewayTestContext(t, c, dynamicToolGatewayName, target.Name, "view")
			ctx = context.WithValue(ctx, todoIDKey{}, todoID)
			ctx = context.WithValue(ctx, executionAttemptKey{}, 1)
			ctx = svc.install(ctx, ResolvedWorkerTools{Names: []string{dynamicToolGatewayName, "view"}, AuthorizedNames: []string{dynamicToolGatewayName, "view"}})
			ctx = withDynamicToolInvocationSink(ctx, accumulator.record, nil)
			callID := "gateway-" + strconv.Itoa(tc.bytes)
			response, err := gateway.Run(svc.withToolOffloader(ctx, dynamicToolGatewayName, callID), fantasy.ToolCall{ID: callID, Input: valid})
			if err != nil || response.IsError || !strings.HasPrefix(response.Content, "[context artifact] artifact_ref=") || len(response.Content) > 8192+512 {
				t.Fatalf("gateway preview=%q err=%v", response.Content[:min(len(response.Content), 120)], err)
			}
			receipts, _ := accumulator.snapshot()
			if len(receipts) != 1 || !receipts[0].Truncated || receipts[0].OriginalBytes != len(text) {
				t.Fatalf("receipts=%#v", receipts)
			}
			id := contextArtifactRefPattern.FindStringSubmatch(response.Content)[1]
			if rebuilt, _ := rebuildThroughView(t, c, ctx, id); rebuilt != text {
				t.Fatal("gateway artifact does not hold the complete result")
			}
		})
	}

	disabled := newDynamicToolGateway(c, &gatewayTestExecutor{text: strings.Repeat("x", maxDynamicGatewayOutputBytes+10)}, []DynamicToolTarget{target})
	response, err := disabled.Run(gatewayTestContext(t, c, dynamicToolGatewayName, target.Name), fantasy.ToolCall{ID: "legacy", Input: valid})
	if err != nil || !strings.Contains(response.Content, "truncated by hufu") || strings.Contains(response.Content, "artifact_ref=") {
		t.Fatalf("legacy gateway response tail=%q err=%v", response.Content[max(0, len(response.Content)-80):], err)
	}
}
