package team

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func writeFanOutTSV(t *testing.T, dir, name string, lines []string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// characterizationWorksetItems is deliberately consumer-neutral.  WP-0 uses
// it to pin today's TSV fan-out semantics before artifact-backed worksets
// replace the path-based source contract.
var characterizationWorksetItems = []string{"alpha", "beta", "gamma"}

func writeCharacterizationWorkset(t *testing.T, workspace string, items []string) string {
	lines := []string{"item"}
	lines = append(lines, items...)
	return writeFanOutTSV(t, workspace, "inputs/items.tsv", lines)
}

func TestCharacterizationFanOutExpandsGenericWorkset(t *testing.T) {
	workspace := t.TempDir()
	writeCharacterizationWorkset(t, workspace, characterizationWorksetItems)
	c := &Coordinator{session: &TeamSession{Workspace: workspace}}

	expanded, err := c.expandFanOutTasks([]TaskDef{{
		Agent:  "worker",
		FanOut: &FanOutSpec{Source: "inputs/items.tsv", GoalTemplate: "process {item}"},
	}})
	if err != nil {
		t.Fatalf("expand fan-out: %v", err)
	}
	if len(expanded) != len(characterizationWorksetItems) {
		t.Fatalf("expanded %d tasks, want %d: %#v", len(expanded), len(characterizationWorksetItems), expanded)
	}
	for i, item := range characterizationWorksetItems {
		if got, want := expanded[i].Goal, "process "+item; got != want {
			t.Fatalf("expanded[%d].Goal = %q, want %q", i, got, want)
		}
	}
}

// This captures the pre-workset behavior: a TSV has neither a canonical item
// identity nor a completeness receipt. A missing row is expanded as supplied
// instead of being diagnosed by the runtime.
func TestCharacterizationFanOutPermitsMissingItem(t *testing.T) {
	workspace := t.TempDir()
	writeCharacterizationWorkset(t, workspace, []string{"alpha", "beta"})
	c := &Coordinator{session: &TeamSession{Workspace: workspace}}

	expanded, err := c.expandFanOutTasks([]TaskDef{{
		Agent:  "worker",
		FanOut: &FanOutSpec{Source: "inputs/items.tsv", GoalTemplate: "process {item}"},
	}})
	if err != nil {
		t.Fatalf("expand fan-out: %v", err)
	}
	got := []string{expanded[0].Goal, expanded[1].Goal}
	want := []string{"process alpha", "process beta"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("missing source row was not reproduced: got %v, want %v", got, want)
	}
}

func TestCharacterizationFanOutPermitsDuplicateItem(t *testing.T) {
	workspace := t.TempDir()
	writeCharacterizationWorkset(t, workspace, []string{"alpha", "beta", "beta"})
	c := &Coordinator{session: &TeamSession{Workspace: workspace}}

	expanded, err := c.expandFanOutTasks([]TaskDef{{
		Agent:  "worker",
		FanOut: &FanOutSpec{Source: "inputs/items.tsv", GoalTemplate: "process {item}"},
	}})
	if err != nil {
		t.Fatalf("expand fan-out: %v", err)
	}
	got := []string{expanded[0].Goal, expanded[1].Goal, expanded[2].Goal}
	want := []string{"process alpha", "process beta", "process beta"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("duplicate source row was not reproduced: got %v, want %v", got, want)
	}
}

// This captures the other legacy boundary: source path contents are read on
// every expansion. There is no durable manifest digest or receipt to bind a
// resumed/retried child set to the first observed source generation.
func TestCharacterizationFanOutReReadsChangedSource(t *testing.T) {
	workspace := t.TempDir()
	path := writeCharacterizationWorkset(t, workspace, characterizationWorksetItems)
	c := &Coordinator{session: &TeamSession{Workspace: workspace}}
	task := TaskDef{Agent: "worker", FanOut: &FanOutSpec{Source: "inputs/items.tsv", GoalTemplate: "process {item}"}}

	first, err := c.expandFanOutTasks([]TaskDef{task})
	if err != nil {
		t.Fatalf("first expansion: %v", err)
	}
	if err := os.WriteFile(path, []byte("item\nalpha\nbeta\ndelta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := c.expandFanOutTasks([]TaskDef{task})
	if err != nil {
		t.Fatalf("second expansion: %v", err)
	}
	if first[2].Goal != "process gamma" || second[2].Goal != "process delta" {
		t.Fatalf("expected source generation to change expansion: first=%#v second=%#v", first, second)
	}
}

// TestExpandFanOutTasks_SubstitutesEachRow proves the core value proposition:
// a coordinator writes the literal record_id/input_path values into the
// dispatch exactly zero times. The runtime reads them straight from the TSV
// row, so there is nothing for a long, repetitive generation to drop or
// merge a character in.
func TestExpandFanOutTasks_SubstitutesEachRow(t *testing.T) {
	workspace := t.TempDir()
	writeFanOutTSV(t, workspace, "inputs/records.tsv", []string{
		"#record_id\tinput_path",
		"row-alpha\tinputs/alpha.txt",
		"row-beta\tinputs/beta.txt",
	})
	c := &Coordinator{session: &TeamSession{Workspace: workspace}}

	tasks := []TaskDef{
		{Agent: "worker", Goal: "prepare inputs"},
		{
			Agent:       "worker",
			Goal:        "ignored",
			Constraints: "read-only",
			FanOut: &FanOutSpec{
				Source:       "inputs/records.tsv",
				GoalTemplate: "Process row {record_id}. Input: {input_path}.",
			},
		},
	}

	expanded, err := c.expandFanOutTasks(tasks)
	if err != nil {
		t.Fatalf("expandFanOutTasks failed: %v", err)
	}
	if len(expanded) != 3 {
		t.Fatalf("expanded = %d tasks, want 3 (1 passthrough + 2 fanned out): %#v", len(expanded), expanded)
	}
	if expanded[0].Goal != "prepare inputs" || expanded[0].Agent != "worker" || expanded[0].FanOut != nil {
		t.Fatalf("non-fan-out task was altered: %#v", expanded[0])
	}
	if expanded[1].Goal != "Process row row-alpha. Input: inputs/alpha.txt." {
		t.Fatalf("row 1 goal = %q", expanded[1].Goal)
	}
	if expanded[2].Goal != "Process row row-beta. Input: inputs/beta.txt." {
		t.Fatalf("row 2 goal = %q", expanded[2].Goal)
	}
	for i, want := range []struct{ agent, constraints string }{{"", ""}, {"worker", "read-only"}, {"worker", "read-only"}} {
		if i == 0 {
			continue
		}
		if expanded[i].Agent != want.agent || expanded[i].Constraints != want.constraints || expanded[i].FanOut != nil {
			t.Fatalf("expanded[%d] lost non-goal fields or kept FanOut: %#v", i, expanded[i])
		}
	}
}

func TestExpandFanOutTasks_NoFanOutTasksPassThroughUnchanged(t *testing.T) {
	c := &Coordinator{session: &TeamSession{Workspace: t.TempDir()}}
	tasks := []TaskDef{{Agent: "a", Goal: "g1"}, {Agent: "b", Goal: "g2"}}
	expanded, err := c.expandFanOutTasks(tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(expanded) != 2 || expanded[0].Goal != "g1" || expanded[1].Goal != "g2" {
		t.Fatalf("expanded = %#v, want unchanged passthrough", expanded)
	}
}

func TestExpandFanOutTasks_UnmatchedPlaceholderFailsWholeDispatch(t *testing.T) {
	workspace := t.TempDir()
	writeFanOutTSV(t, workspace, "inputs/records.tsv", []string{"record_id", "row-alpha"})
	c := &Coordinator{session: &TeamSession{Workspace: workspace}}

	tasks := []TaskDef{
		{Agent: "other", Goal: "unrelated task"},
		{Agent: "worker", Goal: "x", FanOut: &FanOutSpec{Source: "inputs/records.tsv", GoalTemplate: "Process {record_id} range {range_end}"}},
	}
	if _, err := c.expandFanOutTasks(tasks); err == nil || !strings.Contains(err.Error(), "range_end") {
		t.Fatalf("expected an error naming the unmatched placeholder, got %v", err)
	}
}

func TestExpandFanOutTasks_SourceEscapingWorkspaceIsRejected(t *testing.T) {
	c := &Coordinator{session: &TeamSession{Workspace: t.TempDir()}}
	for _, source := range []string{"../outside.tsv", "/etc/passwd", "..", "sub/../../escape.tsv"} {
		tasks := []TaskDef{{Agent: "a", Goal: "g", FanOut: &FanOutSpec{Source: source, GoalTemplate: "{x}"}}}
		if _, err := c.expandFanOutTasks(tasks); err == nil {
			t.Fatalf("source %q should have been rejected as escaping the workspace", source)
		}
	}
}

func TestExpandFanOutTasks_MissingSourceFileFailsClosed(t *testing.T) {
	c := &Coordinator{session: &TeamSession{Workspace: t.TempDir()}}
	tasks := []TaskDef{{Agent: "a", Goal: "g", FanOut: &FanOutSpec{Source: "does-not-exist.tsv", GoalTemplate: "{x}"}}}
	if _, err := c.expandFanOutTasks(tasks); err == nil {
		t.Fatal("expected an error for a missing fan_out source file")
	}
}

func TestExpandFanOutTasks_RowFieldCountMismatchFailsClosed(t *testing.T) {
	workspace := t.TempDir()
	writeFanOutTSV(t, workspace, "inputs/records.tsv", []string{
		"record_id\tinput_path",
		"row-alpha", // missing the second field
	})
	c := &Coordinator{session: &TeamSession{Workspace: workspace}}
	tasks := []TaskDef{{Agent: "a", Goal: "g", FanOut: &FanOutSpec{Source: "inputs/records.tsv", GoalTemplate: "{record_id}"}}}
	if _, err := c.expandFanOutTasks(tasks); err == nil || !strings.Contains(err.Error(), "row 1") {
		t.Fatalf("expected a row-1 field-count error, got %v", err)
	}
}

func TestExpandFanOutTasks_EmptySourceOrTemplateFailsClosed(t *testing.T) {
	c := &Coordinator{session: &TeamSession{Workspace: t.TempDir()}}
	if _, err := c.expandFanOutTasks([]TaskDef{{Agent: "a", Goal: "g", FanOut: &FanOutSpec{GoalTemplate: "{x}"}}}); err == nil {
		t.Fatal("expected an error for an empty source")
	}
	writeFanOutTSV(t, c.session.Workspace, "b.tsv", []string{"x", "1"})
	if _, err := c.expandFanOutTasks([]TaskDef{{Agent: "a", Goal: "g", FanOut: &FanOutSpec{Source: "b.tsv"}}}); err == nil {
		t.Fatal("expected an error for an empty goal_template")
	}
}

func TestExpandFanOutTasks_RowCountAboveLimitFailsClosed(t *testing.T) {
	workspace := t.TempDir()
	lines := make([]string, 0, maxFanOutRows+2)
	lines = append(lines, "id")
	for i := 0; i < maxFanOutRows+1; i++ {
		lines = append(lines, "row")
	}
	writeFanOutTSV(t, workspace, "huge.tsv", lines)
	c := &Coordinator{session: &TeamSession{Workspace: workspace}}
	tasks := []TaskDef{{Agent: "a", Goal: "g", FanOut: &FanOutSpec{Source: "huge.tsv", GoalTemplate: "{id}"}}}
	if _, err := c.expandFanOutTasks(tasks); err == nil || !strings.Contains(err.Error(), "fan-out limit") {
		t.Fatalf("expected a fan-out row limit error, got %v", err)
	}
}

// TestExecuteTasks_RejectsMalformedFanOutBeforeAnyTodoCreation proves the
// ExecuteTasks wiring: expansion runs before TODO creation for every task in
// the call, so one broken fan_out spec never leaves a partial TODO list
// behind from the rest of an otherwise-valid batch.
func TestExecuteTasks_RejectsMalformedFanOutBeforeAnyTodoCreation(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{})
	c.session.Workspace = t.TempDir()

	_, err := c.ExecuteTasks(context.Background(), []TaskDef{
		{Agent: "other", Goal: "unrelated task"},
		{Agent: "worker", Goal: "x", FanOut: &FanOutSpec{Source: "missing.tsv", GoalTemplate: "{record_id}"}},
	})
	if err == nil {
		t.Fatal("expected an error for a fan_out source that does not exist")
	}
	if got := len(c.taskTracker.TodoList().Items()); got != 0 {
		t.Fatalf("rejected fan_out dispatch created %d TODOs, want none", got)
	}
}

func TestExpandFanOutTasks_BlankLinesSkippedAndHeaderHashStripped(t *testing.T) {
	workspace := t.TempDir()
	writeFanOutTSV(t, workspace, "inputs/records.tsv", []string{
		"#record_id\tstate",
		"",
		"row-alpha\tpending",
		"",
		"row-beta\tpending",
	})
	c := &Coordinator{session: &TeamSession{Workspace: workspace}}
	tasks := []TaskDef{{Agent: "a", Goal: "g", FanOut: &FanOutSpec{Source: "inputs/records.tsv", GoalTemplate: "{record_id}:{state}"}}}
	expanded, err := c.expandFanOutTasks(tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(expanded) != 2 || expanded[0].Goal != "row-alpha:pending" || expanded[1].Goal != "row-beta:pending" {
		t.Fatalf("expanded = %#v", expanded)
	}
}
