package improve

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewAnalyticsErrorNilIsNil(t *testing.T) {
	if err := newAnalyticsError(AnalyticsStageOpen, nil); err != nil {
		t.Fatalf("newAnalyticsError(stage, nil) = %v, want nil", err)
	}
}

func TestAnalyticsErrorUnwrapsAndFormats(t *testing.T) {
	base := errors.New("boom")
	err := newAnalyticsError(AnalyticsStageLoadExecution, base)

	var analyticsErr *AnalyticsError
	if !errors.As(err, &analyticsErr) {
		t.Fatalf("errors.As failed to extract *AnalyticsError from %v (%T)", err, err)
	}
	if analyticsErr.Stage != AnalyticsStageLoadExecution {
		t.Fatalf("Stage = %q, want %q", analyticsErr.Stage, AnalyticsStageLoadExecution)
	}
	if !errors.Is(err, base) {
		t.Fatalf("errors.Is(err, base) = false, want true (Unwrap must expose the wrapped error)")
	}
	if got := err.Error(); !strings.HasPrefix(got, "load_execution: ") || !strings.Contains(got, "boom") {
		t.Fatalf("Error() = %q, want it to start with the stage and contain the wrapped message", got)
	}
}

// Every spec.md §18 stage name must exist verbatim so callers can rely on the
// literal string, plus load_memory (see sqlite_analytics_error.go doc
// comment for why it's an addition, not a spec deviation).
func TestAnalyticsErrorStagesMatchSpec(t *testing.T) {
	want := map[AnalyticsErrorStage]string{
		AnalyticsStageOpen:               "open",
		AnalyticsStageSchema:             "schema",
		AnalyticsStageLoadExecution:      "load_execution",
		AnalyticsStageLoadAudit:          "load_audit",
		AnalyticsStageLoadMemory:         "load_memory",
		AnalyticsStageSelectRuns:         "select_runs",
		AnalyticsStageAggregateExecution: "aggregate_execution",
		AnalyticsStageAggregateGroups:    "aggregate_groups",
		AnalyticsStageAttachContext:      "attach_context",
		AnalyticsStageAggregateMemory:    "aggregate_memory",
	}
	for stage, literal := range want {
		if string(stage) != literal {
			t.Fatalf("stage constant %v does not equal literal %q", stage, literal)
		}
	}
}

// AnalyzeRecent must surface a *AnalyticsError (not a bare error) once
// execution-event ingestion genuinely fails, tagged with the boundary that
// failed. A directory in place of the events file forces bufio.Scanner to
// hit a real read error (EISDIR) instead of the tolerated "file missing" /
// "malformed line" cases the loader otherwise swallows.
func TestAnalyzeRecentSurfacesAnalyticsErrorWithLoadExecutionStage(t *testing.T) {
	workspace := t.TempDir()
	teamDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory named execution-events.jsonl: os.Open succeeds, but
	// bufio.Scanner.Scan() fails to read it as a file.
	if err := os.MkdirAll(filepath.Join(workspace, eventsPath), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := AnalyzeRecent(workspace, "dev", teamDir, 1)
	if err == nil {
		t.Fatal("expected an error when execution-events.jsonl is unreadable")
	}
	var analyticsErr *AnalyticsError
	if !errors.As(err, &analyticsErr) {
		t.Fatalf("AnalyzeRecent error = %v (%T), want *AnalyticsError", err, err)
	}
	if analyticsErr.Stage != AnalyticsStageLoadExecution {
		t.Fatalf("Stage = %q, want %q", analyticsErr.Stage, AnalyticsStageLoadExecution)
	}
	// §18: the error must never leak payload/prompt/secret content — only
	// name the failing operation and path, both of which are non-sensitive
	// here (a workspace-relative telemetry path).
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error message unexpectedly contains sensitive-looking content: %q", err.Error())
	}
}

// LatestTeam shares the same load_execution boundary as AnalyzeRecent and
// must classify the same failure the same way.
func TestLatestTeamSurfacesAnalyticsErrorWithLoadExecutionStage(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, eventsPath), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := LatestTeam(workspace)
	if err == nil {
		t.Fatal("expected an error when execution-events.jsonl is unreadable")
	}
	var analyticsErr *AnalyticsError
	if !errors.As(err, &analyticsErr) {
		t.Fatalf("LatestTeam error = %v (%T), want *AnalyticsError", err, err)
	}
	if analyticsErr.Stage != AnalyticsStageLoadExecution {
		t.Fatalf("Stage = %q, want %q", analyticsErr.Stage, AnalyticsStageLoadExecution)
	}
}
