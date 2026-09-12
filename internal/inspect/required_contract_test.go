package inspect

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/team"
)

func TestInspectReceiptDoesNotJoinArbitraryTaskID(t *testing.T) {
	exitCode := 0
	item := &team.TodoItem{
		ID: "task-1",
		ExecutionReceipts: []team.ExecutionReceipt{{
			RunID: "run-1", TaskID: "task-other", Attempt: 1,
			ModelExecutionID: "execution-other", TranscriptRef: "transcript-other", ExitCode: &exitCode,
		}},
	}
	query := InspectQuery{RunID: "run-1", TaskID: item.ID}
	data := projectTask(item, query)
	if len(data.Attempts) != 0 || len(data.ArtifactRefs) != 0 {
		t.Fatalf("task projection joined unrelated receipt: %#v", data)
	}
	if summary := summarizeAttempts([]*team.TodoItem{item}, query.RunID); summary.Total != 0 {
		t.Fatalf("run projection counted unrelated receipt: %#v", summary)
	}
	if entries := receiptTraceCandidates(nil, item, query); len(entries) != 0 {
		t.Fatalf("trace projection joined unrelated receipt: %#v", entries)
	}
}

func TestInspectReplayNeverExecutesProviderOrVerifier(t *testing.T) {
	workspace := t.TempDir()
	marker := filepath.Join(workspace, "verifier-ran")
	if err := os.WriteFile(filepath.Join(workspace, "hufu.yaml"), []byte("provider-url: http://127.0.0.1:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := team.NewEventStore(workspace, "run-no-exec", "session-no-exec")
	if err != nil {
		t.Fatal(err)
	}
	target := execution.ExecutionTarget{Backend: "ollama", Model: "never-call"}
	exitCode := 0
	receipt := team.ExecutionReceipt{
		RunID: "run-no-exec", TaskID: "task-no-exec", Attempt: 1, Backend: target.Backend,
		ModelExecutionID: "execution-no-exec", ProducerID: "worker", ExitCode: &exitCode,
		VerifyResult: &team.VerificationResult{Command: fmt.Sprintf("touch %s", marker), ExitCode: 0, Fingerprint: "persisted-only"},
	}
	for _, event := range []team.RunEvent{
		{Type: "run_started", Actor: "coordinator", Payload: jsonBytes(t, map[string]any{"goal": "do not execute"})},
		{Type: "task_created", Actor: "coordinator", TaskID: receipt.TaskID, Payload: jsonBytes(t, map[string]any{
			"id": receipt.TaskID, "status": team.TaskPending, "execution_target": target,
		})},
		{Type: "task_completed", Actor: "worker", TaskID: receipt.TaskID, Payload: jsonBytes(t, map[string]any{
			"id": receipt.TaskID, "status": team.TaskDone, "execution_target": target, "execution_receipts": []team.ExecutionReceipt{receipt},
		})},
		{Type: "run_finished", Actor: "coordinator", Payload: jsonBytes(t, team.RunResult{
			RunID: "run-no-exec", Outcome: team.RunOutcomePartial, StopReason: team.StopReasonUnresolvedTasks,
		})},
	} {
		if _, err := store.AppendPersisted(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := InspectReplay(t.Context(), InspectQuery{Workspace: workspace, RunID: "run-no-exec"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("persisted verifier command was executed: %v", err)
	}
}

func TestInspectDoesNotPersistNewTruth(t *testing.T) {
	fixture := buildRunFixture(t)
	before := snapshotInspectWorkspace(t, fixture.workspace)
	base := InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID}
	operations := []func() error{
		func() error { _, err := InspectRun(t.Context(), base); return err },
		func() error {
			query := base
			query.TaskID = fixture.taskID
			_, err := InspectTask(t.Context(), query)
			return err
		},
		func() error { _, err := InspectEvidence(t.Context(), base); return err },
		func() error { _, err := InspectTrace(t.Context(), base); return err },
		func() error { _, err := InspectReplay(t.Context(), base); return err },
		func() error {
			query := base
			query.TaskID = fixture.taskID
			query.ProjectID = "project-1"
			_, err := InspectContext(t.Context(), query, ContextOptions{})
			return err
		},
	}
	for index, operation := range operations {
		if err := operation(); err != nil {
			t.Fatalf("inspect operation %d: %v", index, err)
		}
	}
	after := snapshotInspectWorkspace(t, fixture.workspace)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("inspection changed workspace\nbefore: %#v\nafter:  %#v", before, after)
	}
}

type inspectFileSnapshot struct {
	Mode    fs.FileMode
	ModTime int64
	Content []byte
}

func snapshotInspectWorkspace(t *testing.T, root string) map[string]inspectFileSnapshot {
	t.Helper()
	out := make(map[string]inspectFileSnapshot)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[relative] = inspectFileSnapshot{Mode: info.Mode(), ModTime: info.ModTime().UnixNano(), Content: bytes.Clone(content)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
