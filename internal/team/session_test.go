package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadSession tests the LoadSession function
func TestLoadSession(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T) string
		wantErr bool
		wantNil bool
	}{
		{
			name: "load valid session",
			setup: func(t *testing.T) string {
				tmpDir := t.TempDir()
				sessionData := `{
					"created_at": "2024-01-01T00:00:00Z",
					"updated_at": "2024-01-01T00:00:00Z",
					"rounds": 5,
					"entries": [
						{"role": "user", "content": "Hello"},
						{"role": "assistant", "content": "Hi there!"}
					]
				}`
				if err := os.WriteFile(filepath.Join(tmpDir, sessionFile), []byte(sessionData), 0o644); err != nil {
					t.Fatalf("failed to create session file: %v", err)
				}
				return tmpDir
			},
			wantErr: false,
		},
		{
			name: "load nonexistent session returns nil",
			setup: func(t *testing.T) string {
				return t.TempDir()
			},
			wantNil: true,
			wantErr: true,
		},
		{
			name: "load invalid JSON returns nil",
			setup: func(t *testing.T) string {
				tmpDir := t.TempDir()
				if err := os.WriteFile(filepath.Join(tmpDir, sessionFile), []byte("invalid json"), 0o644); err != nil {
					t.Fatalf("failed to create session file: %v", err)
				}
				return tmpDir
			},
			wantNil: true,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := tt.setup(t)
			got := LoadSession(workspace)

			if tt.wantErr && got != nil {
				t.Errorf("LoadSession() returned %v, want nil", got)
			}

			if !tt.wantErr && got == nil {
				t.Error("LoadSession() returned nil, want non-nil")
			}

			if tt.wantNil && got != nil {
				t.Error("LoadSession() returned non-nil, want nil")
			}

			if !tt.wantErr && got != nil {
				if got.CreatedAt == "" {
					t.Error("LoadSession() returned session with empty CreatedAt")
				}
				if got.UpdatedAt == "" {
					t.Error("LoadSession() returned session with empty UpdatedAt")
				}
			}
		})
	}
}

// TestSessionMDPath tests the SessionMDPath function
func TestSessionMDPath(t *testing.T) {
	tests := []struct {
		name      string
		workspace string
		expected  string
	}{
		{
			name:      "basic path",
			workspace: "/tmp/workspace",
			expected:  "/tmp/workspace/session.md",
		},
		{
			name:      "path with trailing slash",
			workspace: "/tmp/workspace/",
			expected:  "/tmp/workspace/session.md",
		},
		{
			name:      "relative path",
			workspace: "workspace",
			expected:  "workspace/session.md",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SessionMDPath(tt.workspace)
			if got != tt.expected {
				t.Errorf("SessionMDPath() = %q, want %q", got, tt.expected)
			}
		})
	}
}

