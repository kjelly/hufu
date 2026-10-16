package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/belief"
	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/team"
)

func TestOfflineShadowJournalMatchesPureChoicesAndReadOnlyInspect(t *testing.T) {
	for _, name := range []string{"pass-pass", "pass-fail", "fail-pass", "fail-fail"} {
		t.Run(name, func(t *testing.T) {
			workspace := filepath.Join(t.TempDir(), "shadow")
			f, err := journalFixture(name)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			args := []string{"--shadow-workspace", workspace, "--fixture", name}
			if err := run(t.Context(), args, bytes.NewReader(nil), &output, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			var view team.DiagnosticEpisodeState
			if err := json.Unmarshal(output.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if view.Source != "offline_fixture" || view.Mode != "shadow" || view.Validation != "verified" || view.State == nil || !view.State.Synthetic || len(view.Observations) != 2 || len(view.Decisions) != 3 || view.EventCount != 6 || view.ReplayDigest == "" {
				t.Fatalf("shadow=%+v", view)
			}
			for i, decision := range view.Decisions {
				expected, err := belief.RankDiagnostic(f.Model, f.Epoch, view.Observations[:min(i, len(view.Observations))], f.Policy)
				if err != nil || !reflect.DeepEqual(expected, decision.Choice) {
					t.Fatalf("choice[%d]=%+v expected=%+v error=%v", i, decision.Choice, expected, err)
				}
			}
			// Repeating the command uses the terminal journal, without another event.
			var repeated bytes.Buffer
			if err := run(t.Context(), args, bytes.NewReader(nil), &repeated, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(output.Bytes(), repeated.Bytes()) {
				t.Fatal("restart changed the shadow trace")
			}
			result, err := inspectpkg.InspectBeliefs(t.Context(), inspectpkg.InspectQuery{Workspace: workspace, RunID: shadowRunID, TaskID: "fixture-owner"})
			if err != nil {
				t.Fatal(err)
			}
			data := result.Data.(inspectpkg.BeliefsData)
			if len(data.Episodes) != 1 || !reflect.DeepEqual(data.Episodes[0], view) || len(data.Execution) != 0 {
				t.Fatalf("inspect=%+v", result)
			}
			store, err := team.OpenEventStoreReadOnly(workspace)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			events, err := store.ReadEvents()
			if err != nil || len(events) != 6 {
				t.Fatalf("events=%d error=%v", len(events), err)
			}
			for _, event := range events {
				if event.Type == "task_created" || event.Type == "task_completed" || event.Type == "action_started" {
					t.Fatal("shadow dispatched runtime work")
				}
			}
		})
	}
}

func TestOfflineShadowRejectsLiveWorkspaceAndInvalidOptions(t *testing.T) {
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "run-live", "session-live")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(team.RunEvent{Type: "task_created", Actor: "coordinator", TaskID: "1", Payload: []byte(`{"id":"1","agent":"worker","status":"pending"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := runFixtureShadow(t.Context(), workspace, "pass-pass"); err == nil {
		t.Fatal("shadow wrote into live workspace")
	}
	missing := filepath.Join(t.TempDir(), "missing")
	for _, args := range [][]string{
		{"--shadow-workspace", missing, "--evaluate"},
		{"--fixture", "pass-pass"},
		{"--shadow-workspace", missing, "--fixture", "../../elsewhere"},
		{"--shadow-workspace", missing, "--traces"},
	} {
		if err := run(t.Context(), args, bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted options %v", args)
		}
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("invalid fixture options created workspace")
	}
}
