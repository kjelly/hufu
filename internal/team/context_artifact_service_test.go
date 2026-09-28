package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

var contextArtifactRefPattern = regexp.MustCompile(`artifact_ref=(ctxart-[0-9a-f]{40})`)

type failingEventJournal struct{ appends int }

func (j *failingEventJournal) Append(context.Context, RunEvent) (RunEvent, error) {
	j.appends++
	return RunEvent{}, errors.New("journal unavailable")
}
func (j *failingEventJournal) ReadEvents(context.Context) ([]RunEvent, error) { return nil, nil }
func (j *failingEventJournal) VerifyHashChain(context.Context) error           { return nil }

func testContextArtifactPolicy() ExecutionContextArtifactPolicySnapshot {
	return ExecutionContextArtifactPolicySnapshot{MinBytes: 4096, PreviewBytes: 1024, MaxArtifactBytes: 65536, MaxReadBytes: 2048, MaxArtifactsPerAttempt: 3}
}

type contextArtifactFixture struct {
	c         *Coordinator
	workspace string
	task      *TodoItem
	sibling   *TodoItem
}

func newContextArtifactFixture(t *testing.T, policy ExecutionContextArtifactPolicySnapshot) contextArtifactFixture {
	t.Helper()
	workspace := t.TempDir()
	tracker := NewTaskTracker()
	items := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "coder", Desc: "run"}, {Agent: "coder", Desc: "sibling"}})
	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c := &Coordinator{
		session: &TeamSession{Workspace: workspace}, projectDir: workspace,
		executionRunID: "run-1", taskTracker: tracker, eventStore: store,
		executionPolicy: &executionPolicyState{snapshot: &ExecutionPolicySnapshot{ContextArtifacts: &policy}},
	}
	return contextArtifactFixture{c: c, workspace: workspace, task: items[0], sibling: items[1]}
}

func (f contextArtifactFixture) attemptCtx(t *testing.T, svc *contextArtifactService, todoID string, attempt int) context.Context {
	t.Helper()
	ctx := context.WithValue(t.Context(), todoIDKey{}, todoID)
	ctx = context.WithValue(ctx, executionAttemptKey{}, attempt)
	ctx = context.WithValue(ctx, tools.AgentToolsAllowedKey, []string{"bash", "view"})
	return svc.install(ctx, ResolvedWorkerTools{Names: []string{"bash", "view"}, AuthorizedNames: []string{"bash", "view"}})
}

func offloadThrough(ctx context.Context, toolName, callID string, capture tools.ToolOutputCapture) (fantasy.ToolResponse, bool) {
	callCtx := contextArtifactServiceFromContext(ctx).withToolOffloader(ctx, toolName, callID)
	return tools.OffloadToolOutput(callCtx, capture)
}

func syntheticToolOutput(bytes int, exitCode int) string {
	var b strings.Builder
	for i := 0; b.Len() < bytes; i++ {
		fmt.Fprintf(&b, "line %05d marker-7c1f 漢字🙂\n", i)
	}
	fmt.Fprintf(&b, "Exit code: %d", exitCode)
	return b.String()
}

func contextArtifactEvents(t *testing.T, c *Coordinator) []RunEvent {
	t.Helper()
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var published []RunEvent
	for _, event := range events {
		if event.Type == string(EventContextArtifactPublished) {
			published = append(published, event)
		}
	}
	return published
}

func readWholeArtifact(t *testing.T, c *Coordinator, ctx context.Context, id string) (string, error) {
	t.Helper()
	reader, err := c.openArtifactRef(ctx, id)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	return string(data), err
}

