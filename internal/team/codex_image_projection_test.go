package team

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/media"
)

func codexImageProjectionFixture(t *testing.T) (*Coordinator, context.Context, AttemptRequest, string) {
	t.Helper()
	c, _ := newCodexImageCoordinator(t, codexImageProtocolVersion, func(string) []fakeCodexStep { return nil })
	ctx := openMediaInvocation(t, c)
	task := nativeInvocationTask(t, c, ctx, media.InvocationSingleTask)
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
