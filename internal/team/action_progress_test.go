package team

import (
	"context"
	jsonv2 "encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestActionProgressWireFramesAreBoundedAndCannotSupplyIdentity(t *testing.T) {
	var frames []ActionProgress
	ctx := WithActionProgressReporter(t.Context(), func(value ActionProgress) { frames = append(frames, value) })
	w := newActionProgressWriter(ctx)
	for _, text := range []string{
		"ordinary diagnostic\n",
		actionProgressPrefix + `{"version":1,"kind":"heartbeat","stage":"deploy","message":"ok","action_id":"forged"}` + "\n",
		actionProgressPrefix + `{"version":1,"version":1,"kind":"heartbeat","stage":"deploy","message":"ok"}` + "\n",
		actionProgressPrefix + strings.Repeat("x", 20000) + "\n",
		actionProgressPrefix + `{"version":1,"kind":"stage","stage":"deploy","status":"running","message":"line\n\u001b[31m next"}` + "\n",
	} {
		for start := 0; start < len(text); start += 7 {
			end := min(start+7, len(text))
			if n, err := w.Write([]byte(text[start:end])); n != end-start || err != nil {
				t.Fatalf("write = %d, %v", n, err)
			}
		}
	}
	if len(frames) != 1 || strings.ContainsAny(frames[0].Message, "\n\x1b") {
		t.Fatalf("frames = %#v", frames)
	}
	if len(w.pending) > 4096 || len(w.diagnostic.String()) > 17*1024 || !w.diagnostic.Truncated() {
		t.Fatal("unbounded adapter diagnostic")
	}
	if ReportActionProgress(ctx, ActionProgress{Version: 1, Kind: "stage", Stage: "../bad"}) {
		t.Fatal("unsafe stage accepted")
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if ReportActionProgress(cancelCtx, frames[0]) {
		t.Fatal("cancelled reporter accepted a late observation")
	}
}

func TestCommandActionStreamsBeforeCompletionAndProgressDoesNotAuthorizeSuccess(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "observed")
	t.Setenv("MARKER", marker)
	command := `cat >/dev/null; printf 'HUFU_ACTION_PROGRESS {"version":1,"kind":"stage","stage":"deploy","status":"completed","message":"claimed complete"}\n' >&2; for i in 1 2 3 4 5 6 7 8 9 10; do test -f "$MARKER" && exit 1; sleep 0.05; done; exit 9`
	provider := &commandActionProvider{capability: "demo", command: []string{"/bin/sh", "-c", command}}
	ctx := WithActionProgressReporter(t.Context(), func(ActionProgress) {
		if err := os.WriteFile(marker, []byte("observed"), 0o600); err != nil {
			t.Error(err)
		}
	})
	_, err := provider.Execute(ctx, Action{Capability: "demo", Type: "apply"})
	if got := classifyTaskFailure(err); got != FailureExecution {
		t.Fatalf("failure = %s, %v", got, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("progress arrived only after action exit")
	}
}

func TestActionProgressProjectionReplaysWithoutAdvancingTask(t *testing.T) {
	session := workflowTestSession(t)
	events, err := NewEventStore(session.Workspace, "run-progress", "session-progress")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	journal, err := openTaskJournal(session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	c := &Coordinator{session: session, taskTracker: NewTaskTracker(), eventStore: events, journal: journal, executionRunID: "run-progress", phaseWorkflow: executePhaseWorkflow(t, session), projectDir: session.Workspace}
	c.SetEventJournal(eventStoreJournal{store: events})
	task, item := createAdmittedTestTask(t, c, TaskDef{ID: "execute", Agent: "executor", Goal: "apply", Phase: PhaseExecute, Action: &Action{Capability: "structured-actions", Type: "apply"}})
	if err := c.commitTaskTransitionFromCurrent(t.Context(), item.ID, TaskInProgress, "executing", "", attemptStartMetadata(1)); err != nil {
		t.Fatal(err)
	}
	c.setCurrentTaskAttempt(item.ID, 1)
	before := c.todoItemByID(item.ID)
	frame := ActionProgress{Version: 1, Kind: "stage", Stage: "deploy", Status: "completed", Message: "adapter observation"}
	c.recordActionProgress(task, item.ID, "action-1", 1, frame)
	c.recordActionProgress(task, item.ID, "stale", 0, frame)
	got := c.todoItemByID(item.ID)
	if taskTransitionEventKey(got) != taskTransitionEventKey(before) || toCanonicalTaskShadow(got).ActionProgress == nil {
		t.Fatal("observation escaped shadow parity or changed lifecycle identity")
	}
	if got.Status != TaskInProgress || got.TypedResult != nil || got.VerifyResult != nil || got.Progress != before.Progress || got.OccurrenceRevision != before.OccurrenceRevision || got.ActionProgress.Sequence != 1 {
		t.Fatalf("progress changed execution authority: %#v", got)
	}
	got.ActionProgress.Stage = "mutated"
	if c.todoItemByID(item.ID).ActionProgress.Stage != "deploy" {
		t.Fatal("projection aliases caller")
	}
	if err := c.commitTaskTransitionFromCurrent(t.Context(), item.ID, TaskError, "failed", "", nil); err != nil {
		t.Fatal(err)
	}
	c.recordActionProgress(task, item.ID, "action-1", 1, frame)
	stored, err := events.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToTodoList(stored)
	if len(replayed) != 1 || replayed[0].Status != TaskError || replayed[0].ActionProgress == nil || replayed[0].ActionProgress.Sequence != 1 || replayed[0].ActionProgress.Stage != "deploy" {
		t.Fatalf("replay = %#v", replayed)
	}
	data, err := jsonv2.Marshal(replayed[0])
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip TodoItem
	if err := jsonv2.Unmarshal(data, &roundtrip); err != nil || roundtrip.ActionProgress.Stage != "deploy" {
		t.Fatalf("session projection = %#v, %v", roundtrip.ActionProgress, err)
	}
	cached, err := loadTaskJournal(session.Workspace, time.Now(), time.Hour, 10)
	if err != nil || len(cached) != 0 {
		t.Fatalf("progress entered completed-result cache: %#v, %v", cached, err)
	}
}
