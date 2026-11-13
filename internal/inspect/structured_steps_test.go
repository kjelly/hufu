package inspect

import (
	"context"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
)

func TestInspectStructuredStepUsesDurableRuntimeAttribution(t *testing.T) {
	workspace := t.TempDir()
	session := &team.TeamSession{Dir: workspace, Workspace: workspace, Config: agent.TeamConfig{Name: "step-inspect"},
		Agents: map[string]*agent.AgentDef{"worker": {Name: "worker", Role: "worker"}}}
	if err := session.SetCompatibilityWorkspaceScope(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	c, err := team.NewCoordinator(session, "http://127.0.0.1:1/v1", "", nil, []config.ModelEntry{{ID: "proposed-only"}}, team.RoleModels{}, 1,
		false, false, false, nil, nil, nil, false, "", true, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := c.PrepareContextPreflightContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	store := c.EventStore()
	if store == nil {
		t.Fatal("public preflight did not create a durable store")
	}
	appendEvent := func(kind, task string, payload any) team.RunEvent {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		event, err := store.AppendPersistedContext(t.Context(), team.RunEvent{Type: kind, TaskID: task, Actor: "coordinator", Payload: raw})
		if err != nil {
			t.Fatal(err)
		}
		return event
	}
	appendEvent("run_started", "", map[string]any{"team": session.Config.Name})
	contract := team.ExecutionContract{Steps: []team.ExecutionStep{{ID: "inspect", Tool: "inspector", Effect: team.ExecutionEffectRead}}}
	item := c.TaskTracker().TodoList().AddBatch([]team.TodoSpec{{Agent: "worker", Desc: "inspect", Execution: contract}})[0]
	appendEvent("task_created", item.ID, map[string]any{"id": item.ID, "status": "pending", "agent": "worker", "execution": contract, "occurrence_revision": item.OccurrenceRevision})
	result, err := c.RunStructuredTask(t.Context(), item.ID, 1, team.StructuredStepRunnerFunc(func(context.Context, team.StructuredStepRequest) (team.ExecutionStepResult, error) {
		return team.ExecutionStepResult{Stdout: "private-step-diagnostic-9384"}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	receipt := result.Receipts[0]
	appendEvent("task_completed", item.ID, map[string]any{"id": item.ID, "status": "done", "agent": "worker",
		"typed_result": team.TaskResult{TaskID: item.ID, Agent: "worker", Attempt: 1, Status: team.TaskResultStatusSuccess, Summary: "completed", ReceiptIDs: []string{receipt.ID}}})
	finish := appendEvent("run_finished", "", map[string]any{"outcome": "completed", "response": "completed"})
	query := InspectQuery{Workspace: workspace, RunID: receipt.RunID, TaskID: item.ID}
	envelope, err := InspectTask(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(TaskData)
	if len(data.StructuredSteps) != 1 || data.StructuredSteps[0].ExecutionKind != "unknown" || data.StructuredSteps[0].ProposedModel != "proposed-only" || len(data.StructuredSteps[0].Invocations) != 0 {
		t.Fatalf("task attribution=%+v", data.StructuredSteps)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-step-diagnostic-9384") {
		t.Fatal("inspect task exposed raw step output")
	}
	trace, err := InspectTrace(t.Context(), InspectQuery{Workspace: workspace, RunID: receipt.RunID})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range trace.Data.(TraceData).Entries {
		if entry.StructuredStep != nil {
			found = entry.StructuredStep.ReceiptID == receipt.ID && entry.StructuredStep.ProposedModel == "proposed-only" && entry.StructuredStep.ExecutionKind == "unknown"
		}
	}
	if !found {
		t.Fatal("trace omitted runtime step attribution")
	}
	saved, err := team.LoadRunReport(t.Context(), workspace, finish.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.StructuredSteps) != 1 || saved.StructuredSteps[0].ReceiptSHA256 != data.StructuredSteps[0].ReceiptSHA256 || saved.StructuredSteps[0].SourceHash != data.StructuredSteps[0].SourceHash {
		t.Fatalf("saved report attribution=%+v", saved.StructuredSteps)
	}
}
