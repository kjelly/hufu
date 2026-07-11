package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"time"

	"github.com/anomalyco/hufu/internal/utils"
)

// TestTruncateString tests the truncateString function
func TestTruncateString(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxRunes int
		want     string
	}{
		{
			name:     "short string unchanged",
			input:    "hello",
			maxRunes: 10,
			want:     "hello",
		},
		{
			name:     "exact length unchanged",
			input:    "hello",
			maxRunes: 5,
			want:     "hello",
		},
		{
			name:     "long string truncated with ellipsis",
			input:    "hello world this is a long string",
			maxRunes: 10,
			want:     "hello worl...",
		},
		{
			name:     "empty string",
			input:    "",
			maxRunes: 10,
			want:     "",
		},
		{
			name:     "unicode characters counted correctly",
			input:    "你好世界",
			maxRunes: 2,
			want:     "你好...",
		},
		{
			name:     "zero maxRunes",
			input:    "hello",
			maxRunes: 0,
			want:     "...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := utils.TruncateRunes(tt.input, tt.maxRunes)
			if got != tt.want {
				t.Errorf("utils.TruncateRunes(%q, %d) = %q, want %q", tt.input, tt.maxRunes, got, tt.want)
			}
		})
	}
}

// TestRemoveFileIfExists tests the removeFileIfExists function
func TestRemoveFileIfExists(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T) string
		wantErr bool
	}{
		{
			name: "remove existing file",
			setup: func(t *testing.T) string {
				tmpFile := filepath.Join(t.TempDir(), "test.txt")
				if err := os.WriteFile(tmpFile, []byte("content"), 0o644); err != nil {
					t.Fatalf("failed to create test file: %v", err)
				}
				return tmpFile
			},
			wantErr: false,
		},
		{
			name: "remove nonexistent file returns nil",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "nonexistent.txt")
			},
			wantErr: false,
		},
		{
			name: "remove from nonexistent directory returns nil (idempotent)",
			setup: func(t *testing.T) string {
				return "/nonexistent/directory/test.txt"
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.setup(t)
			err := removeFileIfExists(path)
			if (err != nil) != tt.wantErr {
				t.Errorf("removeFileIfExists() error = %v, wantErr %v", err, tt.wantErr)
			}

			if !tt.wantErr {
				if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
					t.Errorf("file should be removed after removeFileIfExists()")
				}
			}
		})
	}
}

// TestSaveSession tests the SaveSession function
func TestSaveSession(t *testing.T) {
	tests := []struct {
		name    string
		session *SessionData
		wantErr bool
	}{
		{
			name: "save valid session",
			session: &SessionData{
				CreatedAt: "2024-01-01T00:00:00Z",
				UpdatedAt: "2024-01-01T00:00:00Z",
				Rounds:    5,
				Entries: []SessionEntry{
					{Role: "user", Content: "Hello"},
					{Role: "assistant", Content: "Hi"},
				},
			},
			wantErr: false,
		},
		{
			name: "save session with empty entries",
			session: &SessionData{
				CreatedAt: "2024-01-01T00:00:00Z",
				UpdatedAt: "2024-01-01T00:00:00Z",
				Rounds:    0,
				Entries:   []SessionEntry{},
			},
			wantErr: false,
		},
		{
			name:    "save nil session panics or errors",
			session: nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			err := SaveSession(tmpDir, tt.session)

			if tt.session == nil {
				// Nil session should cause an error or panic
				if err == nil {
					t.Error("SaveSession with nil session should error")
				}
				return
			}

			if (err != nil) != tt.wantErr {
				t.Errorf("SaveSession() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				// Verify file was created
				sessionPath := filepath.Join(tmpDir, sessionFile)
				if _, statErr := os.Stat(sessionPath); os.IsNotExist(statErr) {
					t.Errorf("SaveSession() did not create session file")
				}

				// Verify UpdatedAt was set
				loaded := LoadSession(tmpDir)
				if loaded == nil {
					t.Fatal("failed to load saved session")
				}
				if loaded.UpdatedAt == "" {
					t.Error("SaveSession() should set UpdatedAt")
				}
			}
		})
	}
}