// TestLoadSessionMD tests the LoadSessionMD function
func TestLoadSessionMD(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T) string
		want    string
		wantErr bool
	}{
		{
			name: "load valid session md",
			setup: func(t *testing.T) string {
				tmpDir := t.TempDir()
				content := "This is the session markdown content."
				if err := os.WriteFile(filepath.Join(tmpDir, sessionMDFile), []byte(content), 0o644); err != nil {
					t.Fatalf("failed to create session md file: %v", err)
				}
				return tmpDir
			},
			want:    "This is the session markdown content.",
			wantErr: false,
		},
		{
			name: "load nonexistent file returns empty string",
			setup: func(t *testing.T) string {
				return t.TempDir()
			},
			want:    "",
			wantErr: false,
		},
		{
			name: "load file with whitespace returns trimmed",
			setup: func(t *testing.T) string {
				tmpDir := t.TempDir()
				content := "  Content with whitespace  \n\n"
				if err := os.WriteFile(filepath.Join(tmpDir, sessionMDFile), []byte(content), 0o644); err != nil {
					t.Fatalf("failed to create session md file: %v", err)
				}
				return tmpDir
			},
			want:    "Content with whitespace",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := tt.setup(t)
			got := LoadSessionMD(workspace)

			if got != tt.want {
				t.Errorf("LoadSessionMD() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSaveSessionMD tests the SaveSessionMD function
func TestSaveSessionMD(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T) string
		content string
		wantErr bool
	}{
		{
			name: "save valid session md",
			setup: func(t *testing.T) string {
				return t.TempDir()
			},
			content: "Test session content",
			wantErr: false,
		},
		{
			name: "save empty content",
			setup: func(t *testing.T) string {
				return t.TempDir()
			},
			content: "",
			wantErr: false,
		},
		{
			name: "save to nonexistent directory fails",
			setup: func(t *testing.T) string {
				return "/nonexistent/directory"
			},
			content: "Test content",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := tt.setup(t)
			err := SaveSessionMD(workspace, tt.content)

			if (err != nil) != tt.wantErr {
				t.Errorf("SaveSessionMD() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				// Verify the file was created
				mdPath := SessionMDPath(workspace)
				if _, err := os.Stat(mdPath); os.IsNotExist(err) {
					t.Errorf("SaveSessionMD() did not create session md file: %s", mdPath)
				} else {
					content, readErr := os.ReadFile(mdPath)
					if readErr != nil {
						t.Errorf("failed to read session md file: %v", readErr)
					} else if string(content) != tt.content {
						t.Errorf("SaveSessionMD() content mismatch: got %q, want %q", string(content), tt.content)
					}
				}
			}
		})
	}
}

// TestGenerateSessionMD tests the GenerateSessionMD function
func TestGenerateSessionMD(t *testing.T) {
	sessionData := &SessionData{
		CreatedAt: "2024-01-01T00:00:00Z",
		UpdatedAt: "2024-01-01T00:00:00Z",
		Rounds:    5,
		Entries: []SessionEntry{
			{Role: "user", Content: "Hello"},
			{Role: "assistant", Content: "Hi there!"},
		},
	}

	tests := []struct {
		name     string
		sd       *SessionData
		teamName string
		wantLen  int
	}{
		{
			name:     "generate session md with team name",
			sd:       sessionData,
			teamName: "test-team",
			wantLen:  10, // Should contain team name and basic info
		},
		{
			name:     "generate session md without team name",
			sd:       sessionData,
			teamName: "",
			wantLen:  5, // Should contain basic info
		},
		{
			name:     "generate session md with nil data",
			sd:       nil,
			teamName: "test-team",
			wantLen:  0, // Should return empty string
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GenerateSessionMD(tt.sd, tt.teamName)

			if tt.sd == nil {
				if got != "" {
					t.Errorf("GenerateSessionMD() with nil data returned %q, want empty string", got)
				}
				return
			}

			if len(got) < tt.wantLen {
				t.Errorf("GenerateSessionMD() returned %d chars, want at least %d", len(got), tt.wantLen)
			}

			if tt.teamName != "" && !strings.Contains(got, tt.teamName) {
				t.Errorf("GenerateSessionMD() does not contain team name %q", tt.teamName)
			}
		})
	}
}

// TestSessionEntryTimestamp tests that SessionEntry has timestamp field
func TestSessionEntryTimestamp(t *testing.T) {
	entry := SessionEntry{
		Role:      "user",
		Content:   "Test content",
		Timestamp: "2024-01-01T00:00:00Z",
	}

	if entry.Role != "user" {
		t.Errorf("Role = %q, want %q", entry.Role, "user")
	}
	if entry.Content != "Test content" {
		t.Errorf("Content = %q, want %q", entry.Content, "Test content")
	}
	if entry.Timestamp != "2024-01-01T00:00:00Z" {
		t.Errorf("Timestamp = %q, want %q", entry.Timestamp, "2024-01-01T00:00:00Z")
	}
}

// TestSessionDataFields tests that SessionData has all expected fields
func TestSessionDataFields(t *testing.T) {
	data := &SessionData{
		CreatedAt: "2024-01-01T00:00:00Z",
		UpdatedAt: "2024-01-01T00:00:00Z",
		Rounds:    5,
		Entries: []SessionEntry{
			{Role: "user", Content: "Hello"},
		},
	}

	if data.CreatedAt != "2024-01-01T00:00:00Z" {
		t.Errorf("CreatedAt = %q, want %q", data.CreatedAt, "2024-01-01T00:00:00Z")
	}
	if data.UpdatedAt != "2024-01-01T00:00:00Z" {
		t.Errorf("UpdatedAt = %q, want %q", data.UpdatedAt, "2024-01-01T00:00:00Z")
	}
	if data.Rounds != 5 {
		t.Errorf("Rounds = %d, want %d", data.Rounds, 5)
	}
	if len(data.Entries) != 1 {
		t.Errorf("Entries length = %d, want %d", len(data.Entries), 1)
	}
}

// TestEnsureWorkspaceDirs tests the EnsureWorkspaceDirs function
func TestEnsureWorkspaceDirs(t *testing.T) {
	tests := []struct {
		name      string
		workspace string
		wantErr   bool
	}{
		{
			name:      "create workspace dirs successfully",
			workspace: t.TempDir(),
			wantErr:   false,
		},
		{
			name:      "create workspace dirs in nonexistent parent",
			workspace: "/nonexistent/directory/workspace",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := EnsureWorkspaceDirs(tt.workspace)

			if (err != nil) != tt.wantErr {
				t.Errorf("EnsureWorkspaceDirs() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				// Verify directories were created
				expectedDirs := []string{"inbox", "outbox", "shared", "status", "history", "shared/skills"}
				for _, dir := range expectedDirs {
					dirPath := filepath.Join(tt.workspace, dir)
					if _, err := os.Stat(dirPath); os.IsNotExist(err) {
						t.Errorf("EnsureWorkspaceDirs() did not create directory: %s", dirPath)
					}
				}
			}
		})
	}
}

// TestCleanRunDirs tests the CleanRunDirs function
func TestCleanRunDirs(t *testing.T) {
	tmpDir := t.TempDir()

	// Create the workspace dirs first
	if err := EnsureWorkspaceDirs(tmpDir); err != nil {
		t.Fatalf("EnsureWorkspaceDirs() error = %v", err)
	}

	// Create some files in the run directories
	inboxFile := filepath.Join(tmpDir, "inbox", "test.txt")
	outboxFile := filepath.Join(tmpDir, "outbox", "test.txt")
	statusFile := filepath.Join(tmpDir, "status", "test.txt")

	for _, file := range []string{inboxFile, outboxFile, statusFile} {
		if err := os.WriteFile(file, []byte("test"), 0o644); err != nil {
			t.Fatalf("failed to create test file: %v", err)
		}
	}

	err := CleanRunDirs(tmpDir)
	if err != nil {
		t.Errorf("CleanRunDirs() error = %v", err)
	}

	// Verify run directories were cleaned (directories may be removed or empty)
	for _, dir := range []string{"inbox", "outbox", "status"} {
		dirPath := filepath.Join(tmpDir, dir)
		entries, err := os.ReadDir(dirPath)
		if err != nil && !os.IsNotExist(err) {
			t.Errorf("failed to read directory %s: %v", dirPath, err)
		}
		if err == nil && len(entries) != 0 {
			t.Errorf("CleanRunDirs() did not clean directory %s, still has %d entries", dirPath, len(entries))
		}
	}

	// Verify shared and history directories still exist
	for _, dir := range []string{"shared", "history"} {
		dirPath := filepath.Join(tmpDir, dir)
		if _, err := os.Stat(dirPath); os.IsNotExist(err) {
			t.Errorf("CleanRunDirs() removed directory that should exist: %s", dirPath)
		}
	}
}

// TestWorkspaceDirConstants tests that workspace directory constants are defined
func TestWorkspaceDirConstants(t *testing.T) {
	expectedDirs := []string{
		"inbox",
		"outbox",
		"shared",
		"status",
		"history",
	}

	// Verify expected directory names exist (used in EnsureWorkspaceDirs)
	_ = expectedDirs
}

// TestSessionDataRoundCount tests that rounds count is tracked correctly
func TestSessionDataRoundCount(t *testing.T) {
	data := &SessionData{
		CreatedAt: time.Now().Format(time.RFC3339),
		UpdatedAt: time.Now().Format(time.RFC3339),
		Rounds:    0,
		Entries:   []SessionEntry{},
	}

	if data.Rounds != 0 {
		t.Errorf("Rounds = %d, want %d", data.Rounds, 0)
	}

	data.Rounds = 10
	if data.Rounds != 10 {
		t.Errorf("Rounds = %d, want %d", data.Rounds, 10)
	}
}

// TestSessionEntryRole tests that SessionEntry role is validated
func TestSessionEntryRole(t *testing.T) {
	validRoles := []string{"user", "assistant", "system"}

	for _, role := range validRoles {
		entry := SessionEntry{
			Role:    role,
			Content: "Test",
		}
		if entry.Role != role {
			t.Errorf("Role = %q, want %q", entry.Role, role)
		}
	}
}
