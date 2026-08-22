package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

func newOccurrenceTransactionCoordinator(t *testing.T) (*Coordinator, *TodoItem) {
	t.Helper()
	workspace := t.TempDir()
	c := &Coordinator{
		session:        &TeamSession{Workspace: workspace},
		taskTracker:    NewTaskTracker(),
		executionRunID: "run-occurrence-transaction",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "transaction test"}})[0]
	return c, item
}

func TestOccurrenceTransitionRejectsStaleSubmissionBeforeProjection(t *testing.T) {
	c, item := newOccurrenceTransactionCoordinator(t)
	c.setCurrentTaskAttempt(item.ID, 1)
	oldContext := occurrenceTestContext(c, item.ID, 1)
	c.setCurrentTaskAttempt(item.ID, 2)
	currentIdentity, ok := c.activeTaskResultOccurrence(item.ID)
	if !ok || currentIdentity.Attempt != 2 {
		t.Fatalf("active identity = %#v, want attempt 2", currentIdentity)
	}

	tx, err := c.beginTaskResultSubmission(currentIdentity)
	if err != nil {
		t.Fatal(err)
	}
	staleStarted := make(chan struct{})
	transitionStarted := make(chan struct{})
	staleDone := make(chan error, 1)
	go func() {
		close(staleStarted)
		staleDone <- (coordinatorTaskResultSink{coordinator: c}).Submit(oldContext, item.ID, TaskResult{Status: TaskResultStatusSuccess, Summary: "stale"})
	}()
	transitionDone := make(chan struct{})
	go func() {
		close(transitionStarted)
		c.setCurrentTaskAttempt(item.ID, 3)
		close(transitionDone)
	}()
	<-staleStarted
	<-transitionStarted
	if err := tx.rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-staleDone; err == nil {
		t.Fatal("stale submission was accepted")
	}
	select {
	case <-transitionDone:
	case <-time.After(time.Second):
		t.Fatal("retry transition did not complete after submission gate released")
	}
	if got := c.GetTaskResult(item.ID); got != nil {
		t.Fatalf("stale submission changed result projection: %#v", got)
	}
	if got := c.taskTracker.TodoList().Items()[0].TypedResult; got != nil {
		t.Fatalf("stale submission changed todo projection: %#v", got)
	}

	if err := (coordinatorTaskResultSink{coordinator: c}).Submit(occurrenceTestContext(c, item.ID, 3), item.ID, TaskResult{Status: TaskResultStatusSuccess, Summary: "current"}); err != nil {
		t.Fatalf("current submission: %v", err)
	}
	if got := c.GetTaskResult(item.ID); got == nil || got.Summary != "current" || got.Attempt != 3 {
		t.Fatalf("current result = %#v", got)
	}
}