func TestContextArtifactPublishAndScopedRead(t *testing.T) {
	f := newContextArtifactFixture(t, testContextArtifactPolicy())
	svc := f.c.newContextArtifactService(f.task.ID, TaskDef{})
	if svc == nil {
		t.Fatal("service was not created")
	}
	ctx := f.attemptCtx(t, svc, f.task.ID, 1)
	content := syntheticToolOutput(20000, 3)

	response, ok := offloadThrough(ctx, "bash", "call-1", tools.ToolOutputCapture{ToolName: "bash", Content: content, IsError: true})
	if !ok {
		t.Fatal("eligible result was not offloaded")
	}
	lines := strings.Split(response.Content, "\n")
	if !response.IsError || !strings.HasPrefix(lines[0], "[context artifact] artifact_ref=ctxart-") || lines[len(lines)-1] != "Exit code: 3" {
		t.Fatalf("preview framing wrong: first=%q last=%q isError=%v", lines[0], lines[len(lines)-1], response.IsError)
	}
	if len(lines[0]) > contextArtifactLineCap || len(lines[1]) > contextArtifactLineCap || len(response.Content) > 1024+512 {
		t.Fatalf("preview exceeds its bounds: %d bytes", len(response.Content))
	}
	id := contextArtifactRefPattern.FindStringSubmatch(response.Content)[1]
	t.Logf("original=%d preview=%d bytes", len(content), len(response.Content))

	events := contextArtifactEvents(t, f.c)
	if len(events) != 1 {
		t.Fatalf("published events = %d, want 1", len(events))
	}
	if raw := string(events[0].Payload); strings.Contains(raw, "marker-7c1f") || strings.Contains(raw, f.workspace) || !strings.Contains(raw, id) {
		t.Fatalf("event payload leaks content or path: %s", raw)
	}
	var payload contextArtifactPublishedPayload
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil || payload.Bytes != int64(len(content)) || payload.PreviewBytes != len(response.Content) || payload.Reason != "size" {
		t.Fatalf("payload = %+v err=%v", payload, err)
	}

	if got, err := readWholeArtifact(t, f.c, ctx, id); err != nil || got != content {
		t.Fatalf("stored bytes differ: err=%v", err)
	}
	view := tools.NewViewTool(tools.WithArtifactOpener(f.c.openArtifactRef))
	var rebuilt strings.Builder
	for offset := 0; offset >= 0; {
		result, err := view.Run(ctx, fantasy.ToolCall{ID: "view", Name: "view", Input: fmt.Sprintf(`{"artifact_ref":%q,"byte_offset":%d}`, id, offset)})
		if err != nil || result.IsError {
			t.Fatalf("view at %d: %q %v", offset, result.Content, err)
		}
		header := regexp.MustCompile(`^<artifact:[^ ]+ bytes=\[(\d+),(\d+)\) total=\d+>\n`).FindStringSubmatch(result.Content)
		body := strings.TrimPrefix(result.Content, header[0])
		rebuilt.WriteString(body[:strings.LastIndex(body, "\n</artifact:")])
		offset = -1
		if next := strings.LastIndex(result.Content, "[next byte_offset="); next >= 0 {
			if _, err := fmt.Sscanf(result.Content[next:], "[next byte_offset=%d]", &offset); err != nil {
				t.Fatalf("unparseable trailer: %q", result.Content[next:])
			}
		}
	}
	if rebuilt.String() != content {
		t.Fatal("view byte ranges did not rebuild the stored output")
	}

	// A retry of the same executeTask call keeps access; nothing else does.
	if _, err := readWholeArtifact(t, f.c, f.attemptCtx(t, svc, f.task.ID, 2), id); err != nil {
		t.Fatalf("attempt 2 of the same call lost access: %v", err)
	}
	for name, deniedCtx := range map[string]context.Context{
		"sibling task":  f.attemptCtx(t, svc, f.sibling.ID, 1),
		"other service": f.attemptCtx(t, f.c.newContextArtifactService(f.task.ID, TaskDef{}), f.task.ID, 1),
		"no service":    context.WithValue(t.Context(), todoIDKey{}, f.task.ID),
	} {
		if _, err := readWholeArtifact(t, f.c, deniedCtx, id); err == nil || !strings.Contains(err.Error(), "unknown or not authorized") {
			t.Fatalf("%s read error = %v", name, err)
		}
	}
	f.c.executionRunID = "run-2"
	if _, err := readWholeArtifact(t, f.c, ctx, id); err == nil {
		t.Fatal("a new invocation read an earlier invocation's artifact")
	}
	f.c.executionRunID = "run-1"

	// Republishing the same capture is idempotent.
	again, ok := offloadThrough(ctx, "bash", "call-1", tools.ToolOutputCapture{ToolName: "bash", Content: content, IsError: true})
	if !ok || again.Content != response.Content || len(contextArtifactEvents(t, f.c)) != 1 || svc.counts[1] != 1 {
		t.Fatalf("republish was not idempotent: ok=%v events=%d count=%d", ok, len(contextArtifactEvents(t, f.c)), svc.counts[1])
	}

	// Integrity and availability errors never name a filesystem path.
	dataPath := filepath.Join(f.workspace, logsDir, "artifacts", "data", id)
	if err := os.WriteFile(dataPath, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readWholeArtifact(t, f.c, ctx, id); err == nil || !strings.Contains(err.Error(), "integrity verification") || strings.Contains(err.Error(), f.workspace) {
		t.Fatalf("corrupted read error = %v", err)
	}
	if err := os.Remove(dataPath); err != nil {
		t.Fatal(err)
	}
	if _, err := readWholeArtifact(t, f.c, ctx, id); err == nil || !strings.Contains(err.Error(), "unavailable") || strings.Contains(err.Error(), f.workspace) {
		t.Fatalf("missing read error = %v", err)
	}
	result, _ := view.Run(ctx, fantasy.ToolCall{ID: "view", Name: "view", Input: fmt.Sprintf(`{"artifact_ref":%q}`, id)})
	if !result.IsError || strings.Contains(result.Content, f.workspace) {
		t.Fatalf("line-based view leaked a path: %q", result.Content)
	}
}

func TestContextArtifactEligibility(t *testing.T) {
	policy := testContextArtifactPolicy()
	cases := []struct {
		name    string
		tool    string
		capture tools.ToolOutputCapture
		want    bool
	}{
		{name: "above min bytes", tool: "bash", capture: tools.ToolOutputCapture{ToolName: "bash", Content: syntheticToolOutput(5000, 0)}, want: true},
		{name: "gateway tool", tool: dynamicToolGatewayName, capture: tools.ToolOutputCapture{ToolName: dynamicToolGatewayName, Content: strings.Repeat("g", 5000)}, want: true},
		{name: "below min without legacy loss", tool: "bash", capture: tools.ToolOutputCapture{ToolName: "bash", Content: strings.Repeat("a", 3000)}, want: false},
		{name: "below min with legacy loss", tool: "bash", capture: tools.ToolOutputCapture{ToolName: "bash", Content: strings.Repeat("a\n", 1500), LegacyWouldTruncate: true}, want: true},
		{name: "not above preview", tool: "bash", capture: tools.ToolOutputCapture{ToolName: "bash", Content: strings.Repeat("a", 1024), LegacyWouldTruncate: true}, want: false},
		{name: "above artifact cap", tool: "bash", capture: tools.ToolOutputCapture{ToolName: "bash", Content: strings.Repeat("a", 65537)}, want: false},
		{name: "invalid utf-8", tool: "bash", capture: tools.ToolOutputCapture{ToolName: "bash", Content: strings.Repeat("a", 5000) + "\xff"}, want: false},
		{name: "capture from another tool", tool: "bash", capture: tools.ToolOutputCapture{ToolName: "sudo", Content: strings.Repeat("a", 5000)}, want: false},
		{name: "ineligible source tool", tool: "grep", capture: tools.ToolOutputCapture{ToolName: "grep", Content: strings.Repeat("a", 5000)}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newContextArtifactFixture(t, policy)
			svc := f.c.newContextArtifactService(f.task.ID, TaskDef{})
			_, ok := offloadThrough(f.attemptCtx(t, svc, f.task.ID, 1), tc.tool, "call-1", tc.capture)
			if ok != tc.want {
				t.Fatalf("offloaded = %v, want %v", ok, tc.want)
			}
		})
	}
}

