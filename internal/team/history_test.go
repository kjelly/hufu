package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
)

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
	path := filepath.Join(dir, historyFile)
	os.WriteFile(path, []byte("not valid json{}"), 0o644)

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
