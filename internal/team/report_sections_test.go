package team

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRunReportSectionsUseVerifiedOutputsAndSurviveSourceChanges(t *testing.T) {
	c := newTestCoordinator(t)
	c.session.Dir = t.TempDir()
	writeReportFixture(t, c.session.Dir, "team.yaml", `
name: warehouse
execution-report:
  sections:
    - id: stock
      title: Stock Levels
      source: &stock {task: inventory-collector, output: stock, require: passed-task-output-assertion}
      renderer: {runtime: golang, source: ./reports/stock, mode: trusted-static, timeout: 5}
    - id: summary
      title: Count Summary
      source: *stock
      renderer:
        runtime: template
        template: 'Checked {{.Output.location}} for {{len .Output.names}} items.'
    - id: unverified
      title: Unverified
      source: {task: inventory-collector, output: notes, require: passed-task-output-assertion}
      renderer: {template: '{{.Output}}'}
    - id: bad-json
      title: Invalid JSON
      source: *stock
      renderer: {runtime: golang, source: ./reports/bad, mode: trusted-static}
    - id: timeout
      title: Slow Output
      source: *stock
      renderer: {runtime: golang, source: ./reports/slow, mode: trusted-static, timeout: 1}
    - id: source-drift
      title: Changed Source
      source: *stock
      renderer: {runtime: golang, source: ./reports/changed, mode: trusted-static}
`)
	const stockFile = "reports/stock/main.go"
	const changedFile = "reports/changed/main.go"
	const sortedRenderer = `package main
import ("context"; "encoding/json"; "errors"; "io"; "os"; "sort"; "strings")
func Run(_ context.Context, in io.Reader, out io.Writer) error {
	var input struct {
		Version int
		SectionID string ` + "`json:\"section_id\"`" + `
		Output struct { Names []string; Notes string }
	}
	if err := json.NewDecoder(in).Decode(&input); err != nil { return err }
	if input.Version != 1 || input.SectionID != "stock" || input.Output.Notes != "" ||
		os.Getenv("HUFU_REPORT_TEST_SENTINEL") != "" ||
		os.Getenv("HOME") != "" || os.Getenv("HUFU_REPOSITORY") != "" {
		return errors.New("unexpected report input or environment")
	}
	sort.Strings(input.Output.Names)
	return json.NewEncoder(out).Encode(map[string]string{"markdown": "| Item |\n| --- |\n| " + strings.Join(input.Output.Names, " |\n| ") + " |"})
}`
	writeReportFixture(t, c.session.Dir, stockFile, sortedRenderer)
	writeReportFixture(t, c.session.Dir, changedFile, sortedRenderer)
	writeReportFixture(t, c.session.Dir, "reports/bad/main.go", `package main
import ("context"; "io")
func Run(_ context.Context, _ io.Reader, out io.Writer) error { _, err := io.WriteString(out, "[]"); return err }`)
	writeReportFixture(t, c.session.Dir, "reports/slow/main.go", `package main
import ("context"; "io")
func Run(_ context.Context, _ io.Reader, _ io.Writer) error { for {} }`)
	t.Setenv("HUFU_REPORT_TEST_SENTINEL", "parent-only")
	config, err := parseTeamYML(c.session.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.session.Config = config
	c.SetSessionData(NewSession())
	ctx, endRun := c.beginInvocationExecutionRun(t.Context())
	t.Cleanup(endRun)
	if err := c.resolveRunInputsForInvocation(ctx, "Count stock."); err != nil {
		t.Fatal(err)
	}
	writeReportFixture(t, c.session.Dir, changedFile, sortedRenderer+"\n// post-start change\n")
	verification := recordReportStockEvidence(t, ctx, c)
	result := c.finalizeRunPrepared(ctx, &RunResult{Outcome: RunOutcomePartial, Response: "Inventory counted."}, &AcceptanceResult{State: AcceptancePassed})
	if !c.TerminalLifecycleConfirmed() || result.Outcome != RunOutcomePartial || result.Acceptance.EffectiveState() != AcceptancePassed {
		t.Fatalf("optional rendering changed terminal outcome: %+v", result)
	}
	saved := loadTerminalReport(t, c)
	if len(saved.Sections) != 6 {
		t.Fatalf("section count = %d, want 6", len(saved.Sections))
	}
	stock, summary := saved.Sections[0], saved.Sections[1]
	if stock.Markdown != "| Item |\n| --- |\n| bolts |\n| nuts |" || stock.RendererDigest == "" ||
		stock.Provenance == nil || stock.Provenance.RunID != result.RunID ||
		stock.Provenance.OccurrenceRevision != 1 || stock.Provenance.ActionInvocationID != "warehouse-count" ||
		stock.Provenance.RuntimeOutputsHash == "" || stock.Provenance.RunInputSnapshotHash == "" ||
		stock.Provenance.VerificationFingerprint != verification.Fingerprint {
		t.Fatalf("stock section = %+v, provenance = %+v", stock, stock.Provenance)
	}
	if summary.Markdown != "Checked north for 2 items." || summary.Provenance == nil || *summary.Provenance != *stock.Provenance {
		t.Fatalf("summary section = %+v", summary)
	}
	wantReasons := []string{reportSectionSourceMissing, "invalid or oversized JSON", "timed out", "changed or became unavailable"}
	for i, section := range saved.Sections[2:] {
		if section.Markdown != "" || !strings.Contains(section.UnavailableReason, wantReasons[i]) {
			t.Fatalf("unavailable section = %+v, want reason containing %q", section, wantReasons[i])
		}
	}
	writeReportFixture(t, c.session.Dir, stockFile, sortedRenderer+"\n// post-run change\n")
	assertSameReportSections(t, saved, loadTerminalReport(t, c))
	if err := os.Remove(filepath.Join(c.session.Dir, stockFile)); err != nil {
		t.Fatal(err)
	}
	assertSameReportSections(t, saved, loadTerminalReport(t, c))
}

func recordReportStockEvidence(t *testing.T, ctx context.Context, c *Coordinator) *VerificationResult {
	t.Helper()
	snapshot := c.RunInputSnapshot()
	if snapshot == nil {
		t.Fatal("no frozen inventory inputs")
	}
	outputs, digest, err := CanonicalizeRuntimeOutputs(map[string]any{
		"stock": map[string]any{"location": "north", "names": []any{"nuts", "bolts"}},
		"notes": "This output is outside the assertion.",
	})
	if err != nil {
		t.Fatal(err)
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		PlanTaskID: "inventory-collector", ContractID: "inventory-collector", Agent: "runtime", Desc: "Count stock",
		RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
	}})[0]
	item.Status = TaskDone
	item.OccurrenceRevision = 1
	item.TypedResult = &TaskResult{
		TaskID: item.ID, Attempt: 1, Status: TaskResultStatusSuccess, Summary: "counted", Source: "runtime",
		RuntimeOutputs: outputs, RuntimeOutputsHash: digest,
		RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
	}
	item.ExecutionReceipt = &ExecutionReceipt{
		RunID: c.executionRunID, TaskID: item.ID, Attempt: 1, StartedAt: time.Now(), FinishedAt: time.Now(), ExitCode: new(0),
		ProducerID: "runtime", ModelExecutionID: "warehouse-model", TranscriptRef: "sha256-transcript",
		ActionInvocationID: "warehouse-count", RuntimeOutputsHash: digest,
		RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
	}
	item.ExecutionReceipts = []ExecutionReceipt{*item.ExecutionReceipt}
	c.taskTracker.TodoList().Restore([]*TodoItem{item})
	spec := VerificationSpec{
		Type: VerifyTaskOutputAssert, WorksetSourceTask: "inventory-collector", TaskOutputName: "stock",
		Assertions: []JSONAssertion{{Pointer: "/location", Op: "equals", Value: "north"}},
	}
	verification, err := c.executeTaskOutputAssertVerification(ctx, spec)
	if err != nil || !isVerifySuccess(verification) {
		t.Fatalf("stock assertion = %+v, %v", verification, err)
	}
	item.ExecutionReceipt.VerifyResult = verification
	item.ExecutionReceipts = []ExecutionReceipt{*item.ExecutionReceipt}
	c.taskTracker.TodoList().Restore([]*TodoItem{item})
	appendReportEvent(t, c.eventStore, RunEvent{Type: "task_completed", TaskID: item.ID}, taskTransitionPayload(item))
	return verification
}

func writeReportFixture(t *testing.T, root, path, data string) {
	t.Helper()
	target := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadTerminalReport(t *testing.T, c *Coordinator) *RunReport {
	t.Helper()
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range slices.Backward(events) {
		if event.RunID == c.executionRunID && event.Type == string(EventRunFinished) {
			saved, err := LoadRunReport(t.Context(), c.session.Workspace, event.ID)
			if err != nil {
				t.Fatal(err)
			}
			return saved
		}
	}
	t.Fatal("run_finished event missing")
	return nil
}

func assertSameReportSections(t *testing.T, original, reopened *RunReport) {
	t.Helper()
	want, err := marshalRunReport(original)
	if err != nil {
		t.Fatal(err)
	}
	got, err := marshalRunReport(reopened)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, got) {
		t.Fatal("reopened report differed after a renderer source change")
	}
}
