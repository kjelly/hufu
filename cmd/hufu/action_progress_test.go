package main

import (
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

func TestIdleWarningTracksActionLifecycle(t *testing.T) {
	timer := newIdleWarningTimer(&lineWriter{}, time.Hour)
	defer timer.stop()
	for _, kind := range []string{"action_started", "action_progress"} {
		timer.ObserveAction(team.StatusEvent{Type: kind, Data: map[string]any{"action_id": "action-1", "action_type": "deploy"}})
		if timer.actions["action-1"] != "deploy" {
			t.Fatal("action was shown as waiting for a model")
		}
	}
	timer.ObserveAction(team.StatusEvent{Type: "action_failed", Data: map[string]any{"action_id": "action-1"}})
	if len(timer.actions) != 0 {
		t.Fatal("terminal action remained active")
	}
}

func TestDispatchActionProgressShowsStage(t *testing.T) {
	w := &testStatusWriter{}
	dispatchStatusEvent(w, &reporterState{}, team.StatusEvent{Type: "action_progress", Agent: "executor", Message: "deploy: resource sample captured"})
	if !strings.Contains(w.b.String(), "deploy: resource sample captured") {
		t.Fatalf("status = %q", w.b.String())
	}
}