// TestNewSession tests the NewSession function
func TestNewSession(t *testing.T) {
	session := NewSession()

	if session == nil {
		t.Fatal("NewSession() returned nil")
	}

	if session.CreatedAt == "" {
		t.Error("NewSession() should set CreatedAt")
	}

	if session.UpdatedAt == "" {
		t.Error("NewSession() should set UpdatedAt")
	}

	if session.Rounds != 0 {
		t.Errorf("NewSession() Rounds = %d, want 0", session.Rounds)
	}

	if session.Entries == nil {
		t.Error("NewSession() Entries should not be nil")
	}

	if len(session.Entries) != 0 {
		t.Errorf("NewSession() Entries length = %d, want 0", len(session.Entries))
	}

	// Verify timestamps are valid RFC3339
	if _, err := time.Parse(time.RFC3339, session.CreatedAt); err != nil {
		t.Errorf("CreatedAt is not valid RFC3339: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, session.UpdatedAt); err != nil {
		t.Errorf("UpdatedAt is not valid RFC3339: %v", err)
	}
}

// TestSessionDataAddEntry tests the AddEntry method
func TestSessionDataAddEntry(t *testing.T) {
	session := &SessionData{
		CreatedAt: "2024-01-01T00:00:00Z",
		UpdatedAt: "2024-01-01T00:00:00Z",
		Rounds:    0,
		Entries:   []SessionEntry{},
	}

	session.AddEntry("user", "Hello")
	session.AddEntry("assistant", "Hi there!")

	if len(session.Entries) != 2 {
		t.Errorf("AddEntry() should add entries, got %d", len(session.Entries))
	}

	if session.Entries[0].Role != "user" {
		t.Errorf("first entry role = %q, want %q", session.Entries[0].Role, "user")
	}

	if session.Entries[0].Content != "Hello" {
		t.Errorf("first entry content = %q, want %q", session.Entries[0].Content, "Hello")
	}

	if session.Entries[1].Role != "assistant" {
		t.Errorf("second entry role = %q, want %q", session.Entries[1].Role, "assistant")
	}

	// Verify timestamp was set
	if session.Entries[0].Timestamp == "" {
		t.Error("AddEntry() should set timestamp")
	}

	// Verify timestamp is valid RFC3339
	if _, err := time.Parse(time.RFC3339, session.Entries[0].Timestamp); err != nil {
		t.Errorf("timestamp is not valid RFC3339: %v", err)
	}
}

func TestSessionDataAddEntrySkipsEmptyAndDuplicates(t *testing.T) {
	cases := []struct {
		name    string
		entries [][2]string // role, content pairs added in order
		want    int         // expected recorded entry count
	}{
		{"empty content skipped", [][2]string{{"user", "  "}}, 0},
		{"consecutive duplicate user skipped", [][2]string{{"user", "run it"}, {"user", "run it"}}, 1},
		{"same content different role kept", [][2]string{{"user", "done"}, {"assistant", "done"}}, 2},
		{"same content after reply kept", [][2]string{{"user", "again"}, {"assistant", "ok"}, {"user", "again"}}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session := NewSession()
			for _, e := range tc.entries {
				session.AddEntry(e[0], e[1])
			}
			if len(session.Entries) != tc.want {
				t.Errorf("recorded %d entries, want %d", len(session.Entries), tc.want)
			}
		})
	}
}

