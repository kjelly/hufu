package improve

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/team"
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

// AnalyzeRecent and LatestTeam must report a typed *AnalyticsError when the
// event store they derive execution events from cannot be trusted, tagged
// with the load_execution boundary. A missing store is no data, not an error.
func TestEventStoreLoadFailuresSurfaceLoadExecutionStage(t *testing.T) {
	unreadable := func(t *testing.T, workspace string) {
		// A directory in place of the journal database cannot be opened.
		if err := os.MkdirAll(runtimestore.Path(workspace), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tampered := func(t *testing.T, workspace string) {
		writeExecutionEvents(t, workspace, []team.ExecutionEvent{
			{Timestamp: "2026-07-12T10:00:00Z", RunID: "run-1", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 1, Status: "done", Model: "secret-model"},
		})
		// The payload is hashed; editing it must break the chain.
		tamperEventJournal(t, workspace, `UPDATE events SET payload = CAST(replace(CAST(payload AS TEXT), 'secret-model', 'secret-modex') AS BLOB) WHERE CAST(payload AS TEXT) LIKE '%secret-model%'`)
	}
	analyze := func(workspace string) error {
		_, err := AnalyzeRecent(workspace, "dev", workspace, 1)
		return err
	}
	latest := func(workspace string) error {
		_, err := LatestTeam(workspace)
		return err
	}
	tests := []struct {
		name    string
		prepare func(*testing.T, string)
		load    func(string) error
	}{
		{name: "AnalyzeRecent with an unreadable store", prepare: unreadable, load: analyze},
		{name: "LatestTeam with an unreadable store", prepare: unreadable, load: latest},
		{name: "AnalyzeRecent with a tampered chain", prepare: tampered, load: analyze},
		{name: "LatestTeam with a tampered chain", prepare: tampered, load: latest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			tt.prepare(t, workspace)
			err := tt.load(workspace)
			var analyticsErr *AnalyticsError
			if !errors.As(err, &analyticsErr) {
				t.Fatalf("error = %v (%T), want *AnalyticsError", err, err)
			}
			if analyticsErr.Stage != AnalyticsStageLoadExecution {
				t.Fatalf("Stage = %q, want %q", analyticsErr.Stage, AnalyticsStageLoadExecution)
			}
			// §18: the error names the failing operation, never payload content.
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error message leaks payload content: %q", err.Error())
			}
		})
	}
}

func TestMissingEventStoreIsNoExecutionData(t *testing.T) {
	if _, err := LatestTeam(t.TempDir()); !errors.Is(err, ErrNoExecutionData) {
		t.Fatalf("LatestTeam error = %v, want ErrNoExecutionData", err)
	}
}
