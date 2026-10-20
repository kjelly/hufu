package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
)

func observedSnapshot(run string) operator.OperatorSnapshot {
	return operator.OperatorSnapshot{
		Scope: operator.ResolvedScope{RunID: run, BranchID: "main"},
		Observations: &operator.ObservationSummary{
			Status: "available", SourceHead: cost.Freshness{EventID: "event-" + run, EventHash: "hash"},
			ComparisonScope: "single_run_observed", TaskCount: new(2), VerifiedTasks: new(1),
			UnavailableTasks: new(1), UsageCostMicros: new(int64(21)), KnownCostCoverage: new(0.5),
			ReasonCodes: []string{"unknown_cost"}, Routes: []operator.ObservedRoute{},
		},
	}
}

func TestOperatorObservationsResizeAndEpaper(t *testing.T) {
	for _, preset := range []DisplayPreset{DisplayDefault, DisplayEpaper} {
		m := NewWithOptions("task", TeamInfo{}, Options{Theme: ThemeMono, NoColor: true, DisplayPreset: preset})
		snapshot := observedSnapshot("current")
		updated, _ := m.Update(OperatorSnapshotMsg{Generation: 1, Snapshot: snapshot})
		m = updated.(Model)
		for _, width := range []int{100, 24, 80} {
			updated, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 20})
			m = updated.(Model)
			if !m.inOperator {
				updated, _ = m.Update(keyPress("L"))
				m = updated.(Model)
			}
			m.operatorVP.GotoBottom()
			view := ansi.Strip(m.operatorView())
			compact := strings.Join(strings.Fields(view), "")
			for _, value := range []string{"Verified:1", "Evidenceunavailable:1", "Usage subtotal (USD micros): 21", "unknown_cost"} {
				if !strings.Contains(compact, strings.ReplaceAll(value, " ", "")) {
					t.Fatalf("width=%d preset=%s missing %q:\n%s", width, preset, value, view)
				}
			}
			if hasColorSGR(m.operatorView()) {
				t.Fatal("no-color observations emitted color")
			}
		}
	}
}

func TestOperatorObservationsScopeFailureAndLateReplies(t *testing.T) {
	m := New("task", TeamInfo{})
	m.width, m.height = 100, 30
	m.tasks = []*team.TodoItem{{ID: "task", Status: team.TaskDone}}
	update := func(msg tea.Msg) {
		t.Helper()
		next, _ := m.Update(msg)
		m = next.(Model)
	}
	old := observedSnapshot("old")
	update(OperatorSnapshotMsg{Generation: 1, Snapshot: old})
	update(OperatorDetailsMsg{Generation: 1, Promotions: []OperatorPromotionDetail{{ID: "old"}}})
	update(OperatorSnapshotMsg{Generation: 2, Snapshot: observedSnapshot("new")})
	if len(m.operatorPromotions) != 0 || m.operatorSnapshot.Scope.RunID != "new" {
		t.Fatal("scope change retained old details")
	}
	update(OperatorSnapshotUnavailableMsg{Generation: 1})
	if !m.hasOperatorSummary {
		t.Fatal("old failure cleared new snapshot")
	}
	update(OperatorSnapshotUnavailableMsg{Generation: 3})
	update(OperatorSnapshotMsg{Generation: 2, Snapshot: old})
	update(OperatorDetailsMsg{Generation: 3, Promotions: []OperatorPromotionDetail{{ID: "late"}}})
	if m.hasOperatorSummary || m.operatorSnapshot.Observations != nil || len(m.operatorPromotions) != 0 ||
		m.tasks[0].Status != team.TaskDone || !strings.Contains(m.buildOperatorContent(), "Observations: unavailable") {
		t.Fatal("failed query retained observations or changed runtime state")
	}
	update(OperatorSnapshotMsg{Generation: 4, Snapshot: observedSnapshot("recovered")})
	if !m.hasOperatorSummary || m.operatorSnapshot.Scope.RunID != "recovered" {
		t.Fatal("new query did not recover presentation")
	}
}
