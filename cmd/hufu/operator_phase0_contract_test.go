package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/team"
	"github.com/spf13/cobra"
)

func TestOperatorPhase0WorkspaceContract(t *testing.T) {
	tests := []struct {
		name              string
		workspace         string
		team              string
		workspaceExplicit bool
		wantWorkspace     string
		wantTeam          string
		wantError         string
	}{
		{
			name:              "resume exact workspace infers team",
			workspace:         filepath.Join("state", "reviewer"),
			workspaceExplicit: true,
			wantWorkspace:     filepath.Join("state", "reviewer"),
			wantTeam:          "reviewer",
		},
		{
			name:              "recovery exact workspace keeps custom basename",
			workspace:         filepath.Join("state", "custom"),
			team:              " Reviewer ",
			workspaceExplicit: true,
			wantWorkspace:     filepath.Join("state", "custom"),
			wantTeam:          "reviewer",
		},
		{
			name:          "team flag expands default workspace root",
			workspace:     "workspace",
			team:          "Reviewer",
			wantWorkspace: filepath.Join("workspace", "reviewer"),
			wantTeam:      "reviewer",
		},
		{
			name:      "workspace root without team is ambiguous",
			workspace: "workspace",
			wantError: "cannot infer team",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace, teamName, err := resolveCommandWorkspaceAndTeam(tt.workspace, tt.team, tt.workspaceExplicit)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("resolveCommandWorkspaceAndTeam() error = %v, want containing %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveCommandWorkspaceAndTeam() error = %v", err)
			}
			if workspace != tt.wantWorkspace || teamName != tt.wantTeam {
				t.Fatalf("resolveCommandWorkspaceAndTeam() = (%q, %q), want (%q, %q)", workspace, teamName, tt.wantWorkspace, tt.wantTeam)
			}
		})
	}
}

func TestOperatorPhase0RootWorkspaceRemainsLegacyBase(t *testing.T) {
	previous := opts
	t.Cleanup(func() { opts = previous })

	base := t.TempDir()
	opts.workspace = base
	session := &team.TeamSession{}
	if err := resolveTeamWorkspacePath("reviewer", session); err != nil {
		t.Fatalf("resolveTeamWorkspacePath() error = %v", err)
	}
	want := filepath.Join(base, "reviewer")
	if session.Workspace != want || session.Config.WorkspaceDir != want {
		t.Fatalf("legacy root workspace = (%q, %q), want %q", session.Workspace, session.Config.WorkspaceDir, want)
	}
}

func TestOperatorPhase0HelpAndCompletionCreateNothing(t *testing.T) {
	rootDir := t.TempDir()
	missingWorkspace := filepath.Join(rootDir, "missing-workspace")
	missingTeams := filepath.Join(rootDir, "missing-teams")
	previous := opts
	t.Cleanup(func() { opts = previous })
	opts.workspace = missingWorkspace
	opts.agentTeamSearchPath = missingTeams
	opts.providerURL = "http://127.0.0.1:1"

	commands := [][]string{
		{"--help"},
		{"chat", "--help"},
		{"resume", "--help"},
		{"retry", "--help"},
		{"reconcile", "--help"},
		{"context", "--help"},
		{"context", "promotion", "--help"},
		{"inspect", "--help"},
		{"skill", "--help"},
		{"team", "--help"},
	}
	for _, args := range commands {
		command := newRootCommand()
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		command.SetArgs(args)
		if err := command.Execute(); err != nil {
			t.Fatalf("hufu %s: %v", strings.Join(args, " "), err)
		}
	}

	command := newRootCommand()
	complete, ok := command.GetFlagCompletionFunc("agent-team")
	if !ok {
		t.Fatal("agent-team completion is not registered")
	}
	_, directive := complete(command, nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("agent-team completion directive = %v, want NoFileComp", directive)
	}
	for _, path := range []string{missingWorkspace, missingTeams} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("help/completion created %q: %v", path, err)
		}
	}
}

func TestOperatorPhase0RootJSONGolden(t *testing.T) {
	coordinator := &team.Coordinator{}
	coordinator.SetLastRunResult(&team.RunResult{
		RunID:         "run-golden",
		Outcome:       team.RunOutcomeCompleted,
		GoalSatisfied: true,
		GoalMode:      team.GoalModeOutcome,
		Response:      "completed result",
		StopReason:    team.StopReasonCompleted,
		Acceptance:    &team.AcceptanceResult{State: team.AcceptancePassed, Passed: true},
	})
	got := captureStdout(t, func() {
		if err := printResultJSON("completed result", map[string]*teamContext{
			"demo": {teamName: "demo", coordinator: coordinator},
		}, nil); err != nil {
			t.Fatalf("printResultJSON() error = %v", err)
		}
	})
	assertOperatorGolden(t, "root_completed.golden.json", []byte(got))
}

