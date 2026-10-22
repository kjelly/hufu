package main

import (
	"bytes"
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/teatest/v2"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/operator"
	tuipkg "github.com/kjelly/hufu/internal/tui"
)

func TestOperatorObservationsCLIJSONAndTUIUseSameFixture(t *testing.T) {
	workspace, runID, _ := buildInspectCommandFixture(t)
	run := func(format string) string {
		t.Helper()
		command := newInspectCommand()
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&bytes.Buffer{})
		command.SetArgs([]string{"overview", "--run", runID, "--workspace", workspace, "--output", format})
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
		return output.String()
	}
	var envelope struct {
		Data inspectpkg.OverviewData `json:"data"`
	}
	if err := json.Unmarshal([]byte(run("json")), &envelope); err != nil {
		t.Fatal(err)
	}
	snapshot := envelope.Data.Snapshot
	economics, err := inspectpkg.InspectEconomics(t.Context(), inspectpkg.InspectQuery{Workspace: workspace, RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	data := economics.Data.(inspectpkg.EconomicsData)
	if snapshot.Observations == nil || snapshot.Observations.Status != "available" ||
		*snapshot.Observations.UsageCostMicros != *data.UsageCostMicros ||
		*snapshot.Observations.VerifiedTasks != data.VerifiedTaskCount || snapshot.Observations.SourceHead != data.SourceHead {
		t.Fatal("overview and source economics disagree")
	}
	text := run("text")
	shared := operator.RenderObservationSummary(snapshot.Observations)
	if !strings.Contains(text, shared) || strings.ContainsRune(text, '\x1b') {
		t.Fatalf("plain CLI did not use shared summary:\n%s", text)
	}
	var model tea.Model = tuipkg.NewWithOptions("task", tuipkg.TeamInfo{}, tuipkg.Options{
		Theme: tuipkg.ThemeMono, NoColor: true, DisplayPreset: tuipkg.DisplayEpaper,
	})
	for _, message := range []tea.Msg{
		tea.WindowSizeMsg{Width: 160, Height: 80},
		tuipkg.OperatorSnapshotMsg{Generation: 1, Snapshot: *snapshot},
		tea.KeyPressMsg{Code: 'L', Text: "L"}, tea.KeyPressMsg{Code: 'G', Text: "G"},
	} {
		model, _ = model.Update(message)
	}
	view := ansi.Strip(model.View().Content)
	for _, line := range strings.Split(strings.TrimSpace(shared), "\n") {
		if !strings.Contains(view, line) {
			t.Fatalf("TUI omitted shared line %q:\n%s", line, view)
		}
	}
	for _, secret := range []string{"private worker output", "sk-fixture-secret", "provider.example"} {
		if strings.Contains(text+view, secret) {
			t.Fatal("observation rendering exposed private content")
		}
	}
}

type snapshotReporterRecorder struct{ message tea.Msg }

func (m snapshotReporterRecorder) Init() tea.Cmd  { return nil }
func (m snapshotReporterRecorder) View() tea.View { return tea.NewView("") }
func (m snapshotReporterRecorder) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message.(type) {
	case tuipkg.OperatorSnapshotMsg, tuipkg.OperatorSnapshotUnavailableMsg:
		m.message = message
		return m, tea.Quit
	default:
		return m, nil
	}
}

func TestOperatorObservationsAsyncReporterSuccessAndFailure(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "available", true: "unavailable"}[unavailable], func(t *testing.T) {
			workspace := filepath.Join(t.TempDir(), "absent")
			if !unavailable {
				workspace, _, _ = buildInspectCommandFixture(t)
			}
			recorder := teatest.NewTestModel(t, snapshotReporterRecorder{}, teatest.WithInitialTermSize(80, 24))
			t.Cleanup(func() { _ = recorder.Quit() })
			refresh, stop := newTUISnapshotReporter(recorder.GetProgram(), workspace)
			t.Cleanup(stop)
			refresh()
			result := recorder.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(snapshotReporterRecorder)
			stop()
			switch message := result.message.(type) {
			case tuipkg.OperatorSnapshotMsg:
				if unavailable || message.Generation != 1 || message.Snapshot.Observations == nil {
					t.Fatalf("unexpected snapshot: %+v", message)
				}
			case tuipkg.OperatorSnapshotUnavailableMsg:
				if !unavailable || message.Generation != 1 {
					t.Fatalf("unexpected failure: %+v", message)
				}
			default:
				t.Fatalf("reporter did not deliver query result: %T", result.message)
			}
		})
	}
}