// TestSessionDataContextSummary tests the ContextSummary method
func TestSessionDataContextSummary(t *testing.T) {
	tests := []struct {
		name        string
		session     *SessionData
		wantEmpty   bool
		wantContent string
	}{
		{
			name: "empty entries returns empty string",
			session: &SessionData{
				CreatedAt: "2024-01-01T00:00:00Z",
				UpdatedAt: "2024-01-01T00:00:00Z",
				Rounds:    0,
				Entries:   []SessionEntry{},
			},
			wantEmpty: true,
		},
		{
			name: "single entry summary",
			session: &SessionData{
				CreatedAt: "2024-01-01T00:00:00Z",
				UpdatedAt: "2024-01-01T00:00:00Z",
				Rounds:    5,
				Entries: []SessionEntry{
					{Role: "user", Content: "Hello"},
				},
			},
			wantContent: "1 exchanges",
		},
		{
			name: "multiple entries with truncation",
			session: &SessionData{
				CreatedAt: "2024-01-01T00:00:00Z",
				UpdatedAt: "2024-01-01T00:00:00Z",
				Rounds:    10,
				Entries: func() []SessionEntry {
					entries := make([]SessionEntry, 50)
					for i := 0; i < 50; i++ {
						entries[i] = SessionEntry{
							Role:    "user",
							Content: "Message " + string(rune('A'+i)),
						}
					}
					return entries
				}(),
			},
			wantContent: "50 exchanges",
		},
		{
			name: "long content truncated",
			session: &SessionData{
				CreatedAt: "2024-01-01T00:00:00Z",
				UpdatedAt: "2024-01-01T00:00:00Z",
				Rounds:    1,
				Entries: []SessionEntry{
					{Role: "user", Content: strings.Repeat("x", 1000)},
				},
			},
			wantContent: "...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary := tt.session.ContextSummary()

			if tt.wantEmpty && summary != "" {
				t.Errorf("ContextSummary() = %q, want empty string", summary)
			}

			if !tt.wantEmpty && tt.wantContent != "" {
				if !strings.Contains(summary, tt.wantContent) {
					t.Errorf("ContextSummary() should contain %q, got %q", tt.wantContent, summary)
				}
			}

			if !tt.wantEmpty {
				if !strings.Contains(summary, "Previous session context") {
					t.Error("ContextSummary() should contain header")
				}
				if !strings.Contains(summary, "rounds") {
					t.Error("ContextSummary() should mention rounds")
				}
			}
		})
	}
}