func TestOperatorPhase0InspectJSONGolden(t *testing.T) {
	envelope := &inspectpkg.Envelope{
		SchemaVersion: inspectpkg.SchemaVersion,
		Kind:          inspectpkg.KindRun,
		Query:         inspectpkg.InspectQuery{RunID: "run-golden", BranchID: "main"},
		Integrity:     inspectpkg.Integrity{EventChain: "verified", Projection: "consistent"},
		Data: inspectpkg.RunData{
			RunID:           "run-golden",
			TerminalEventID: "event-terminal",
			Outcome:         string(team.RunOutcomePartial),
			StopReason:      string(team.StopReasonUnresolvedTasks),
			Acceptance:      string(team.AcceptanceFailed),
			Completion:      "incomplete",
			TaskSummary:     inspectpkg.TaskSummary{Total: 2, Done: 1, Unresolved: 1},
			AttemptSummary:  inspectpkg.AttemptSummary{Total: 2, Failed: 1},
			EvidenceRefs:    []string{"manifest:sha256:fixture"},
		},
		Diagnostics: []inspectpkg.Diagnostic{},
	}
	command := &cobra.Command{}
	var output bytes.Buffer
	command.SetOut(&output)
	if err := finishInspect(command, string(inspectpkg.FormatJSON), envelope, nil); err != nil {
		t.Fatalf("finishInspect() error = %v", err)
	}
	assertOperatorGolden(t, "inspect_run_partial.golden.json", output.Bytes())
}

func TestOperatorPhase0ContractInventory(t *testing.T) {
	type surface struct {
		Name               string   `json:"name"`
		WorkspaceSemantics string   `json:"workspace_semantics"`
		OutputContract     string   `json:"output_contract"`
		ExitContract       string   `json:"exit_contract"`
		CharacterizedBy    []string `json:"characterized_by"`
	}
	var inventory struct {
		SchemaVersion  int       `json:"schema_version"`
		VerifiedCommit string    `json:"verified_commit"`
		Surfaces       []surface `json:"surfaces"`
	}
	decodeOperatorFixture(t, "contract_inventory.json", &inventory)
	if inventory.SchemaVersion != 1 || inventory.VerifiedCommit == "" {
		t.Fatalf("invalid inventory header: %#v", inventory)
	}
	want := []string{"chat", "context", "inspect", "promotion", "reconcile", "resume", "retry", "root", "skill", "team"}
	got := make([]string, 0, len(inventory.Surfaces))
	for _, item := range inventory.Surfaces {
		got = append(got, item.Name)
		if item.WorkspaceSemantics == "" || item.OutputContract == "" || item.ExitContract == "" || len(item.CharacterizedBy) == 0 {
			t.Errorf("surface %q has an incomplete contract: %#v", item.Name, item)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("inventory surfaces = %v, want %v", got, want)
	}
}

func TestOperatorPhase0JourneyFixtures(t *testing.T) {
	type expectation struct {
		Activity      string `json:"activity"`
		RunOutcome    string `json:"run_outcome"`
		Attention     string `json:"attention"`
		PrimaryAction string `json:"primary_action"`
		Availability  string `json:"availability"`
	}
	var fixtures struct {
		SchemaVersion int `json:"schema_version"`
		Journeys      []struct {
			ID       string      `json:"id"`
			Expected expectation `json:"expected"`
		} `json:"journeys"`
	}
	decodeOperatorFixture(t, "journeys.json", &fixtures)
	if fixtures.SchemaVersion != 1 {
		t.Fatalf("journey schema version = %d, want 1", fixtures.SchemaVersion)
	}
	want := map[string]expectation{
		"ambiguous_scope":         {Activity: "unknown", Attention: "human_required", PrimaryAction: "select-scope", Availability: "blocked"},
		"completed":               {Activity: "finished", RunOutcome: "completed", Attention: "none", PrimaryAction: "none-required", Availability: "available"},
		"external_effect_unknown": {Activity: "blocked", Attention: "review_required", PrimaryAction: "inspect-task-recovery", Availability: "available"},
		"failed":                  {Activity: "finished", RunOutcome: "failed", Attention: "review_required", PrimaryAction: "review-result", Availability: "available"},
		"interrupted":             {Activity: "interrupted", Attention: "action_available", PrimaryAction: "resume-session", Availability: "blocked"},
		"verifying":               {Activity: "verifying", Attention: "informational", PrimaryAction: "wait-runtime", Availability: "available"},
	}
	if len(fixtures.Journeys) != len(want) {
		t.Fatalf("journey count = %d, want %d", len(fixtures.Journeys), len(want))
	}
	for _, fixture := range fixtures.Journeys {
		expected, ok := want[fixture.ID]
		if !ok {
			t.Errorf("unexpected journey %q", fixture.ID)
			continue
		}
		if fixture.Expected != expected {
			t.Errorf("journey %q expectation = %#v, want %#v", fixture.ID, fixture.Expected, expected)
		}
		delete(want, fixture.ID)
	}
	if len(want) != 0 {
		t.Fatalf("missing journeys: %v", want)
	}
}

func TestOperatorUsabilityMeasurementScript(t *testing.T) {
	script := filepath.Join("..", "..", "scripts", "operator-usability-measure.sh")
	command := exec.CommandContext(t.Context(), "bash", script, "baseline", "-")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("operator usability script: %v\n%s", err, output)
	}
	text := string(output)
	for _, expected := range []string{"Mode: baseline", "Journey A", "Journey F", "State correct", "Safety incidents"} {
		if !strings.Contains(text, expected) {
			t.Errorf("measurement template missing %q", expected)
		}
	}
}

func assertOperatorGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", "operator", name))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("operator golden %s changed\nwant:\n%s\ngot:\n%s", name, want, got)
	}
}

func decodeOperatorFixture(t *testing.T, name string, target any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "operator", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}
