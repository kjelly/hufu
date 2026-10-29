package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func derivedCLIFixture(t *testing.T) versionedCLIFixture {
	t.Helper()
	previous := opts
	t.Cleanup(func() { opts = previous })
	f := newVersionedCLIFixture(t)
	f.write(t, "source.txt", "parent source\n")
	if err := team.SaveSessionTree(f.control, team.NewSessionTree()); err != nil {
		t.Fatal(err)
	}
	es, err := team.NewEventStore(f.control, "parent-run", "derived-cli")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []team.RunEvent{
		{Type: string(team.EventRunStarted), Actor: "coordinator", Payload: []byte(`{"team":"dev"}`)},
		{Type: string(team.EventTaskCreated), Actor: "coordinator", TaskID: "7", Payload: []byte(`{"id":"7","agent":"worker","status":"error","side_effect":"none"}`)},
		{Type: string(team.EventWorkflowStateChanged), Actor: "runtime", Payload: []byte(`{"state":"FAILED"}`)},
	} {
		if _, err := es.AppendPersistedContext(t.Context(), event); err != nil {
			_ = es.Close()
			t.Fatal(err)
		}
	}
	if err := es.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, "workspace", "version", "snapshot", "--workspace", f.control); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestCLIDerivedForkCreatesAwaitingChild(t *testing.T) {
	f := derivedCLIFixture(t)
	out, err := runCLI(t, "session", "fork", "main", "--new-task", "--name", "investigation", "--workspace", f.control, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var fork sessionForkOutput
	if err := json.Unmarshal([]byte(out), &fork); err != nil || !fork.NewTask || fork.InitialState != "awaiting_new_request" || fork.MetadataOnly {
		t.Fatalf("derived fork output = %s (%v)", out, err)
	}
	child := team.LoadSession(f.control)
	if child == nil || child.DerivedFork == nil || !child.DerivedFork.AwaitingNewRequest || child.WorkflowTaskIDFloor != 7 || len(child.Tasks) != 0 {
		t.Fatal("CLI child inherited parent work")
	}
	if _, err := runCLI(t, "resume", "--workspace", f.control); err == nil {
		t.Fatal("resume admitted an awaiting derived child")
	}
}

func TestCLIDerivedForkRefusesUnsupportedOptionsAndUnsafeSources(t *testing.T) {
	for _, failure := range []string{"metadata_only", "observe", "off", "unknown_effect"} {
		t.Run(failure, func(t *testing.T) {
			f := derivedCLIFixture(t)
			args := []string{"session", "fork", "main", "--new-task", "--name", "investigation", "--workspace", f.control}
			switch failure {
			case "metadata_only":
				args = append(args, "--metadata-only")
			case "observe", "off":
				if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".config", "hufu", "hufu.yaml"), []byte("workspace-versioning:\n  mode: "+failure+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unknown_effect":
				es, err := team.OpenEventStore(f.control)
				if err != nil {
					t.Fatal(err)
				}
				_, appendErr := es.AppendPersistedContext(t.Context(), team.RunEvent{Type: string(team.EventTaskCreated), RunID: "parent-run", TaskID: "8", Actor: "coordinator", Payload: []byte(`{"id":"8","agent":"worker","status":"error","side_effect":"unknown"}`)})
				if err := es.Close(); err != nil || appendErr != nil {
					t.Fatalf("fixture event: %v / %v", appendErr, err)
				}
			}
			before, err := team.LoadSessionTree(f.control)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runCLI(t, args...); err == nil {
				t.Fatal("unsupported or unsafe derived fork succeeded")
			}
			after, err := team.LoadSessionTree(f.control)
			if err != nil || !reflect.DeepEqual(before, after) || f.read(t, "source.txt") != "parent source\n" {
				t.Fatalf("rejected derived fork changed its source: %v", err)
			}
		})
	}
}
