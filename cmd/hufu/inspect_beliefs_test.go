package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/belief"
	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/team"
)

func TestBeliefInspectCommandJSONAndText(t *testing.T) {
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "run-cli-belief", "session-cli-belief")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	artifacts, err := team.NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := team.NewBeliefJournal(testEventJournal{store}, artifacts, team.NewSessionTree(), "main", "run-cli-belief", "session-cli-belief")
	if err != nil {
		t.Fatal(err)
	}
	modelBytes, err := os.ReadFile("../../internal/belief/testdata/synthetic-model.json")
	if err != nil {
		t.Fatal(err)
	}
	model, err := belief.DecodeModel(modelBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.BindVerificationModel(t.Context(), model, "comparison"); err != nil {
		t.Fatal(err)
	}
	receipt := team.ExecutionReceipt{RunID: "run-cli-belief", TaskID: "task-cli", Attempt: 1, ModelExecutionID: "worker-cli", RunInputSnapshotHash: "fixed-input", VerifyResult: &team.VerificationResult{ExitCode: 0, Fingerprint: "fixed-verification", Command: "private-command", Stdout: "private-output"}}
	data, err := json.Marshal(map[string]any{"id": "task-cli", "status": "done", "execution_receipt": receipt})
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AppendPersisted(team.RunEvent{Type: "task_completed", TaskID: "task-cli", Actor: "worker", Payload: data})
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := team.BeliefVerificationEpoch(model, belief.Scope{RunID: "run-cli-belief", SessionID: "session-cli-belief", BranchID: "main", TaskID: "task-cli", Attempt: 1}, source, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.OpenEpoch(t.Context(), epoch); err != nil {
		t.Fatal(err)
	}
	if err := journal.AdmitVerification(t.Context(), epoch.ID, source.ID, "comparison", ""); err != nil {
		t.Fatal(err)
	}
	states, err := journal.Replay(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	digest := states[0].State.Digest
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			command := newInspectCommand()
			var stdout bytes.Buffer
			command.SetOut(&stdout)
			command.SetErr(&bytes.Buffer{})
			command.SetArgs([]string{"--workspace", workspace, "--output", format, "--session", "session-cli-belief", "beliefs", "run-cli-belief", "--task", "task-cli"})
			if err := command.Execute(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stdout.String(), digest) || strings.Contains(stdout.String(), "private-command") || strings.Contains(stdout.String(), "private-output") || strings.Contains(stdout.String(), "\x1b") {
				t.Fatalf("unsafe or incomplete CLI output: %s", stdout.String())
			}
			if format == "json" {
				var result struct {
					Kind inspectpkg.Kind        `json:"kind"`
					Data inspectpkg.BeliefsData `json:"data"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.Kind != inspectpkg.KindBeliefs || result.Data.Epochs[0].State.Digest != digest || !*result.Data.Models[0].Synthetic {
					t.Fatalf("CLI belief data=%+v", result)
				}
			} else {
				for _, want := range []string{"Execution task-cli: done", "synthetic=true", "status=available", "0.142857143", "0.857142857", "Source: event=", "outcome=pass"} {
					if !strings.Contains(stdout.String(), want) {
						t.Fatalf("CLI text missing %q: %s", want, stdout.String())
					}
				}
			}
		})
	}
}

func TestBeliefInspectCommandMissingWorkspaceIsReadOnly(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "absent")
	command := newInspectCommand()
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--workspace", workspace, "beliefs", "run-absent"})
	if err := command.Execute(); err == nil {
		t.Fatal("missing run was accepted")
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatal("belief inspection created a missing workspace")
	}
}

func TestBeliefInspectUnavailableKeepsSourceRefs(t *testing.T) {
	data := inspectpkg.BeliefsData{BeliefRunState: team.BeliefRunState{RunID: "run-1", Models: []team.BeliefModelState{{Digest: "locked-model", Unavailable: "model_unavailable"}}, Epochs: []team.BeliefEpochState{{Epoch: belief.Epoch{ID: "epoch-1"}, Unavailable: "model_unavailable", Sources: []team.BeliefSource{{EventID: "source-1"}}}}}}
	var output bytes.Buffer
	if err := renderInspectBeliefsText(&output, inspectpkg.InspectQuery{}, data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Belief: unavailable") || !strings.Contains(output.String(), "Source: event=source-1") || strings.Contains(output.String(), "0.000000000") {
		t.Fatalf("unavailable output lost provenance or invented a mass: %s", output.String())
	}
	output.Reset()
	data = inspectpkg.BeliefsData{BeliefRunState: team.BeliefRunState{RunID: "run-1", Diagnostics: []team.BeliefDiagnostic{{Code: "model_binding_unavailable"}}}}
	if err := renderInspectBeliefsText(&output, inspectpkg.InspectQuery{}, data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Historical diagnostic: model_binding_unavailable") {
		t.Fatal("missing model binding hid its diagnostic")
	}
}

func TestBeliefPolicyInspectTextPreservesHistoricalSelection(t *testing.T) {
	data := inspectpkg.BeliefsData{
		RunID:    "run-1",
		Policies: []team.BeliefPolicyState{{Version: "synthetic-v1", Digest: "locked-policy", Unavailable: "policy_unavailable"}},
		Suggestions: []team.BeliefSuggestionView{{
			Record:     team.BeliefPolicySuggestion{Mode: "advisory", EpochID: "epoch-1", DecisionGeneration: 7, SelectedCandidateID: "check-b", SelectedHypothesisID: "h2", PolicyVersion: "synthetic-v1", PolicyDigest: "locked-policy", SnapshotDigest: "historical-snapshot", StateDigest: "historical-state", SourceCursorID: "admission-1", SourceCursorHash: "cursor-hash", SourceActor: "worker"},
			Validation: "unavailable", Unavailable: "policy_unavailable",
		}},
	}
	var output bytes.Buffer
	if err := renderInspectBeliefsText(&output, inspectpkg.InspectQuery{}, data); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mode=advisory", "generation=7 candidate=check-b hypothesis=h2 status=available", "historical-snapshot", "historical-state", "locked-policy", "admission-1 hash=cursor-hash actor=worker", "Validation unavailable: policy_unavailable"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("lost advisory history %q: %s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "unavailable=unavailable") || strings.Contains(output.String(), "0.000000000") {
		t.Fatal("renderer invented a missing status or probability")
	}
}
