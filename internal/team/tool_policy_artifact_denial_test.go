package team

import (
	"context"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/tools"
)

func TestArtifactPathDenialReachesAttemptReceipt(t *testing.T) {
	for _, path := range []string{"data/blocked", "meta/blocked.json", "media/data/blocked", "media/meta/blocked.json"} {
		t.Run(path, func(t *testing.T) {
			c := newTestCoordinator(t)
			c.projectDir = c.session.Workspace
			def := c.session.Agents["worker"]
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: def.Name, Desc: "inspect"}})[0]
			ctx, cancel, roundCancel, err := c.buildDirectAgentTaskContext(t.Context(), def, def.Name, item.Desc, item.ID, "test", []string{"view"})
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			defer roundCancel()
			collector := &attemptToolDispositions{}
			ctx = context.WithValue(ctx, tools.ToolExecutionDispositionReporterKey, newToolDispositionReporter(collector, SideEffectNone, c.contextRunID(), item.ID, 1))
			view := tools.NewViewTool(tools.WithWorkDir(c.projectDir), tools.WithAllowedPaths([]string{c.projectDir}))
			gated := c.gatePolicyTools([]fantasy.AgentTool{view})[0]
			response, err := gated.Run(ctx, fantasy.ToolCall{ID: "artifact-denied", Name: "view", Input: `{"file_path":"` + filepath.ToSlash(filepath.Join(logsDir, "artifacts", path)) + `"}`})
			if err != nil || !response.IsError {
				t.Fatalf("denial = %#v, %v", response, err)
			}
			receipt := ExecutionReceipt{Attempt: 1, ToolDispositions: collector.snapshot()}
			if err := c.setAttemptReceipt(item.ID, &receipt); err != nil {
				t.Fatal(err)
			}
			saved := c.todoItemByID(item.ID).ExecutionReceipt.ToolDispositions
			if len(saved) != 1 || saved[0].Kind != ToolExecutionPolicyDenied || saved[0].ReasonCode != "path_out_of_scope" || saved[0].Executed || saved[0].TodoID != item.ID || saved[0].RunID != c.contextRunID() || saved[0].ToolCallID != "artifact-denied" {
				t.Fatalf("receipt dispositions = %+v", saved)
			}
		})
	}
}
