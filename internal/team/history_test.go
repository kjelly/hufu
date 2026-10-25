package team

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/runtimestore"
)

func TestCancelledHistoryCompactionPreservesInputWithoutProviderWork(t *testing.T) {
	for _, name := range []string{"cancelled_context", "sealed_run"} {
		t.Run(name, func(t *testing.T) {
			c := newTestCoordinator(t)
			messages := []fantasy.Message{
				fantasy.NewUserMessage("original user request"),
				fantasy.NewUserMessage("required verification evidence"),
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if name == "cancelled_context" {
				cancel()
			} else {
				c.SetLastRunResult(&RunResult{RunID: c.executionRunID, Outcome: RunOutcomeCancelled})
				c.finishCalled.Store(true)
			}
			projection := c.buildCompactionProjection(ctx, messages, 0, []int{1, 1}, nil, &mockCompacter{err: errors.New("provider work must not start after cancellation")})
			if !errors.Is(projection.err, context.Canceled) || projection.summary != nil || projection.tokensBefore != 0 || !reflect.DeepEqual(projection.messages, messages) {
				t.Fatalf("cancelled compaction performed work or replaced evidence: %#v", projection)
			}
		})
	}
}

func TestPriorCancelledRunDoesNotSuppressCompaction(t *testing.T) {
	c := newTestCoordinator(t)
	c.SetLastRunResult(&RunResult{RunID: "previous-run", Outcome: RunOutcomeCancelled})
	c.finishCalled.Store(true)
	c.executionRunID = "new-run"
	messages := []fantasy.Message{fantasy.NewUserMessage("new request")}
	projection := c.buildCompactionProjection(t.Context(), messages, 0, []int{1}, nil, &mockCompacter{})
	if errors.Is(projection.err, context.Canceled) || projection.tokensBefore == 0 {
		t.Fatal("a prior run's cancellation suppressed new-run compaction")
	}
}

func TestSaveAndLoadConversationHistory(t *testing.T) {
	dir := t.TempDir()

	messages := []fantasy.Message{
		fantasy.NewUserMessage("hello"),
		fantasy.NewSystemMessage("system prompt"),
		fantasy.NewUserMessage("do something"),
	}

	if err := SaveConversationHistory(dir, messages); err != nil {
		t.Fatalf("SaveConversationHistory failed: %v", err)
	}

	loaded := LoadConversationHistory(dir)
	if len(loaded) != len(messages) {
		t.Fatalf("expected %d messages, got %d", len(messages), len(loaded))
	}

	for i, msg := range loaded {
		if msg.Role != messages[i].Role {
			t.Errorf("message %d: expected role %q, got %q", i, messages[i].Role, msg.Role)
		}
	}
}

func TestLoadConversationHistoryMissing(t *testing.T) {
	dir := t.TempDir()
	loaded := LoadConversationHistory(dir)
	if loaded != nil {
		t.Fatalf("expected nil for missing file, got %v", loaded)
	}
}

func TestLoadConversationHistoryCorrupt(t *testing.T) {
	dir := t.TempDir()
	putRuntimeDocument(t, dir, runtimestore.DocumentSessionHistory, []byte("not valid json{}"))

	loaded := LoadConversationHistory(dir)
	if loaded != nil {
		t.Fatalf("expected nil for corrupt file, got %v", loaded)
	}
}

func TestDeleteConversationHistory(t *testing.T) {
	dir := t.TempDir()

	SaveConversationHistory(dir, []fantasy.Message{fantasy.NewUserMessage("test")})
	if !HasConversationHistory(dir) {
		t.Fatal("expected history file to exist")
	}

	if err := DeleteConversationHistory(dir); err != nil {
		t.Fatalf("DeleteConversationHistory failed: %v", err)
	}
	if HasConversationHistory(dir) {
		t.Fatal("expected history file to be deleted")
	}

	if err := DeleteConversationHistory(dir); err != nil {
		t.Fatalf("DeleteConversationHistory on missing file should not error: %v", err)
	}
}

func TestFilterMessagesTruncatesOversized(t *testing.T) {
	big := strings.Repeat("x", maxMessageSize+1000)
	filtered := filterMessages([]fantasy.Message{fantasy.NewUserMessage(big)})
	if len(filtered) != 1 {
		t.Fatalf("oversized message must be kept (truncated), got %d messages", len(filtered))
	}
	size := messageTextSize(filtered[0])
	if size > maxMessageSize {
		t.Fatalf("truncated message size = %d, want <= %d", size, maxMessageSize)
	}
	if size == 0 {
		t.Fatal("truncated message lost all content")
	}

	smallMsg := fantasy.NewUserMessage("ok")
	filtered = filterMessages([]fantasy.Message{smallMsg})
	if len(filtered) != 1 || messageTextSize(filtered[0]) != 2 {
		t.Fatalf("small message must pass through unchanged")
	}
}
