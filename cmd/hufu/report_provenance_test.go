package main

import (
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestCurrentRunReportDiagnosticsDistinguishesReceiptOccurrences(t *testing.T) {
	for _, tc := range []struct {
		name       string
		prior      int
		retries    int
		wantWinner bool
	}{
		{name: "superseded occurrence", prior: 1, retries: 1, wantWinner: true},
		{name: "same occurrence", prior: 2, retries: 1},
		{name: "legacy unknown", retries: 1},
		{name: "counter mismatch", prior: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prior := team.ExecutionReceipt{RunID: "run-current", TaskID: "task-1", Attempt: 1, OccurrenceAttempt: tc.prior, ModelExecutionID: "prior", ProducerID: "worker", ExitCode: new(0), TranscriptRef: "prior-transcript"}
			current := team.ExecutionReceipt{RunID: prior.RunID, TaskID: prior.TaskID, Attempt: 1, OccurrenceAttempt: 2, ModelExecutionID: "current", ProducerID: "worker", ExitCode: new(0), TranscriptRef: "current-transcript"}
			item := &team.TodoItem{ID: current.TaskID, Agent: "worker", Status: team.TaskDone, Retries: tc.retries, ExecutionReceipt: &current, ExecutionReceipts: []team.ExecutionReceipt{prior, current}}
			artifact := team.ArtifactRef{ID: current.TranscriptRef, RunID: current.RunID, TaskID: current.TaskID, Attempt: current.Attempt}
			manifest := &team.EvidenceManifest{RunID: current.RunID, Status: "accepted", ArtifactRefs: []team.ArtifactRef{artifact}, EvidenceResults: []team.EvidenceResult{{RequirementID: "task:" + current.TaskID, Status: "passed", ArtifactRefs: []team.ArtifactRef{artifact}, Binding: &team.EvidenceBinding{RunID: current.RunID, TaskID: current.TaskID, Attempt: current.Attempt, ModelExecutionID: current.ModelExecutionID, ProducerID: current.ProducerID, TranscriptRef: current.TranscriptRef, ArtifactIDs: []string{current.TranscriptRef}}}}}
			diagnostics := currentRunReportDiagnostics([]*team.TodoItem{item}, manifest)["worker"]
			if strings.Contains(diagnostics, "model_execution_id: `current`") != tc.wantWinner || strings.Contains(diagnostics, "prior-transcript") {
				t.Fatalf("winner=%t diagnostics=%s", tc.wantWinner, diagnostics)
			}
		})
	}
}
