package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/teatest/v2"
	inspectpkg "github.com/kjelly/hufu/internal/inspect"
)

func TestResultsPickScopeAndFullOutput(t *testing.T) {
	workspace, _ := resultsTestFixture(t)
	for _, tasks := range []bool{false, true} {
		deps := resultsTestDeps(t)
		deps.pick = func(_ context.Context, _ io.Reader, _ io.Writer, rows []inspectpkg.HistoricalResult, query string) (int, error) {
			if query != "逾時" {
				t.Fatalf("initial query = %q", query)
			}
			wantCount, wantKind := 1, "run_response"
			if tasks {
				wantCount, wantKind = 3, "task"
			}
			if len(rows) != wantCount {
				t.Fatalf("candidates = %#v", rows)
			}
			for _, row := range rows {
				if row.Kind != wantKind || row.Team != "review" || row.OriginalPrompt != "請調查原始連線逾時問題" || len(row.Branches) == 0 {
					t.Fatalf("candidate lost identity/scope: %#v", row)
				}
			}
			return len(rows) - 1, nil
		}
		args := []string{"pick", "逾時", "--team", "review", "--workspace", workspace, "--all-branches"}
		if tasks {
			args = append(args, "--tasks", "--agent", "helper")
		}
		_, output, err := executeResultsTest(t, deps, args...)
		if err != nil {
			t.Fatal(err)
		}
		if tasks {
			if !strings.Contains(output, "保存的分析與解法 1") || strings.Contains(output, "保存的分析與解法 2") {
				t.Fatalf("wrong task selected: %s", output)
			}
		} else if output != "coordinator final response\n" {
			t.Fatalf("not the saved final body: %q", output)
		}
		value, output, err := executeResultsTest(t, deps, append(args, "--output", "json")...)
		if err != nil || len(value.Results) != 1 || strings.Contains(output, "\x1b") {
			t.Fatalf("JSON framing/selection: %v, %s", err, output)
		}
	}
}

func TestResultsPickCancellationErrorsAndNonTTY(t *testing.T) {
	workspace, _ := resultsTestFixture(t)
	for _, test := range []struct {
		index   int
		err     error
		wantErr bool
	}{
		{-1, nil, false}, {99, nil, true}, {-2, nil, true}, {-1, errors.New("picker failed"), true},
	} {
		deps := resultsTestDeps(t)
		deps.pick = func(context.Context, io.Reader, io.Writer, []inspectpkg.HistoricalResult, string) (int, error) {
			return test.index, test.err
		}
		_, output, err := executeResultsTest(t, deps, "pick", "--team", "review", "--workspace", workspace)
		if (err != nil) != test.wantErr || output != "" {
			t.Fatalf("cancel/error produced output: %v, %q", err, output)
		}
	}
	for _, flags := range [][]string{{"--cursor", "x"}, {"--limit", "1"}, {"--agent", "helper"}, {"--status", "done"}, {"--display-mode", "plain"}} {
		deps := resultsTestDeps(t)
		deps.pick = func(context.Context, io.Reader, io.Writer, []inspectpkg.HistoricalResult, string) (int, error) {
			t.Fatal("invalid options opened picker")
			return 0, nil
		}
		if _, _, err := executeResultsTest(t, deps, append([]string{"pick", "--workspace", workspace}, flags...)...); err == nil {
			t.Fatalf("accepted invalid flags: %v", flags)
		}
	}
	_, _, err := executeResultsTest(t, resultsTestDeps(t), "pick", "--workspace", workspace, "--team", "review")
	if err == nil || !strings.Contains(err.Error(), "requires terminal stdin and stderr") {
		t.Fatalf("non-TTY did not fail promptly: %v", err)
	}
}

func TestResultsPickDoesNotPaginateCandidates(t *testing.T) {
	workspace, _ := resultsTestFixture(t)
	deps := resultsTestDeps(t)
	load := deps.load
	deps.load = func(ctx context.Context, scope inspectpkg.HistoricalResultScope) (inspectpkg.HistoricalSnapshot, error) {
		snapshot, err := load(ctx, scope)
		if err != nil {
			return snapshot, err
		}
		row := snapshot.Results[0]
		row.Kind = "run_response"
		for range 30 {
			snapshot.Results = append(snapshot.Results, row)
		}
		return snapshot, nil
	}
	deps.pick = func(_ context.Context, _ io.Reader, _ io.Writer, rows []inspectpkg.HistoricalResult, _ string) (int, error) {
		if len(rows) != 31 {
			t.Fatalf("picker silently limited results: %d", len(rows))
		}
		return -1, nil
	}
	if _, _, err := executeResultsTest(t, deps, "pick", "--workspace", workspace, "--team", "review"); err != nil {
		t.Fatal(err)
	}
}

func TestResultsPickerUnicodeFilteringNavigationAndEmpty(t *testing.T) {
	rows := []inspectpkg.HistoricalResult{
		{ResultID: "first", OriginalPrompt: "調查葉元之助理事件", Summary: "警方證據"},
		{ResultID: "second", OriginalPrompt: "timeout deadline", Agent: "verifier", Branches: []string{"experiment"}},
		{ResultID: "third", OriginalPrompt: "調查葉元之最新報告"},
	}
	m := newResultsPickerModel(rows, "葉助")
	if len(m.matches) != 1 || m.matches[0] != 0 {
		t.Fatalf("Chinese subsequence failed: %v", m.matches)
	}
	m = newResultsPickerModel(rows, "TMOUT experiment VERIFIER")
	if len(m.matches) != 1 || m.matches[0] != 1 {
		t.Fatalf("multi-term case-folded matching failed: %v", m.matches)
	}
	m = newResultsPickerModel(rows, "")
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = next.(resultsPickerModel)
	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if next.(resultsPickerModel).selected != 1 || cmd == nil {
		t.Fatal("navigation selected wrong source row")
	}
	m = newResultsPickerModel(rows, "不存在")
	next, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil || next.(resultsPickerModel).selected != -1 || !strings.Contains(m.View().Content, "No matching results") {
		t.Fatal("empty filter selected a hidden row")
	}
	for _, key := range []tea.KeyPressMsg{{Code: tea.KeyEscape}, {Code: 'c', Mod: tea.ModCtrl}} {
		next, cmd = m.Update(key)
		if cmd == nil || next.(resultsPickerModel).selected != -1 {
			t.Fatal("cancel selected a row")
		}
	}
	for _, size := range []tea.WindowSizeMsg{{Width: 1, Height: 1}, {Width: 36, Height: 12}} {
		next, _ = m.Update(size)
		view := next.(resultsPickerModel).View()
		if len(strings.Split(view.Content, "\n")) > size.Height {
			t.Fatal("view exceeds terminal height")
		}
		for line := range strings.SplitSeq(view.Content, "\n") {
			if ansi.StringWidth(line) > size.Width {
				t.Fatal("view exceeds terminal width")
			}
		}
	}
}

func TestResultsPickerProgramTypingAndSelection(t *testing.T) {
	m := newResultsPickerModel([]inspectpkg.HistoricalResult{{OriginalPrompt: "另一任務"}, {OriginalPrompt: "中文搜尋目標"}}, "")
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
	tm.Send(tea.KeyPressMsg{Code: '搜', Text: "搜"})
	tm.Send(tea.KeyPressMsg{Code: '尋', Text: "尋"})
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	final := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(resultsPickerModel)
	if final.selected != 1 || final.input.Value() != "搜尋" {
		t.Fatalf("typing selected wrong result: %#v", final)
	}
}
