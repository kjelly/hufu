package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/media"
)

func codexImageProjectionFixture(t *testing.T, configure ...func(*TeamSession)) (*Coordinator, context.Context, AttemptRequest, string) {
	t.Helper()
	c, _ := newCodexImageCoordinator(t, codexImageProtocolVersion, func(string) []fakeCodexStep { return nil }, configure...)
	ctx := openMediaInvocation(t, c)
	task := nativeInvocationTask(t, c, ctx, media.InvocationSingleTask)
	return codexImageProjectionRequest(t, c, ctx, task)
}

func codexImageProjectionRequest(t *testing.T, c *Coordinator, ctx context.Context, task TaskDef) (*Coordinator, context.Context, AttemptRequest, string) {
	t.Helper()
	spec, id := prepareMediaOccurrenceForModel(t, c, ctx, task, "codex/gpt-5-codex")
	items, err := c.CommitTaskCreationResolved(ctx, []TodoSpec{spec}, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitTaskTransition(ctx, id, TaskPending, TaskInProgress, "running", "", nil); err != nil {
		t.Fatal(err)
	}
	c.setCurrentTaskAttempt(id, 1)
	identity, ok := c.activeTaskResultOccurrence(id)
	if !ok {
		t.Fatal("missing attempt lease")
	}
	ctx = withSubmitResultRuntimeIdentity(ctx, identity)
	item := c.todoItemByID(items[0].ID)
	request := AttemptRequest{RunID: c.executionRunID, BranchID: c.activeBranchID(), TaskID: id, Attempt: 1, Agent: c.session.Agents["worker"], Task: taskDefFromTodoItem(item), Prompt: item.Goal, ExecutionTarget: item.ExecutionTarget, ModelID: item.ExecutionTarget.Model}
	hash, err := codexImageProjectionHash(item.MediaBinding, item.ExecutionTarget.String())
	if err != nil {
		t.Fatal(err)
	}
	return c, ctx, request, hash
}

func TestCodexImageProjectionRebuildsStablePathsAndRejectsTampering(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	for _, scenario := range []string{"missing", "matching", "changed", "writable", "symlink", "parent symlink"} {
		t.Run(scenario, func(t *testing.T) {
			c, ctx, request, hash := codexImageProjectionFixture(t)
			ref := request.Task.MediaBinding.Refs[0]
			directory := filepath.Join(c.artifactStoreRootPath(), logsDir, "media-projections", "input-"+strings.TrimPrefix(hash, "sha256:"))
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, codexImageFilename(ref))
			switch scenario {
			case "matching":
				if err := os.WriteFile(path, mediaStorePNG(t), 0o400); err != nil {
					t.Fatal(err)
				}
			case "changed":
				if err := os.WriteFile(path, []byte("changed"), 0o400); err != nil {
					t.Fatal(err)
				}
			case "writable":
				if err := os.WriteFile(path, mediaStorePNG(t), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(filepath.Join(c.session.Workspace, "input.png"), path); err != nil {
					t.Fatal(err)
				}
			case "parent symlink":
				if err := os.Remove(directory); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(mediaStoreDirectory(t), directory); err != nil {
					t.Fatal(err)
				}
			}
			projection, err := newCodexImageProjection(ctx, c, request, hash)
			if scenario != "missing" && scenario != "matching" {
				if err == nil {
					projection.Close()
					t.Fatal("unsafe surviving projection accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			inputs := projection.Inputs(request.Prompt)
			if len(inputs) != 3 || inputs[1].Path != path || inputs[2].Path != path {
				projection.Close()
				t.Fatal("rebuild changed stable path or ordered repeats")
			}
			if err := projection.Verify(ctx, media.Limits{}); err != nil {
				projection.Close()
				t.Fatal(err)
			}
			projection.Close()
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatal("projection bytes not reclaimed")
			}
		})
	}
}

func TestCodexImageProjectionRechecksRevokedLease(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	c, ctx, request, hash := codexImageProjectionFixture(t)
	if err := c.CommitTaskTransition(ctx, request.TaskID, TaskInProgress, TaskVerifying, "revoke", "", nil); err != nil {
		t.Fatal(err)
	}
	if projection, err := newCodexImageProjection(ctx, c, request, hash); err == nil {
		projection.Close()
		t.Fatal("revoked lease acquired CAS bytes")
	}
}

func TestCodexImageProjectionRejectsJournalRevocationWithStaleMemory(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	c, ctx, request, hash := codexImageProjectionFixture(t)
	item := c.todoItemByID(request.TaskID)
	payload := taskTransitionPayload(item)
	payload["occurrence_revision"] = item.OccurrenceRevision + 1
	payload["dispatch_id"] = ""
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.EventJournal().Append(ctx, RunEvent{Type: string(EventTaskCancelled), TaskID: item.ID, Actor: "coordinator", Payload: encoded}); err != nil {
		t.Fatal(err)
	}
	if c.todoItemByID(item.ID).Status != TaskInProgress {
		t.Fatal("fixture did not retain stale local state")
	}
	if projection, err := newCodexImageProjection(ctx, c, request, hash); err == nil {
		projection.Close()
		t.Fatal("journal-revoked lease read canonical image bytes")
	}
}

func TestCodexImageProjectionSharesCopiesUntilLastConsumer(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	c, ctx, request, hash := codexImageProjectionFixture(t)
	first, err := newCodexImageProjection(ctx, c, request, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	secondCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	second, err := newCodexImageProjection(secondCtx, c, request, hash)
	if err != nil {
		t.Fatalf("independent consumer blocked behind an active projection: %v", err)
	}
	defer second.Close()
	if first.path != second.path {
		t.Fatal("same binding did not share stable projection paths")
	}
	first.Close()
	first.Close()
	if err := second.Verify(ctx, media.Limits{}); err != nil {
		t.Fatalf("first consumer reclaimed another consumer's image: %v", err)
	}
	path := second.Inputs("")[1].Path
	second.Close()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("last consumer did not reclaim image: %v", err)
	}
	third, err := newCodexImageProjection(ctx, c, request, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	if err := third.Verify(ctx, media.Limits{}); err != nil {
		t.Fatal(err)
	}
}

func TestCodexImageProjectionCopyQuotaWhileConsumersAreActive(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	for _, scenario := range []string{"fits", "blob limit", "byte limit"} {
		t.Run(scenario, func(t *testing.T) {
			size := int64(len(mediaStorePNG(t)))
			c, ctx, request, hash := codexImageProjectionFixture(t, func(session *TeamSession) {
				session.MediaPolicy.Image.MaxBranchBlobs = 2
				session.MediaPolicy.Image.MaxBranchBytes = 2 * size
				if scenario == "blob limit" {
					session.MediaPolicy.Image.MaxBranchBlobs = 1
				}
				if scenario == "byte limit" {
					session.MediaPolicy.Image.MaxBranchBytes = size
				}
			})
			first, err := newCodexImageProjection(ctx, c, request, hash)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Close()
			task := admittedMediaTask(t, c, ctx, "independent-turn")
			_, secondCtx, secondRequest, secondHash := codexImageProjectionRequest(t, c, ctx, task)
			if request.TaskID == secondRequest.TaskID || hash == secondHash {
				t.Fatal("fixture did not create independent task/binding")
			}
			second, err := newCodexImageProjection(secondCtx, c, secondRequest, secondHash)
			if scenario == "fits" {
				if err != nil {
					t.Fatal(err)
				}
				defer second.Close()
			} else {
				if second != nil {
					second.Close()
				}
				requireMediaCode(t, err, media.LimitExceeded)
			}
			if err := first.Verify(ctx, media.Limits{}); err != nil {
				t.Fatal("quota admission damaged the active consumer", err)
			}
			first.Close()
			if second != nil {
				if err := second.Verify(secondCtx, media.Limits{}); err != nil {
					t.Fatal(err)
				}
				second.Close()
			}
			// Rejection did not poison the lease or reserve unavailable copy space.
			again, err := newCodexImageProjection(secondCtx, c, secondRequest, secondHash)
			if err != nil {
				t.Fatal(err)
			}
			again.Close()
		})
	}
}

func TestCodexImageProjectionCancelledWaitPreservesConsumer(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	c, ctx, request, hash := codexImageProjectionFixture(t)
	first, err := newCodexImageProjection(ctx, c, request, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	unlock, err := acquireCodexStateFileLock(ctx, filepath.Join(c.artifactStoreRootPath(), logsDir, "media-projections", codexProjectionLockFile))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	waiting, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	second, err := newCodexImageProjection(waiting, c, request, hash)
	if second != nil {
		second.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait did not respect deadline: %v", err)
	}
	unlock()
	if err := first.Verify(ctx, media.Limits{}); err != nil {
		t.Fatal(err)
	}
	first.Close()
	third, err := newCodexImageProjection(ctx, c, request, hash)
	if err != nil {
		t.Fatal(err)
	}
	third.Close()
}

func TestCodexImageProjectionRetainsCopiesWhenCleanupIsContended(t *testing.T) {
	if testing.Short() {
		t.Skip(unsupportedCodexIntegrationReason)
	}
	skipUnsupportedCodexIntegration(t)
	c, ctx, request, hash := codexImageProjectionFixture(t)
	first, err := newCodexImageProjection(ctx, c, request, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	path := first.Inputs("")[1].Path
	unlock, err := acquireCodexStateFileLock(ctx, filepath.Join(c.artifactStoreRootPath(), logsDir, "media-projections", codexProjectionLockFile))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	cleanupCtx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	first.close(cleanupCtx) // Bounded cleanup cannot delete outside the quota lock.
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("contended cleanup lost retained copies", err)
	}
	unlock()
	next, err := newCodexImageProjection(ctx, c, request, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if err := next.Verify(ctx, media.Limits{}); err != nil {
		t.Fatal(err)
	}
	next.Close()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("retained copy was not reclaimed after reuse: %v", err)
	}
}

func TestCodexImageProjectionConcurrentQuotaAdmission(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	c, ctx, request, hash := codexImageProjectionFixture(t, func(session *TeamSession) { session.MediaPolicy.Image.MaxBranchBlobs = 1 })
	_, otherCtx, otherRequest, otherHash := codexImageProjectionRequest(t, c, ctx, admittedMediaTask(t, c, ctx, "competing-turn"))
	requests := []AttemptRequest{request, otherRequest}
	contexts := []context.Context{ctx, otherCtx}
	hashes := []string{hash, otherHash}
	type outcome struct {
		index      int
		projection *codexImageProjection
		err        error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := range 2 {
		workers.Go(func() {
			<-start
			bounded, cancel := context.WithTimeout(contexts[i], 5*time.Second)
			defer cancel()
			projection, err := newCodexImageProjection(bounded, c, requests[i], hashes[i])
			results <- outcome{i, projection, err}
		})
	}
	close(start)
	workers.Wait()
	close(results)
	winner, loser := -1, -1
	for result := range results {
		if result.err == nil {
			if winner != -1 {
				result.projection.Close()
				t.Fatal("concurrent publication exceeded the one-copy quota")
			}
			winner = result.index
			defer result.projection.Close()
			if err := result.projection.Verify(ctx, media.Limits{}); err != nil {
				t.Fatal(err)
			}
		} else {
			requireMediaCode(t, result.err, media.LimitExceeded)
			loser = result.index
		}
	}
	if winner < 0 || loser < 0 {
		t.Fatalf("quota did not admit exactly one producer: winner=%d loser=%d", winner, loser)
	}
}
