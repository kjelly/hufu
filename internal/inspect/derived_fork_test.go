package inspect

import (
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

func TestWorkerHubExcludesParentAttemptsAfterDerivedInitialization(t *testing.T) {
	parent := []IndexedEvent{
		{Ordinal: 1, Event: team.RunEvent{ID: "parent-created", RunID: "parent", TaskID: "1", Type: string(team.EventTaskCreated)}},
		{Ordinal: 2, Event: team.RunEvent{ID: "parent-started", RunID: "parent", TaskID: "1", Type: string(team.EventTaskStarted), Payload: []byte(`{"agent":"worker","status":"in_progress","attempt":1,"dispatch_attempt":1}`)}},
	}
	if views := ProjectWorkerHub(nil, parent, nil, time.Now()); len(views.Attempts) != 1 {
		t.Fatal("fixture has no parent attempt")
	}
	events := append(parent, IndexedEvent{Ordinal: 3, Event: team.RunEvent{ID: "derived", Type: string(team.EventDerivedForkCreated)}})
	if views := ProjectWorkerHub(nil, events, nil, time.Now()); len(views.Attempts) != 0 {
		t.Fatal("pending child inherited a current worker attempt")
	}
	events = append(events,
		IndexedEvent{Ordinal: 4, Event: team.RunEvent{ID: "child-created", RunID: "child", TaskID: "2", Type: string(team.EventTaskCreated)}},
		IndexedEvent{Ordinal: 5, Event: team.RunEvent{ID: "child-started", RunID: "child", TaskID: "2", Type: string(team.EventTaskStarted), Payload: []byte(`{"agent":"worker","status":"in_progress","attempt":1,"dispatch_attempt":1}`)}},
	)
	views := ProjectWorkerHub(nil, events, nil, time.Now())
	if len(views.Attempts) != 1 || views.Attempts[0].RunID != "child" || views.Attempts[0].TaskID != "2" {
		t.Fatal("child worker hub includes historical parent work")
	}
}

func TestSessionProjectionGateStopsAtDerivedInitialization(t *testing.T) {
	terminal := team.RunEvent{ID: "parent-finished", RunID: "parent-run", Type: string(team.EventRunFinished)}
	selected := selectedRun{terminal: &terminal, terminalIndex: 0}
	lineage := Lineage{BranchID: "child", ActiveBranchID: "child", Events: []IndexedEvent{{Ordinal: 1, Event: terminal}}}
	if reason := sessionProjectionGate(lineage, selected); reason != "" {
		t.Fatalf("terminal projection unexpectedly unavailable: %s", reason)
	}
	lineage.Events = append(lineage.Events, IndexedEvent{Ordinal: 2, Event: team.RunEvent{ID: "derived-created", Type: string(team.EventDerivedForkCreated)}})
	if reason := sessionProjectionGate(lineage, selected); reason != ReasonProjectionNotRunScoped {
		t.Fatalf("parent report compared against child checkpoint: %s", reason)
	}
}