func TestStaleSubmitResultArtifactHasNoPersistenceOrPendingState(t *testing.T) {
	c, item := newOccurrenceTransactionCoordinator(t)
	path := filepath.Join(c.session.Workspace, "stale.txt")
	if err := os.WriteFile(path, []byte("stale artifact"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.setCurrentTaskAttempt(item.ID, 1)
	oldContext := occurrenceTestContext(c, item.ID, 1)
	c.setCurrentTaskAttempt(item.ID, 2)
	input, err := json.Marshal(map[string]any{
		"status": "success", "summary": "stale artifact", "artifacts": []map[string]string{{"path": "stale.txt"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(oldContext, toolCallForTest(string(input)))
	if err != nil || !response.IsError {
		t.Fatalf("stale artifact response = %#v, err = %v", response, err)
	}
	if c.GetTaskResult(item.ID) != nil || c.taskTracker.TodoList().Items()[0].TypedResult != nil {
		t.Fatal("stale artifact changed result projection")
	}
	controller := c.occurrenceController(item.ID)
	controller.mu.Lock()
	pending := len(controller.pending)
	controller.mu.Unlock()
	if pending != 0 {
		t.Fatalf("stale artifact left pending refs: %d", pending)
	}
}

func TestDuplicateArtifactSubmissionLeavesFirstAuthoritative(t *testing.T) {
	c, item := newOccurrenceTransactionCoordinator(t)
	firstPath := filepath.Join(c.session.Workspace, "first.txt")
	secondPath := filepath.Join(c.session.Workspace, "second.txt")
	if err := os.WriteFile(firstPath, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.setCurrentTaskAttempt(item.ID, 1)
	ctx := occurrenceTestContext(c, item.ID, 1)
	tool := &submitResultTool{coordinator: c, todoID: item.ID}
	first, err := tool.Run(ctx, toolCallForTest(`{"status":"success","summary":"first","artifacts":[{"path":"first.txt"}]}`))
	if err != nil || first.IsError {
		t.Fatalf("first artifact response = %#v, err = %v", first, err)
	}
	second, err := tool.Run(ctx, toolCallForTest(`{"status":"success","summary":"second","artifacts":[{"path":"second.txt"}]}`))
	if err != nil || !second.IsError {
		t.Fatalf("duplicate artifact response = %#v, err = %v", second, err)
	}
	got := c.GetTaskResult(item.ID)
	if got == nil || got.Summary != "first" || len(got.Artifacts) != 1 || got.Artifacts[0].Path != "first.txt" {
		t.Fatalf("authoritative first result = %#v", got)
	}
	entries, err := os.ReadDir(filepath.Join(c.session.Workspace, logsDir, "artifacts", "data"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("duplicate submission materialized %d artifacts, want 1", len(entries))
	}
}

func TestArtifactTransactionRollbackClearsMembershipAndRetainsCASData(t *testing.T) {
	c, item := newOccurrenceTransactionCoordinator(t)
	sharedPath := filepath.Join(c.session.Workspace, "shared.txt")
	newPath := filepath.Join(c.session.Workspace, "new.txt")
	if err := os.WriteFile(sharedPath, []byte("shared"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	sharedResult, err := store.Put(context.Background(), PutArtifactRequest{Path: "shared.txt", SourcePath: "shared.txt", RunID: "shared-run", TaskID: "other", Attempt: 1, Agent: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if !sharedResult.CreatedData || !sharedResult.CreatedMetadata {
		t.Fatalf("initial shared artifact ownership = %#v, want both objects created", sharedResult)
	}
	shared := sharedResult.ArtifactRef
	digest := sha256.Sum256([]byte("new"))
	newID := "sha256-" + hex.EncodeToString(digest[:])
	badMeta := filepath.Join(c.session.Workspace, logsDir, "artifacts", "meta", newID+".json")
	if err := os.WriteFile(badMeta, []byte("not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.setCurrentTaskAttempt(item.ID, 1)
	response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(occurrenceTestContext(c, item.ID, 1), toolCallForTest(`{"status":"success","summary":"partial","artifacts":[{"path":"shared.txt"},{"path":"new.txt"}]}`))
	if err != nil || !response.IsError {
		t.Fatalf("failed materialization response = %#v, err = %v", response, err)
	}
	if c.GetTaskResult(item.ID) != nil {
		t.Fatal("failed artifact transaction stored a result")
	}
	if _, err := os.Stat(filepath.Join(c.session.Workspace, logsDir, "artifacts", "data", newID)); err != nil {
		t.Fatalf("transaction-created immutable data was removed, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(c.session.Workspace, logsDir, "artifacts", "data", shared.ID)); err != nil {
		t.Fatalf("preexisting shared artifact was removed: %v", err)
	}
	if _, err := os.Stat(badMeta); err != nil {
		t.Fatalf("preexisting conflicting metadata was removed: %v", err)
	}
	controller := c.occurrenceController(item.ID)
	controller.mu.Lock()
	if controller.reserved || len(controller.pending) != 0 {
		t.Fatalf("transaction state leaked: reserved=%t pending=%d", controller.reserved, len(controller.pending))
	}
	controller.mu.Unlock()
	if got := c.taskTracker.TodoList().Items()[0].TypedResult; got != nil {
		t.Fatalf("failed submission changed todo result projection: %#v", got)
	}
	c.lastEvidenceManifestMu.RLock()
	manifest := c.lastEvidenceManifest
	c.lastEvidenceManifestMu.RUnlock()
	if manifest != nil {
		t.Fatalf("failed submission changed evidence membership: %#v", manifest)
	}
}

type acceptingArtifactSink struct {
	store     *FileArtifactStore
	path      string
	putResult ArtifactPutResult
	putErr    error
}

func (s *acceptingArtifactSink) Submit(_ context.Context, _ string, _ TaskResult) error {
	s.putResult, s.putErr = s.store.Put(context.Background(), PutArtifactRequest{
		Path: s.path, SourcePath: s.path, RunID: "run-b", TaskID: "task-b", Attempt: 1, Agent: "worker-b",
	})
	if s.putErr != nil {
		return s.putErr
	}
	return fmt.Errorf("forced post-materialization failure after second store accepted artifact")
}

func TestArtifactRollbackDoesNotDeleteEEXISTArtifactFromAliasStore(t *testing.T) {
	workspace := t.TempDir()
	alias := filepath.Join(filepath.Dir(workspace), filepath.Base(workspace)+"-alias")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Skipf("symlink aliases are unavailable: %v", err)
	}
	c := &Coordinator{
		session:        &TeamSession{Workspace: workspace},
		taskTracker:    NewTaskTracker(),
		executionRunID: "run-a",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker-a", Desc: "alias rollback"}})[0]
	path := "shared.txt"
	if err := os.WriteFile(filepath.Join(workspace, path), []byte("shared immutable artifact\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	storeB, err := NewFileArtifactStore(alias, alias)
	if err != nil {
		t.Fatal(err)
	}
	sink := &acceptingArtifactSink{store: storeB, path: path}
	c.setCurrentTaskAttempt(item.ID, 1)
	response, err := (&submitResultTool{coordinator: c, todoID: item.ID, sink: sink}).Run(
		occurrenceTestContext(c, item.ID, 1),
		toolCallForTest(`{"status":"success","summary":"alias rollback","artifacts":[{"path":"shared.txt"}]}`),
	)
	if err != nil || !response.IsError {
		t.Fatalf("rollback response = %#v, err = %v", response, err)
	}
	if sink.putErr != nil {
		t.Fatalf("alias store EEXIST acceptance failed: %v", sink.putErr)
	}
	if sink.putResult.CreatedData || sink.putResult.CreatedMetadata {
		t.Fatalf("alias store unexpectedly created immutable objects: %#v", sink.putResult)
	}

	ref, err := storeB.Get(context.Background(), sink.putResult.ID)
	if err != nil {
		t.Fatalf("alias store metadata after rollback: %v", err)
	}
	if err := storeB.Verify(context.Background(), ref); err != nil {
		t.Fatalf("alias store verification after rollback: %v", err)
	}
	resolved, err := storeB.Resolve(context.Background(), ArtifactRef{ID: ref.ID})
	if err != nil || resolved.ID != ref.ID {
		t.Fatalf("alias store resolution after rollback = %#v, err = %v", resolved, err)
	}
	opened, err := storeB.Open(context.Background(), ref.ID)
	if err != nil {
		t.Fatalf("alias store open after rollback: %v", err)
	}
	data, readErr := io.ReadAll(opened)
	closeErr := opened.Close()
	if readErr != nil || closeErr != nil || string(data) != "shared immutable artifact\n" {
		t.Fatalf("alias store data after rollback = %q, read err=%v, close err=%v", data, readErr, closeErr)
	}
	if c.GetTaskResult(item.ID) != nil || c.taskTracker.TodoList().Items()[0].TypedResult != nil {
		t.Fatal("failed submission committed task result membership")
	}
}

func TestSubmitResultIdentityRequiresDispatchProvenanceAndTrustedSeedIsSeparate(t *testing.T) {
	c, item := newOccurrenceTransactionCoordinator(t)
	c.setCurrentTaskAttempt(item.ID, 1)
	tool := &submitResultTool{coordinator: c, todoID: item.ID}
	valid := toolCallForTest(`{"status":"success","summary":"direct"}`)
	missing, err := tool.Run(context.Background(), valid)
	if err != nil || !missing.IsError || !strings.Contains(missing.Content, "identity") {
		t.Fatalf("missing identity response = %#v, err = %v", missing, err)
	}
	identity, ok := c.activeTaskResultOccurrence(item.ID)
	if !ok {
		t.Fatal("active identity missing")
	}
	direct, err := tool.Run(withSubmitResultRuntimeIdentity(context.Background(), identity), valid)
	if err != nil || direct.IsError {
		t.Fatalf("explicit identity response = %#v, err = %v", direct, err)
	}
	old := identity
	old.RunID = "old-run"
	stale, err := tool.Run(withSubmitResultRuntimeIdentity(context.Background(), old), toolCallForTest(`{"status":"success","summary":"old"}`))
	if err != nil || !stale.IsError {
		t.Fatalf("old-run response = %#v, err = %v", stale, err)
	}
	if got := c.GetTaskResult(item.ID); got == nil || got.Summary != "direct" {
		t.Fatalf("old run changed authoritative result: %#v", got)
	}

	seedCoordinator, seedItem := newOccurrenceTransactionCoordinator(t)
	seedCoordinator.storeSubmittedTaskResult(seedItem.ID, &TaskResult{TaskID: seedItem.ID, Agent: seedItem.Agent, Status: TaskResultStatusSuccess, Summary: "trusted seed", Source: "runtime"})
	if got := seedCoordinator.GetTaskResult(seedItem.ID); got == nil || got.Summary != "trusted seed" {
		t.Fatalf("trusted coordinator seed = %#v", got)
	}
}

func toolCallForTest(input string) fantasy.ToolCall {
	return fantasy.ToolCall{Name: "submit_result", Input: input}
}