// TestArchiveSession tests the ArchiveSession function
func TestArchiveSession(t *testing.T) {
	tests := []struct {
		name    string
		summary string
		wantErr bool
		check   func(t *testing.T, workspace string)
	}{
		{
			name:    "archive with simple summary",
			summary: "Session completed successfully",
			wantErr: false,
			check: func(t *testing.T, workspace string) {
				histDir := filepath.Join(workspace, historyDirName)
				entries, err := os.ReadDir(histDir)
				if err != nil {
					t.Fatalf("failed to read history dir: %v", err)
				}
				if len(entries) != 1 {
					t.Errorf("expected 1 archived file, got %d", len(entries))
				}
			},
		},
		{
			name:    "archive with markdown header",
			summary: "# Task Completed\n\nDetails here",
			wantErr: false,
			check: func(t *testing.T, workspace string) {
				histDir := filepath.Join(workspace, historyDirName)
				entries, err := os.ReadDir(histDir)
				if err != nil {
					t.Fatalf("failed to read history dir: %v", err)
				}
				// Filename should contain "task-completed"
				found := false
				for _, e := range entries {
					if strings.Contains(e.Name(), "task-completed") {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected filename to contain 'task-completed', got %v", entries)
				}
			},
		},
		{
			name:    "archive with long title truncated",
			summary: "# This is a very long title that should be truncated to fit within the filename limit properly",
			wantErr: false,
			check: func(t *testing.T, workspace string) {
				histDir := filepath.Join(workspace, historyDirName)
				entries, err := os.ReadDir(histDir)
				if err != nil {
					t.Fatalf("failed to read history dir: %v", err)
				}
				if len(entries) != 1 {
					t.Errorf("expected 1 archived file, got %d", len(entries))
				}
				// Filename should be <= reasonable length
				if len(entries[0].Name()) > 60 {
					t.Errorf("filename too long: %s", entries[0].Name())
				}
			},
		},
		{
			name:    "archive removes session.json",
			summary: "Session done",
			wantErr: false,
			check: func(t *testing.T, workspace string) {
				sessionPath := filepath.Join(workspace, sessionFile)
				if _, err := os.Stat(sessionPath); !os.IsNotExist(err) {
					t.Error("ArchiveSession should remove session.json")
				}
			},
		},
		{
			name:    "archive to nonexistent history dir creates it",
			summary: "Test",
			wantErr: false,
			check: func(t *testing.T, workspace string) {
				histDir := filepath.Join(workspace, historyDirName)
				if _, err := os.Stat(histDir); os.IsNotExist(err) {
					t.Error("ArchiveSession should create history directory")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			// Create session.json first
			sessionPath := filepath.Join(tmpDir, sessionFile)
			if err := os.WriteFile(sessionPath, []byte(`{"rounds":1}`), 0o644); err != nil {
				t.Fatalf("failed to create session file: %v", err)
			}

			err := ArchiveSession(tmpDir, tt.summary)
			if (err != nil) != tt.wantErr {
				t.Errorf("ArchiveSession() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && tt.check != nil {
				tt.check(t, tmpDir)
			}
		})
	}
}

// TestHasSession tests the HasSession function
func TestHasSession(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) string
		want  bool
	}{
		{
			name: "session file exists",
			setup: func(t *testing.T) string {
				tmpDir := t.TempDir()
				sessionPath := filepath.Join(tmpDir, sessionFile)
				if err := os.WriteFile(sessionPath, []byte(`{"rounds":1}`), 0o644); err != nil {
					t.Fatalf("failed to create session file: %v", err)
				}
				return tmpDir
			},
			want: true,
		},
		{
			name: "session file does not exist",
			setup: func(t *testing.T) string {
				return t.TempDir()
			},
			want: false,
		},
		{
			name: "session file is a directory",
			setup: func(t *testing.T) string {
				tmpDir := t.TempDir()
				sessionPath := filepath.Join(tmpDir, sessionFile)
				if err := os.MkdirAll(sessionPath, 0o755); err != nil {
					t.Fatalf("failed to create directory: %v", err)
				}
				return tmpDir
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := tt.setup(t)
			got := HasSession(workspace)
			if got != tt.want {
				t.Errorf("HasSession() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSessionDataSerialization tests JSON serialization round-trip
func TestSessionDataSerialization(t *testing.T) {
	original := &SessionData{
		CreatedAt: "2024-01-01T00:00:00Z",
		UpdatedAt: "2024-01-02T00:00:00Z",
		Rounds:    10,
		Entries: []SessionEntry{
			{Role: "user", Content: "Hello", Timestamp: "2024-01-01T00:00:00Z"},
			{Role: "assistant", Content: "Hi there!", Timestamp: "2024-01-01T00:00:01Z"},
			{Role: "system", Content: "System message", Timestamp: "2024-01-01T00:00:02Z"},
		},
	}

	tmpDir := t.TempDir()
	err := SaveSession(tmpDir, original)
	if err != nil {
		t.Fatalf("SaveSession() error = %v", err)
	}

	loaded := LoadSession(tmpDir)
	if loaded == nil {
		t.Fatal("LoadSession() returned nil")
	}

	if loaded.CreatedAt != original.CreatedAt {
		t.Errorf("CreatedAt = %q, want %q", loaded.CreatedAt, original.CreatedAt)
	}

	if loaded.UpdatedAt != original.UpdatedAt {
		t.Errorf("UpdatedAt = %q, want %q", loaded.UpdatedAt, original.UpdatedAt)
	}

	if loaded.Rounds != original.Rounds {
		t.Errorf("Rounds = %d, want %d", loaded.Rounds, original.Rounds)
	}

	if len(loaded.Entries) != len(original.Entries) {
		t.Errorf("Entries length = %d, want %d", len(loaded.Entries), len(original.Entries))
	}

	for i, entry := range loaded.Entries {
		if entry.Role != original.Entries[i].Role {
			t.Errorf("entry %d role = %q, want %q", i, entry.Role, original.Entries[i].Role)
		}
		if entry.Content != original.Entries[i].Content {
			t.Errorf("entry %d content = %q, want %q", i, entry.Content, original.Entries[i].Content)
		}
	}
}

// TestSessionEntryWithOptionalTimestamp tests that timestamp is optional in JSON
func TestSessionEntryWithOptionalTimestamp(t *testing.T) {
	jsonData := `{"role": "user", "content": "Hello"}`
	tmpDir := t.TempDir()
	sessionPath := filepath.Join(tmpDir, sessionFile)

	sessionJSON := `{
		"created_at": "2024-01-01T00:00:00Z",
		"updated_at": "2024-01-01T00:00:00Z",
		"rounds": 1,
		"entries": [` + jsonData + `]
	}`

	if err := os.WriteFile(sessionPath, []byte(sessionJSON), 0o644); err != nil {
		t.Fatalf("failed to write session file: %v", err)
	}

	loaded := LoadSession(tmpDir)
	if loaded == nil {
		t.Fatal("LoadSession() returned nil")
	}

	if len(loaded.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(loaded.Entries))
	}

	// Timestamp should be empty when not provided
	if loaded.Entries[0].Timestamp != "" {
		t.Errorf("Timestamp = %q, want empty string", loaded.Entries[0].Timestamp)
	}
}

// TestArchiveSessionSlugGeneration tests the slug generation for archive filenames
func TestArchiveSessionSlugGeneration(t *testing.T) {
	tests := []struct {
		name          string
		summary       string
		wantSubstring string
	}{
		{
			name:          "simple title",
			summary:       "# Code Review\n\nCompleted",
			wantSubstring: "code-review",
		},
		{
			name:          "title with slashes",
			summary:       "# Fix bug in api/users\n\nDone",
			wantSubstring: "fix-bug-in-api-users",
		},
		{
			name:          "title with special chars",
			summary:       "# Task: Review & Fix!\n\nDone",
			wantSubstring: "task-review-fix",
		},
		{
			name:          "no markdown header uses default",
			summary:       "Just plain text",
			wantSubstring: "session",
		},
		{
			name:          "empty header uses default",
			summary:       "# \n\nContent",
			wantSubstring: "session",
		},
		{
			name:          "whitespace-only header uses default",
			summary:       "#    \n\nContent",
			wantSubstring: "session",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			sessionPath := filepath.Join(tmpDir, sessionFile)
			if err := os.WriteFile(sessionPath, []byte(`{"rounds":1}`), 0o644); err != nil {
				t.Fatalf("failed to create session file: %v", err)
			}

			err := ArchiveSession(tmpDir, tt.summary)
			if err != nil {
				t.Fatalf("ArchiveSession() error = %v", err)
			}

			histDir := filepath.Join(tmpDir, historyDirName)
			entries, err := os.ReadDir(histDir)
			if err != nil {
				t.Fatalf("failed to read history dir: %v", err)
			}

			if len(entries) != 1 {
				t.Fatalf("expected 1 archived file, got %d", len(entries))
			}

			filename := entries[0].Name()
			if !strings.Contains(filename, tt.wantSubstring) {
				t.Errorf("filename %q should contain %q", filename, tt.wantSubstring)
			}

			// Verify filename format: YYYY-MM-DD-slug.md
			if !strings.HasSuffix(filename, ".md") {
				t.Errorf("filename should end with .md: %s", filename)
			}
		})
	}
}