func TestContextArtifactCountLimitIsPerAttempt(t *testing.T) {
	f := newContextArtifactFixture(t, testContextArtifactPolicy())
	svc := f.c.newContextArtifactService(f.task.ID, TaskDef{})
	content := syntheticToolOutput(5000, 0)
	for i := 1; i <= 4; i++ {
		_, ok := offloadThrough(f.attemptCtx(t, svc, f.task.ID, 1), "bash", fmt.Sprintf("call-%d", i), tools.ToolOutputCapture{ToolName: "bash", Content: content})
		if ok != (i <= 3) {
			t.Fatalf("publication %d offloaded = %v", i, ok)
		}
	}
	if _, ok := offloadThrough(f.attemptCtx(t, svc, f.task.ID, 2), "bash", "call-5", tools.ToolOutputCapture{ToolName: "bash", Content: content}); !ok {
		t.Fatal("a new attempt did not get its own publication budget")
	}
}

func TestContextArtifactFailuresFallBackWithoutReference(t *testing.T) {
	content := syntheticToolOutput(5000, 0)
	t.Run("store unavailable", func(t *testing.T) {
		f := newContextArtifactFixture(t, testContextArtifactPolicy())
		svc := f.c.newContextArtifactService(f.task.ID, TaskDef{})
		blocked := filepath.Join(f.workspace, "not-a-dir")
		if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		ctx := context.WithValue(f.attemptCtx(t, svc, f.task.ID, 1), artifactAccessScopeKey, &ArtifactAccessScope{StoreRoot: blocked, TaskID: f.task.ID})
		if _, ok := offloadThrough(ctx, "bash", "call-1", tools.ToolOutputCapture{ToolName: "bash", Content: content}); ok {
			t.Fatal("offload succeeded without a store")
		}
		if len(svc.refs) != 0 || len(contextArtifactEvents(t, f.c)) != 0 {
			t.Fatal("a failed store exposed a reference or event")
		}
	})
	t.Run("event append failure", func(t *testing.T) {
		f := newContextArtifactFixture(t, testContextArtifactPolicy())
		svc := f.c.newContextArtifactService(f.task.ID, TaskDef{})
		journal := &failingEventJournal{}
		f.c.SetEventJournal(journal)
		ctx := f.attemptCtx(t, svc, f.task.ID, 1)
		if _, ok := offloadThrough(ctx, "bash", "call-1", tools.ToolOutputCapture{ToolName: "bash", Content: content}); ok {
			t.Fatal("offload succeeded without a durable event")
		}
		if journal.appends != 1 || len(svc.refs) != 0 {
			t.Fatalf("appends=%d refs=%d", journal.appends, len(svc.refs))
		}
		sum := sha256.Sum256([]byte(content))
		orphan := contextArtifactID("run-1", f.task.ID, 1, "call-1", hex.EncodeToString(sum[:]))
		if _, err := readWholeArtifact(t, f.c, ctx, orphan); err == nil || !strings.Contains(err.Error(), "not authorized") {
			t.Fatalf("orphan artifact read error = %v", err)
		}
	})
}

