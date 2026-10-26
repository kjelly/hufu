package team

import (
	"bytes"
	"encoding/json/v2"
	"testing"
)

func TestTerminalRunInputsRequireCurrentSelection(t *testing.T) {
	for _, mode := range []string{"current", "stale", "recovery"} {
		t.Run(mode, func(t *testing.T) {
			c := newTestCoordinator(t)
			c.sessionData = NewSession()
			origin := c.executionRunID
			if mode != "current" {
				origin = "original-run"
			}
			snapshot, err := ResolveRunInputSnapshot([]RunInputDefinition{{Name: "count", Schema: RunInputSchema{Type: "integer"}, Required: true}},
				[]RunInputAssignment{{Name: "count", RawValue: []byte("3"), Source: RunInputSourceCLI}}, origin, origin+":invocation", c.session.Config.Name)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(snapshot)
			c.sessionData.RunInputSnapshots = []RunInputSnapshot{*snapshot}
			c.sessionData.ActiveRunInputSnapshotID = snapshot.ID
			if mode == "recovery" {
				if err := c.selectRecoveryRunInputSnapshot(snapshot); err != nil {
					t.Fatal(err)
				}
			}
			result := &RunResult{RunID: c.executionRunID, Outcome: RunOutcomeFailed}
			c.prepareTerminalResult(result)
			if (result.RunInputs == nil) != (mode == "stale") {
				t.Fatalf("terminal inputs lost their admission boundary: %#v", result.RunInputs)
			}
			if mode == "recovery" && result.RunInputOriginRunID != origin {
				t.Fatal("recovery inputs lost their original run")
			}
			payload, _ := json.Marshal(terminalLifecyclePayload(c, result))
			event := RunEvent{Type: "run_finished", RunID: c.executionRunID, Payload: payload}
			if err := validateRunFinishedEventPayload(event); err != nil {
				t.Fatal(err)
			}
			source := RunEvent{Type: string(EventRunInputsResolved), RunID: origin, Payload: before}
			if err := validateTerminalInputOrigins([]RunEvent{source, event}); err != nil {
				t.Fatal(err)
			}
			if mode == "recovery" {
				if err := validateTerminalInputOrigins([]RunEvent{event}); err == nil {
					t.Fatal("accepted a recovery input without its source")
				}
				wrongSource := source
				wrongSource.RunID = "different-run"
				if err := validateTerminalInputOrigins([]RunEvent{wrongSource, event}); err == nil {
					t.Fatal("accepted an input source attributed to a different run")
				}
				replayed := ReduceToSessionData([]RunEvent{source, event})
				if replayed.RunResult == nil || replayed.RunResult.RunInputOriginRunID != origin || replayed.RunResult.RunInputs.SnapshotHash != snapshot.SnapshotHash {
					t.Fatal("terminal replay lost the exact recovery input binding")
				}
				result.RunInputOriginRunID = "different-run"
				event.Payload, _ = json.Marshal(terminalLifecyclePayload(c, result))
				if err := validateRunFinishedEventPayload(event); err == nil {
					t.Fatal("accepted a mismatched recovery input origin")
				}
			}
			after, _ := json.Marshal(c.RunInputSnapshot())
			if !bytes.Equal(before, after) {
				t.Fatal("terminal preparation rewrote the frozen input snapshot")
			}
		})
	}
}

func TestTerminalInputOriginsAcceptLegacyEvents(t *testing.T) {
	for _, payload := range [][]byte{nil, []byte(`{}`), []byte(`{"outcome":"failed"}`)} {
		if err := validateTerminalInputOrigins([]RunEvent{{Type: "run_finished", Payload: payload}}); err != nil {
			t.Fatalf("legacy terminal event rejected: %v", err)
		}
	}
}
