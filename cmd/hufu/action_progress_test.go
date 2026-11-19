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

func TestValidationProgressLifecycleAndDisplay(t *testing.T) {
	timer := newIdleWarningTimer(&lineWriter{}, time.Hour)
	defer timer.stop()
	for _, id := range []string{"outer", "inner"} {
		for _, phase := range []string{"started", "progress"} {
			event := team.StatusEvent{Type: "runtime_validation", Message: "Runtime validation: result_recovery / runtime_inputs", Data: map[string]any{"validation_id": id, "phase": phase, "scope": "result_recovery", "check": "runtime_inputs"}}
			timer.ObserveValidation(event)
			w := &testStatusWriter{}
			dispatchStatusEvent(w, &reporterState{}, event)
			if !timer.validations[id] || !strings.Contains(w.b.String(), "Runtime validation:") {
				t.Fatalf("validation progress lost: id=%s status=%s", id, w.b.String())
			}
			identity := validationStatusIdentity(event)
			if identity.ID != id || identity.Phase != phase || identity.Scope != "result_recovery" || identity.Check != "runtime_inputs" {
				t.Fatalf("JSONL validation identity lost: %+v", identity)
			}
		}
	}
	timer.ObserveValidation(team.StatusEvent{Type: "runtime_validation", Data: map[string]any{"validation_id": "inner", "phase": "completed"}})
	if len(timer.validations) != 1 || !timer.validations["outer"] {
		t.Fatal("nested validation completion cleared its caller")
	}
	timer.ObserveValidation(team.StatusEvent{Type: "runtime_validation", Data: map[string]any{"validation_id": "outer", "phase": "completed"}})
	if len(timer.validations) != 0 {
		t.Fatal("validation remained active after completion")
	}
}
