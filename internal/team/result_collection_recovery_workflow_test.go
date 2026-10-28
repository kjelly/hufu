package team

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestResultCollectionRecoveryPreservesDownstreamAndAcceptanceGates(t *testing.T) {
	for _, artifactPresent := range []bool{false, true} {
		t.Run(fmt.Sprint("artifact_present_", artifactPresent), func(t *testing.T) {
			c, id, original := blockedRecoveryFixture(t, `{"status":"success","summary":"original limitation retained for downstream"}`, true, RepairFailureSource)
			spec := TodoSpec{Agent: "worker", Desc: "consume accepted review", Model: "ollama/test", ExecutionTarget: original.ExecutionTarget, EvidenceFrom: []string{id}}
			ids := c.taskTracker.TodoList().ReserveIDs(1)
			projection, err := taskOccurrenceProjectionFromSpec(spec, ids[0])
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.admitTaskOccurrence(t.Context(), projection, ids[0], 1); err != nil {
				t.Fatal(err)
			}
			items, err := c.CommitTaskCreationResolved(t.Context(), []TodoSpec{spec}, ids)
			if err != nil {
				t.Fatal(err)
			}
			downstream := items[0].ID
			if got := c.dependencyResultsForTask(downstream); len(got) != 0 {
				t.Fatal("unaccepted recovery result reached downstream")
			}
			if err := c.SetAcceptanceSpec(AcceptanceSpec{RequiredWorkers: []string{"worker"}, RequireNoUnresolvedTasks: true}); err != nil {
				t.Fatal(err)
			}
			result, err := c.runAcceptance(t.Context())
			if err == nil || result.IsPassed() {
				t.Fatal("acceptance accepted blocked review")
			}
			c = reopenRecoveryCoordinator(t, c)
			c.executionRunID = "workflow-recovery"
			if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), id)); err != nil {
				t.Fatal(err)
			}
			if got := c.dependencyResultsForTask(downstream); len(got) != 0 {
				t.Fatal("grant alone exposed unaccepted claims")
			}
			worker := scriptWorker(scriptedTurn{stream: func(_ context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
				if !strings.Contains(call.Prompt, "original limitation") {
					t.Error("downstream lost accepted source limitation")
				}
				return textAgentResult("downstream review assembled"), nil
			}})
			c.workerAgentOverride = worker
			if _, err := c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(c.todoItemByID(id)), id); err != nil {
				t.Fatal(err)
			}
			if worker.Calls() != 0 {
				t.Fatal("recovery repeated source worker")
			}
			sources := c.dependencyResultsForTask(downstream)
			if len(sources) != 1 || sources[0].TaskID != id || sources[0].Attempt != original.Attempt || !strings.Contains(sources[0].Summary, "original limitation") {
				t.Fatalf("source provenance=%+v", sources)
			}
			if _, err := c.executeTask(t.Context(), taskDefFromTodoItem(c.todoItemByID(downstream)), downstream); err != nil {
				t.Fatal(err)
			}
			if worker.Calls() != 1 || c.todoItemByID(downstream).Status != TaskDone {
				t.Fatal("downstream did not complete exactly once")
			}
			// Acceptance is independent of a successful repaired result or worker prose.
			if err := c.SetAcceptanceSpec(AcceptanceSpec{RequiredWorkers: []string{"worker"}, RequireNoUnresolvedTasks: true, RequiredArtifacts: []string{"final-review.txt"}}); err != nil {
				t.Fatal(err)
			}
			if artifactPresent {
				if err := os.WriteFile(filepath.Join(c.projectDir, "final-review.txt"), []byte("verified final review"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			result, err = c.runAcceptance(t.Context())
			if result.IsPassed() != artifactPresent || (err == nil) != artifactPresent {
				t.Fatalf("artifact present=%t final acceptance=%+v err=%v", artifactPresent, result, err)
			}

			if currentResultCollection(c.todoItemByID(id)).Work != original.Work {
				t.Fatal("downstream replaced original evidence")
			}

		})
	}
}
