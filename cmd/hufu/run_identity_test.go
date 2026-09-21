package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	hulog "github.com/kjelly/hufu/internal/log"
)

func TestEmitExecutionIdentityText(t *testing.T) {
	previous := opts
	defer func() { opts = previous }()
	opts.eventFormat = "text"
	opts.intent = "execute"

	var output bytes.Buffer
	emitExecutionIdentity(&output, "inv-text", []cliRunIdentity{
		{Team: "review", RunID: "run-first"},
		{Team: "review", RunID: "run-second"},
	})
	for _, want := range []string{
		"Invocation ID: inv-text",
		"Run ID: run-first (team: review)",
		"Run ID: run-second (team: review)",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("identity output missing %q: %q", want, output.String())
		}
	}
}

func TestEmitExecutionIdentityJSONL(t *testing.T) {
	previous := opts
	defer func() { opts = previous }()
	opts.eventFormat = "jsonl"
	opts.intent = "execute"

	var output bytes.Buffer
	emitExecutionIdentity(&output, "inv-jsonl", []cliRunIdentity{{Team: "review", RunID: "run-jsonl"}})
	decoder := json.NewDecoder(&output)
	var invocationEvent jsonStatusEvent
	if err := decoder.Decode(&invocationEvent); err != nil {
		t.Fatal(err)
	}
	if invocationEvent.Type != "invocation_identity" || invocationEvent.InvocationID != "inv-jsonl" || invocationEvent.Time == "" {
		t.Fatalf("invocation identity event = %#v", invocationEvent)
	}
	var event jsonStatusEvent
	if err := decoder.Decode(&event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "run_identity" || event.Team != "review" || event.InvocationID != "inv-jsonl" || event.RunID != "run-jsonl" || event.Time == "" {
		t.Fatalf("identity event = %#v", event)
	}
}

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	writes chan struct{}
}

func (w *synchronizedBuffer) Write(data []byte) (int, error) {
	w.mu.Lock()
	n, err := w.buffer.Write(data)
	w.mu.Unlock()
	w.writes <- struct{}{}
	return n, err
}

func (w *synchronizedBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String()
}

func TestInvocationIdentityHeartbeatEmitsOnlyOnStartAndTicks(t *testing.T) {
	previous := opts
	defer func() { opts = previous }()
	opts.eventFormat = "text"

	writer := &synchronizedBuffer{writes: make(chan struct{}, 2)}
	ticks := make(chan time.Time)
	stop := startInvocationIdentityHeartbeatWithTicks(writer, "inv-heartbeat", ticks, func() {})
	<-writer.writes
	ticks <- time.Now()
	<-writer.writes
	stop()

	if got := strings.Count(writer.String(), "Invocation ID: inv-heartbeat"); got != 2 {
		t.Fatalf("heartbeat identity count = %d, want start plus one tick; output=%q", got, writer.String())
	}
}

func TestInvocationIdentityHeartbeatDoesNotWriteThroughActiveTUI(t *testing.T) {
	previous := opts
	opts.eventFormat = "text"
	opts.outputFormat = "text"
	opts.quietMode = false
	deactivate := activateTUIProgram(new(tea.Program))
	t.Cleanup(func() {
		deactivate()
		opts = previous
		hulog.SetWriter(nil)
		syncLogState()
	})

	var identityOutput bytes.Buffer
	ticks := make(chan time.Time)
	stop := startInvocationIdentityHeartbeatWithTicks(&identityOutput, "inv-tui", ticks, func() {})
	ticks <- time.Now()
	stop()
	if identityOutput.Len() != 0 {
		t.Fatalf("heartbeat wrote through active TUI: %q", identityOutput.String())
	}

	var logOutput bytes.Buffer
	hulog.SetWriter(&logOutput)
	stderrLog("hidden")
	if logOutput.Len() != 0 {
		t.Fatalf("logger wrote through active TUI: %q", logOutput.String())
	}

	deactivate()
	emitInvocationIdentity(&identityOutput, "inv-visible")
	stderrLog("visible")
	if !strings.Contains(identityOutput.String(), "Invocation ID: inv-visible") {
		t.Fatalf("identity remained suppressed after TUI cleanup: %q", identityOutput.String())
	}
	if logOutput.String() != "visible" {
		t.Fatalf("logger state was not restored after TUI cleanup: %q", logOutput.String())
	}
}

func TestPrintResultJSONProjectsOneOrManyRunIDs(t *testing.T) {
	previous := opts
	defer func() { opts = previous }()
	opts.invocationID = "inv-result"

	render := func(t *testing.T, identities []cliRunIdentity) jsonRunOutput {
		t.Helper()
		oldStdout := os.Stdout
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = writer
		err = printResultJSONForInvocation("result", map[string]*teamContext{}, nil, nil, identities)
		_ = writer.Close()
		os.Stdout = oldStdout
		if err != nil {
			t.Fatal(err)
		}
		var out jsonRunOutput
		if err := json.NewDecoder(reader).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	many := render(t, []cliRunIdentity{
		{Team: "alpha", RunID: "run-a"},
		{Team: "alpha", RunID: "run-b"},
		{Team: "beta", RunID: "run-c"},
	})
	if many.RunID != "" || len(many.RunIDs) != 3 {
		t.Fatalf("multi-run identity projection = %#v", many)
	}
	if many.InvocationID != "inv-result" {
		t.Fatalf("invocation_id = %q, want inv-result", many.InvocationID)
	}
	oneTeam := render(t, []cliRunIdentity{{Team: "alpha", RunID: "run-first"}, {Team: "alpha", RunID: "run-latest"}})
	if oneTeam.RunID != "run-latest" || len(oneTeam.RunIDs) != 2 {
		t.Fatalf("single-team identity projection = %#v", oneTeam)
	}
}
