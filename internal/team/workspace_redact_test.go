package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactWorkspaceManagedRecords(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, logsDir, "audit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, sessionFile), []byte(`{"entries":[{"content":"api_token: leaked-value"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, logsDir, taskJournalFile), []byte(`{"output":"password=leaked-value"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewEventStore(ws, "run", "session")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(RunEvent{Type: "user_message_added", Actor: "user", Payload: []byte(`{"content":"secret=leaked-value"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	if err := RedactWorkspaceManagedRecords(ws); err != nil {
		t.Fatalf("RedactWorkspaceManagedRecords: %v", err)
	}
	if err := OpenAndVerifyEventStore(ws); err != nil {
		t.Fatalf("event store hash chain invalid after redaction: %v", err)
	}
	for _, path := range []string{filepath.Join(ws, sessionFile), filepath.Join(ws, logsDir, taskJournalFile), filepath.Join(ws, logsDir, eventStoreFile)} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "leaked-value") {
			t.Fatalf("secret remains in %s", filepath.Base(path))
		}
	}
}
