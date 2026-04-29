package team

import (
	"os"
	"path/filepath"
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

func TestFilterMessagesSkipsOversized(t *testing.T) {
	msg := fantasy.NewUserMessage(string(make([]byte, maxMessageSize+1)))
	filtered := filterMessages([]fantasy.Message{msg})
	if len(filtered) != 0 {
		t.Fatalf("expected oversized message to be filtered, got %d", len(filtered))
	}

	smallMsg := fantasy.NewUserMessage("ok")
	filtered = filterMessages([]fantasy.Message{smallMsg})
	if len(filtered) != 1 {
		t.Fatalf("expected small message to pass, got %d", len(filtered))
	}
}
