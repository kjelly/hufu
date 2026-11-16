package main

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

func TestCollectWorkspaceStatus(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure bool
		tie     bool
	}{{"complete", false, false}, {"partial", true, false}, {"tie", false, true}} {
		t.Run(test.name, func(t *testing.T) {
			workspaces := []workspacepkg.Workspace{
				{TeamName: "zeta", ControlRoot: "zeta", State: "active"},
				{TeamName: "alpha", ControlRoot: "alpha", State: "active"},
				{TeamName: "empty", ControlRoot: "empty", State: "active"},
			}
			data, err := collectWorkspaceStatus(t.Context(), workspacepkg.Project{}, workspaces, func(_ context.Context, path string) (*inspectpkg.LatestRun, error) {
				if path == "empty" {
					return nil, nil
				}
				if test.failure && path == "zeta" {
					return nil, inspectpkg.ErrIntegrity
				}
				started := time.Date(2026, 11, 16, 1, 0, 0, 0, time.UTC)
				if path == "zeta" && !test.tie {
					started = started.Add(time.Hour)
				}
				return &inspectpkg.LatestRun{StartedAt: started, Run: inspectpkg.RunData{RunID: path}}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if data.Selection != inspectpkg.LatestRunSelection || data.Complete == test.failure || len(data.Teams) != 3 || data.Teams[0].Team != "alpha" || data.Teams[1].State != "no_runs" {
				t.Fatalf("data = %+v", data)
			}
			if test.failure {
				if data.Latest != nil || data.Teams[2].Latest != nil || data.Teams[2].Error == nil {
					t.Fatalf("partial = %+v", data)
				}
			} else {
				want := "zeta"
				if test.tie {
					want = "alpha"
				}
				if data.Latest == nil || data.Latest.Team != want {
					t.Fatalf("latest = %+v, want %s", data.Latest, want)
				}
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := collectWorkspaceStatus(ctx, workspacepkg.Project{}, []workspacepkg.Workspace{{State: "active"}}, func(context.Context, string) (*inspectpkg.LatestRun, error) {
		t.Fatal("read after cancellation")
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestWorkspaceStatusPreservesRegistryState(t *testing.T) {
	for _, state := range []string{"trashed", "purging"} {
		t.Run(state, func(t *testing.T) {
			data, err := collectWorkspaceStatus(t.Context(), workspacepkg.Project{}, []workspacepkg.Workspace{{TeamName: "review", ControlRoot: "/workspace", State: state}}, func(context.Context, string) (*inspectpkg.LatestRun, error) {
				t.Fatal("inactive workspace must not be read")
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if data.Complete || data.Latest != nil || len(data.Teams) != 1 || data.Teams[0].State != "unavailable" || data.Teams[0].RegistryState != state {
				t.Fatalf("registry state lost: %+v", data)
			}
			var output bytes.Buffer
			if err := writeWorkspaceJSON(&output, data); err != nil {
				t.Fatal(err)
			}
			var document struct {
				Data workspaceStatusData `json:"data"`
			}
			if err := json.Unmarshal(output.Bytes(), &document); err != nil {
				t.Fatal(err)
			}
			if document.Data.Teams[0].RegistryState != state {
				t.Fatalf("JSON lost registry state: %s", output.String())
			}
			output.Reset()
			if err := writeWorkspaceStatus(&output, data); err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(output.String()), "\n")
			headers, cells := strings.Fields(lines[1]), strings.Fields(lines[2])
			columns := make(map[string]string)
			for i, header := range headers {
				columns[header] = cells[i]
			}
			if columns["REGISTRY_STATE"] != state || columns["AVAILABILITY"] != "unavailable" {
				t.Fatalf("text lost registry state: %s", output.String())
			}
		})
	}
}

func TestWorkspaceStatusCommand(t *testing.T) {
	fixture := newWorkspaceCLIFixture(t)
	fixture.run(t, "register")
	registry, err := workspacepkg.OpenReadWrite(fixture.stateRoot, workspacepkg.WithIDGenerator(workspacepkg.NewIDGenerator(bytes.NewReader(fixtureIDs(0x22, 0x33)))))
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := registry.CreateWorkspace(t.Context(), fixture.projectID, "example")
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := team.NewEventStore(workspace.ControlRoot, "run-latest", "session-latest")
	if err != nil {
		t.Fatal(err)
	}
	store.SetInvocationID("inv-latest")
	if _, err := store.AppendPersisted(team.RunEvent{Type: "run_started", Actor: "coordinator", Payload: []byte(`{"goal":"example"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(fixture.stateRoot, "registry.sqlite")
	before, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	output := fixture.run(t, "status", "--output", "json")
	var result struct {
		SchemaVersion int                 `json:"schema_version"`
		Data          workspaceStatusData `json:"data"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != 1 || !result.Data.Complete || result.Data.Latest == nil || result.Data.Latest.InvocationID != "inv-latest" || result.Data.Latest.Workspace != workspace.ControlRoot {
		t.Fatalf("output = %s", output)
	}
	text := fixture.run(t, "status", fixture.projectID)
	if !strings.Contains(text, "inv-latest") || !strings.Contains(text, "run-latest") || !strings.Contains(text, "0/0") {
		t.Fatal(text)
	}
	after, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("status changed registry")
	}
	if stdout, err := executeWorkspaceCommand(t, fixture.deps, "status", "--output", "invalid"); err == nil || stdout != "" {
		t.Fatalf("invalid output = %q, %v", stdout, err)
	}
	if err := os.Rename(workspace.ControlRoot, workspace.ControlRoot+"-saved"); err != nil {
		t.Fatal(err)
	}
	output = fixture.run(t, "status", "--output", "json")
	result.Data = workspaceStatusData{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if result.Data.Complete || result.Data.Latest != nil || len(result.Data.Teams) != 1 || result.Data.Teams[0].State != "unavailable" {
		t.Fatalf("missing registered root = %s", output)
	}
	if _, err := os.Stat(workspace.ControlRoot); !os.IsNotExist(err) {
		t.Fatalf("status recreated missing root: %v", err)
	}
}

func TestWriteWorkspaceStatusSeparatesAvailabilityAndOutcome(t *testing.T) {
	for _, test := range []struct {
		name, availability, outcome, displayedOutcome, errorCode string
		hasRun                                                   bool
	}{
		{"completed", "available", "completed", "completed", "-", true},
		{"failed", "available", "failed", "failed", "-", true},
		{"partial", "available", "partial", "partial", "-", true},
		{"ongoing", "available", "", "-", "-", true},
		{"no_runs", "no_runs", "", "-", "-", false},
		{"unavailable", "unavailable", "", "-", "integrity_failure", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			row := workspaceTeamStatus{Team: "example", Workspace: "/workspace", State: test.availability}
			if test.hasRun {
				row.Latest = &inspectpkg.LatestRun{InvocationID: "inv-example", BranchID: "main",
					Run: inspectpkg.RunData{RunID: "run-example", Outcome: test.outcome}}
			}
			if test.errorCode != "-" {
				row.Error = &inspectpkg.OperatorErrorView{Code: test.errorCode}
			}
			data := workspaceStatusData{Teams: []workspaceTeamStatus{row}}
			var output bytes.Buffer
			if err := writeWorkspaceStatus(&output, data); err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(output.String()), "\n")
			if len(lines) != 3 {
				t.Fatalf("unexpected table: %s", output.String())
			}
			headers, cells := strings.Fields(lines[1]), strings.Fields(lines[2])
			if len(headers) != len(cells) {
				t.Fatalf("header/cell mismatch: %s", output.String())
			}
			columns := make(map[string]string)
			for i, header := range headers {
				columns[header] = cells[i]
			}
			if columns["AVAILABILITY"] != test.availability || columns["OUTCOME"] != test.displayedOutcome || columns["ERROR"] != test.errorCode {
				t.Fatalf("availability, outcome and diagnostic must remain distinct: %s", output.String())
			}
			output.Reset()
			if err := writeWorkspaceJSON(&output, data); err != nil {
				t.Fatal(err)
			}
			var document map[string]any
			if err := json.Unmarshal(output.Bytes(), &document); err != nil {
				t.Fatal(err)
			}
			rows := document["data"].(map[string]any)["teams"].([]any)
			jsonRow := rows[0].(map[string]any)
			if jsonRow["state"] != test.availability {
				t.Fatalf("JSON availability contract changed: %s", output.String())
			}
			if test.hasRun {
				run := jsonRow["latest"].(map[string]any)["run"].(map[string]any)
				var wantOutcome any
				if test.outcome != "" {
					wantOutcome = test.outcome
				}
				if run["outcome"] != wantOutcome {
					t.Fatalf("JSON outcome contract changed: %s", output.String())
				}
			}
		})
	}
}
