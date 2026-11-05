package team

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/media"
)

func TestDirectMediaInvocationRejectsUnknownImageCapabilityWithCanonicalEvidence(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	path := filepath.Join(c.projectDir, "input.png")
	if err := os.WriteFile(path, mediaStorePNG(t), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := c.RunDirectAgentWithMedia(t.Context(), "worker", "describe this image", MediaTurnRequest{Images: []string{path, path}})
	// Unknown model image support must fail before starting a provider boundary.
	requireMediaCode(t, err, media.Unsupported)
	items := c.taskTracker.TodoList().Items()
	if len(items) != 1 || items[0].MediaBinding == nil || items[0].MediaBinding.InvocationMode != media.InvocationDirectAgent ||
		len(items[0].MediaBinding.Selections) != 2 || !isTerminalTaskStatus(items[0].Status) || items[0].Status == TaskDone || c.LastRunResult() == nil {
		t.Fatalf("direct media bypassed canonical ownership or run finalization: items=%+v result=%+v err=%v", items, c.LastRunResult(), err)
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
		t.Fatal(err)
	}
	replayed := todoItemByID(ReduceToTodoList(events), items[0].ID)
	if replayed == nil || replayed.MediaBinding == nil || replayed.MediaBinding.Hash != items[0].MediaBinding.Hash {
		t.Fatal("direct media lost its canonical binding during replay")
	}
}

func TestDirectMediaInvocationPreCancelledRequestDoesNotIngestImages(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := c.RunDirectAgentWithMedia(ctx, "worker", "describe image", MediaTurnRequest{Images: []string{filepath.Join(c.projectDir, "missing.png")}})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("pre-cancelled image invocation lost its original cancellation", err)
	}
	items := c.taskTracker.TodoList().Items()
	if len(items) != 1 || items[0].MediaBinding != nil || items[0].MediaInputDigest != "" {
		t.Fatal("pre-cancelled direct request ingested images or lost its task identity")
	}
	assertNoDirectMediaInputEvents(t, c)
}

func TestDirectMediaInvocationRejectsEmptyBundleBeforeTaskCreation(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	_, err := c.RunDirectAgentWithMedia(t.Context(), "worker", "describe image", MediaTurnRequest{})
	requireMediaCode(t, err, media.BindingConflict)
	if len(c.taskTracker.TodoList().Items()) != 0 {
		t.Fatal("invalid direct image bundle became executable work")
	}
	assertNoDirectMediaInputEvents(t, c)
}

func assertNoDirectMediaInputEvents(t *testing.T, c *Coordinator) {
	t.Helper()
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == string(EventMediaInputAdmitted) || event.Type == string(EventTaskMediaBound) || event.Type == string(EventMediaProviderAdmitted) {
			t.Fatal("request ingested or authorized media before validation/cancellation")
		}
	}
}