func TestContextArtifactServiceCreationConditions(t *testing.T) {
	policy := testContextArtifactPolicy()
	f := newContextArtifactFixture(t, policy)
	if f.c.newContextArtifactService(f.task.ID, TaskDef{}) == nil {
		t.Fatal("baseline service was not created")
	}
	cases := []struct {
		name  string
		setup func(f contextArtifactFixture) (string, TaskDef)
	}{
		{name: "disabled policy", setup: func(f contextArtifactFixture) (string, TaskDef) {
			f.c.executionPolicy = &executionPolicyState{snapshot: &ExecutionPolicySnapshot{}}
			return f.task.ID, TaskDef{}
		}},
		{name: "coordinator turn", setup: func(contextArtifactFixture) (string, TaskDef) { return CoordTodoID, TaskDef{} }},
		{name: "no durable journal", setup: func(f contextArtifactFixture) (string, TaskDef) {
			f.c.eventStore = nil
			return f.task.ID, TaskDef{}
		}},
		{name: "no execution run", setup: func(f contextArtifactFixture) (string, TaskDef) {
			f.c.executionRunID = ""
			return f.task.ID, TaskDef{}
		}},
		{name: "closed tool sequence", setup: func(f contextArtifactFixture) (string, TaskDef) {
			return f.task.ID, TaskDef{Execution: ExecutionContract{ToolSequence: []string{"bash", "submit_result"}}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fresh := newContextArtifactFixture(t, policy)
			todoID, task := tc.setup(fresh)
			if svc := fresh.c.newContextArtifactService(todoID, task); svc != nil {
				t.Fatal("service was created")
			}
		})
	}

	svc := f.c.newContextArtifactService(f.task.ID, TaskDef{})
	ctx := context.WithValue(t.Context(), todoIDKey{}, f.task.ID)
	if contextArtifactServiceFromContext(svc.install(ctx, ResolvedWorkerTools{Names: []string{"bash"}, AuthorizedNames: []string{"bash"}})) != nil {
		t.Fatal("service installed on an attempt without view")
	}
	installed := f.attemptCtx(t, svc, f.task.ID, 1)
	if tools.ArtifactMaxReadBytes(installed) != policy.MaxReadBytes {
		t.Fatal("view read limit was not set")
	}
	if contextArtifactServiceFromContext(withoutContextArtifactService(installed)) != nil {
		t.Fatal("declared-step context still carries the service")
	}

	asserted := f.c.newContextArtifactService(f.task.ID, TaskDef{VerifySpec: &VerificationSpec{ToolCallAssertions: []agent.ToolCallAssertion{{Tool: "bash", ResultContains: "PASS"}}}})
	if _, ok := offloadThrough(f.attemptCtx(t, asserted, f.task.ID, 1), "bash", "call-1", tools.ToolOutputCapture{ToolName: "bash", Content: syntheticToolOutput(5000, 0)}); ok {
		t.Fatal("a result_contains-asserted tool was offloaded")
	}
}

func TestContextArtifactConcurrentPublications(t *testing.T) {
	policy := testContextArtifactPolicy()
	policy.MaxArtifactsPerAttempt = 32
	f := newContextArtifactFixture(t, policy)
	svc := f.c.newContextArtifactService(f.task.ID, TaskDef{})
	ctx := f.attemptCtx(t, svc, f.task.ID, 1)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := offloadThrough(ctx, "bash", fmt.Sprintf("call-%d", i), tools.ToolOutputCapture{ToolName: "bash", Content: syntheticToolOutput(5000+i, 0)}); !ok {
				t.Errorf("publication %d failed", i)
			}
		}()
	}
	wg.Wait()
	if len(svc.refs) != 8 || svc.counts[1] != 8 || len(contextArtifactEvents(t, f.c)) != 8 {
		t.Fatalf("refs=%d count=%d", len(svc.refs), svc.counts[1])
	}
}

