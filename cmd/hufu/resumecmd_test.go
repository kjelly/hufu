package main

import (
	"bytes"
	"encoding/json/v2"
	"github.com/kjelly/hufu/internal/team"
	"github.com/spf13/cobra"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolveCommandWorkspaceAndTeam(t *testing.T) {
	tests := []struct {
		name              string
		workspace         string
		requestedTeam     string
		workspaceExplicit bool
		wantWorkspace     string
		wantTeam          string
		wantErr           string
	}{
		{
			name:              "explicit per-team workspace",
			workspace:         "./workspace/reviewer",
			workspaceExplicit: true,
			wantWorkspace:     "workspace/reviewer",
			wantTeam:          "reviewer",
		},
		{
			name:          "team flag uses default workspace root",
			workspace:     "./workspace",
			requestedTeam: "Reviewer",
			wantWorkspace: "workspace/reviewer",
			wantTeam:      "reviewer",
		},
		{
			name:      "root workspace needs team flag",
			workspace: "./workspace",
			wantErr:   "pass --agent-team",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace, teamName, err := resolveCommandWorkspaceAndTeam(tt.workspace, tt.requestedTeam, tt.workspaceExplicit)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolveCommandWorkspaceAndTeam() error = %v, want %q", err, tt.wantErr)
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

func TestResumeRecoveryPreviewFlags(t *testing.T) {
	oldOpts, oldResult, oldModel := opts, resumeRepairResult, resumeRepairModel
	t.Cleanup(func() { opts, resumeRepairResult, resumeRepairModel = oldOpts, oldResult, oldModel })
	for _, tc := range []struct {
		name             string
		dry, report, tui bool
		task, model      string
		wantErr          bool
	}{
		{name: "ordinary resume"},
		{name: "preview", dry: true, task: "7"},
		{name: "missing task", dry: true, wantErr: true},
		{name: "model without task", model: "ollama/test", wantErr: true},
		{name: "preview model", dry: true, task: "7", model: "ollama/test", wantErr: true},
		{name: "preview report", dry: true, task: "7", report: true, wantErr: true},
		{name: "preview tui", dry: true, task: "7", tui: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts.dryRun, opts.reportMode, opts.tuiMode = tc.dry, tc.report, tc.tui
			resumeRepairResult, resumeRepairModel = tc.task, tc.model
			if err := validateResumeRepairFlags(); (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestResumeRecoveryPreviewDoesNotActivateBranch(t *testing.T) {
	originalOpts := opts
	originalTeam, originalSearch, originalBranch, originalRun, originalTask, originalModel := resumeTeamName, resumeSearch, resumeBranch, resumeRun, resumeRepairResult, resumeRepairModel
	t.Cleanup(func() {
		opts = originalOpts
		resumeTeamName, resumeSearch, resumeBranch, resumeRun, resumeRepairResult, resumeRepairModel = originalTeam, originalSearch, originalBranch, originalRun, originalTask, originalModel
	})
	root := t.TempDir()
	teamDir := filepath.Join(root, "teams", "fixture-team")
	for _, dir := range []string{teamDir, filepath.Join(root, ".git"), filepath.Join(root, "home")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("HUFU_STATE_HOME", filepath.Join(root, "unused-state"))
	t.Chdir(root)
	if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte("name: fixture-team\nworker-model: ollama/fixture\ncoordinator-model: ollama/fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "worker.md"), []byte("---\nname: worker\nrole: worker\n---\nInspect fixture only.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := writeSessionFacadeFixture(t, &team.TodoItem{ID: "7", Agent: "worker", Status: team.TaskBlocked, FailureEvent: &team.FailureEventPayload{FailureClass: team.FailureProtocol}})
	tree := team.NewSessionTree()
	tree.Branches["other"] = &team.SessionBranch{ID: "other", Name: "other", CreatedAt: "2026-10-28T00:00:00Z"}
	tree.ActiveBranch = "other"
	if err := team.SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	before := runtimeStoreFiles(t, workspace)
	namesBefore := completionDirectoryNames(t, workspace)
	opts = runOptions{workspace: workspace, workspaceMode: "exact", dryRun: true, canonicalRun: true, outputFormat: "json", displayMode: "plain", eventFormat: "text"}
	resumeTeamName, resumeSearch, resumeBranch, resumeRun, resumeRepairResult, resumeRepairModel = "fixture-team", filepath.Join(root, "teams"), "main", "run-1", "7", ""
	cmd := &cobra.Command{Use: "resume"}
	cmd.SetContext(t.Context())
	var output bytes.Buffer
	cmd.SetOut(&output)
	if err := runResumeCommand(cmd, nil); err != nil {
		t.Fatal(err)
	}
	var assessment team.ResultCollectionRecoveryAssessment
	if err := json.Unmarshal(output.Bytes(), &assessment); err != nil {
		t.Fatalf("invalid JSON: %v %s", err, output.String())
	}
	if assessment.Disposition != team.RecoveryBlocked {
		t.Fatalf("unsupported task disposition=%s", assessment.Disposition)
	}
	after := runtimeStoreFiles(t, workspace)
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(namesBefore, completionDirectoryNames(t, workspace)) {
		t.Fatal("preview wrote persistent workspace data")
	}
	tree, err := team.LoadSessionTree(workspace)
	if err != nil || tree.ActiveBranch != "other" {
		t.Fatalf("preview changed active branch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "unused-state")); !os.IsNotExist(err) {
		t.Fatalf("preview created managed state: %v", err)
	}
}

func TestResumeRecoveryAssessmentOutputPreservesUnknownAndBlocked(t *testing.T) {
	original := opts
	t.Cleanup(func() { opts = original })
	assessment := &team.ResultCollectionRecoveryAssessment{TaskID: "requested-task", BranchID: "selected-branch", SourceRunID: "original-run", Version: 1, Disposition: team.RecoveryBlocked, Checks: []team.ResultCollectionRecoveryCheck{{Code: "target", Passed: false, Diagnostic: "unsupported recovery target"}}}
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			opts.outputFormat = format
			cmd := &cobra.Command{}
			var output bytes.Buffer
			cmd.SetOut(&output)
			if err := printResultRecoveryAssessment(cmd, assessment); err != nil {
				t.Fatal(err)
			}
			if format == "json" {
				var value map[string]any
				if err := json.Unmarshal(output.Bytes(), &value); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"accepted_tasks", "turns_used", "remaining_turns", "additional_turns"} {
					if _, present := value[name]; present {
						t.Fatalf("unknown %s rendered as zero", name)
					}
				}
				if value["task_id"] != "requested-task" || value["branch_id"] != "selected-branch" || value["source_run_id"] != "original-run" {
					t.Fatal("preview lost requested scope")
				}
			} else if !strings.Contains(output.String(), "unknown") || !strings.Contains(output.String(), "no grant") || !strings.Contains(output.String(), "unsupported recovery target") || !strings.Contains(output.String(), "requested-task") || !strings.Contains(output.String(), "selected-branch") || !strings.Contains(output.String(), "original-run") {
				t.Fatalf("output=%s", output.String())
			}
		})
	}
}
