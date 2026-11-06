package team

import (
	"context"
	"testing"

	"github.com/kjelly/hufu/internal/media"
)

func TestMediaFanoutTransportRequiresBothLiveLeases(t *testing.T) {
	for _, scenario := range []string{"allowed", "parent revoked", "leaf revoked", "wrong model execution", "changed slot", "parent revoked during journal read"} {
		t.Run(scenario, func(t *testing.T) {
			parent, ctx, item, request := nativeMediaProviderFixture(t, func(session *TeamSession) {
				session.Agents["worker"].ExtraModels = []string{"fixture/next"}
			})
			parent.saveCheckpoint()
			leaf := cloneCoordinator(parent, cloneSession(parent.session, parent.session.Workspace))
			leaf.modelFanout = &modelFanoutBinding{parent: parent, taskID: item.ID, index: 0, target: item.ExecutionTarget}
			var err error
			leaf.modelExecutionID, err = nativeMediaFanoutExecutionID(item, parent.executionRunID, 0)
			if err != nil {
				t.Fatal(err)
			}
			private := cloneTodoItem(item)
			private.Status = TaskPending
			private.DispatchID = ""
			leaf.taskTracker = NewTaskTracker()
			leaf.taskTracker.TodoList().SetRunID(parent.executionRunID)
			leaf.taskTracker.TodoList().AddReserved([]*TodoItem{private})
			ctx = context.WithValue(ctx, leafExecutionKey{}, true)
			if err := leaf.CommitTaskTransition(ctx, item.ID, TaskPending, TaskInProgress, "leaf", "", nil); err != nil {
				t.Fatal(err)
			}
			leaf.setCurrentTaskAttempt(item.ID, 1)
			before, err := parent.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := leaf.emitTaskEventsFromCheckpoint(leaf.TaskTracker().TodoList().Items()); err != nil {
				t.Fatal(err)
			}
			leaf.saveCheckpoint()
			after, err := parent.EventJournal().ReadEvents(t.Context())
			if err != nil || len(after) != len(before) {
				t.Fatal("private leaf published a parent lifecycle checkpoint", err)
			}
			stored := LoadSession(parent.session.Workspace)
			if stored == nil || len(stored.Tasks) != 1 || stored.Tasks[0].OccurrenceRevision != item.OccurrenceRevision {
				t.Fatal("private leaf overwrote the canonical parent session checkpoint")
			}
			identity, found := leaf.activeTaskResultOccurrence(item.ID)
			if !found {
				t.Fatal("private leaf lease not opened")
			}
			ctx = withSubmitResultRuntimeIdentity(ctx, identity)
			ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: parent.executionRunID, TaskID: item.ID, AgentName: item.Agent,
				AgentRole: "worker", Attempt: 1, Purpose: "task_execution", ModelExecutionID: leaf.modelExecutionID})
			request.InvocationID = "fanout-transport"
			admission := providerRequestAdmission{c: leaf}
			if err := admission.CommitProviderInvocation(ctx, request); err != nil {
				t.Fatal("valid independent fanout permission", err)
			}
			revokeParent := func() {
				t.Helper()
				if err := parent.CommitTaskTransition(ctx, item.ID, TaskInProgress, TaskVerifying, "revoked", "", nil); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "parent revoked":
				revokeParent()
			case "leaf revoked":
				if err := leaf.CommitTaskTransition(ctx, item.ID, TaskInProgress, TaskVerifying, "revoked", "", nil); err != nil {
					t.Fatal(err)
				}
			case "wrong model execution":
				leaf.modelExecutionID = "another-model-execution"
			case "changed slot":
				leaf.modelFanout.index = 1
			case "parent revoked during journal read":
				leaf.SetEventJournal(&mediaTransportReadHook{AtomicEventJournal: leaf.EventJournal().(AtomicEventJournal), afterRead: revokeParent})
			}
			err = admission.AuthorizeProviderTransport(ctx, request)
			if scenario == "allowed" {
				if err != nil {
					t.Fatal("valid fanout transport rejected", err)
				}
			} else {
				requireMediaCode(t, err, media.AccessDenied)
			}
		})
	}
}