func TestContextArtifactPreviewSplit(t *testing.T) {
	cases := []struct {
		name    string
		content string
		budget  int
	}{
		{name: "lines with footer", content: syntheticToolOutput(9000, 1), budget: 1024},
		{name: "no newline", content: strings.Repeat("字", 3000), budget: 1024},
		{name: "final line longer than tail", content: "head\n" + strings.Repeat("z", 5000), budget: 1024},
		{name: "trailing newline", content: strings.Repeat("row\n", 2000), budget: 1024},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			head, tail := contextArtifactPreview(tc.content, tc.budget)
			if len(head)+len(tail) > tc.budget || !utf8.ValidString(head) || !utf8.ValidString(tail) {
				t.Fatalf("head=%d tail=%d budget=%d", len(head), len(tail), tc.budget)
			}
			if !strings.HasPrefix(tc.content, head) || !strings.HasSuffix(tc.content, tail) || len(head)+len(tail) > len(tc.content) {
				t.Fatal("preview is not a head and tail of the content")
			}
			final := tc.content[strings.LastIndexByte(strings.TrimSuffix(tc.content, "\n"), '\n')+1:]
			if len(final) <= tc.budget-len(head) && !strings.HasSuffix(tail, final) {
				t.Fatalf("tail %q lost the final line %q", tail, final)
			}
		})
	}
}
