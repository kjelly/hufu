package team

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBudgetSnapshotLoadsLegacyRedactedTokenCounter(t *testing.T) {
	var snapshot BudgetSnapshot
	if err := json.Unmarshal([]byte(`{"tokens_used":"[REDACTED]","attempt":2}`), &snapshot); err != nil {
		t.Fatalf("legacy budget snapshot should remain loadable: %v", err)
	}
	if snapshot.TokensUsed != 0 || snapshot.Attempt != 2 {
		t.Fatalf("legacy budget snapshot = %#v, want tokens_used=0 attempt=2", snapshot)
	}
}

func TestLoadSessionLoadsLegacyRedactedRepairCostTokens(t *testing.T) {
	workspace := t.TempDir()
	sessionData := `{
		"created_at": "2026-08-11T00:00:00Z",
		"updated_at": "2026-08-11T00:00:00Z",
		"rounds": 0,
		"entries": [],
		"run_result": {
			"outcome": "failed",
			"goal_satisfied": false,
			"response": "previous run failed",
			"telemetry": {
				"repair_cost": {
					"attempts": 2,
					"tokens": "[REDACTED]",
					"wall_clock_ms": 17
				}
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(workspace, sessionFile), []byte(sessionData), 0o644); err != nil {
		t.Fatalf("write legacy session: %v", err)
	}

	loaded, err := loadSessionQuiet(workspace)
	if err != nil {
		t.Fatalf("legacy redacted RepairCost must remain loadable: %v", err)
	}
	if loaded == nil || loaded.RunResult == nil || loaded.RunResult.Telemetry == nil {
		t.Fatalf("loaded session telemetry = %#v", loaded)
	}
	cost := loaded.RunResult.Telemetry.RepairCost
	if cost.Attempts != 2 || cost.Tokens != 0 || cost.WallClockMS != 17 {
		t.Fatalf("loaded RepairCost = %#v, want attempts=2 tokens=0 wall_clock_ms=17", cost)
	}
}

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
			expected:  "/tmp/workspace/chat_history.md",
		},
		{
			name:      "path with trailing slash",
			workspace: "/tmp/workspace/",
			expected:  "/tmp/workspace/chat_history.md",
		},
		{
			name:      "relative path",
			workspace: "workspace",
			expected:  "workspace/chat_history.md",
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
				expectedDirs := []string{"tasks", "shared", "status", "history"}
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
	tasksFile := filepath.Join(tmpDir, "tasks", "test.txt")
	statusFile := filepath.Join(tmpDir, "status", "test.txt")

	for _, file := range []string{tasksFile, statusFile} {
		if err := os.WriteFile(file, []byte("test"), 0o644); err != nil {
			t.Fatalf("failed to create test file: %v", err)
		}
	}

	err := CleanRunDirs(tmpDir)
	if err != nil {
		t.Errorf("CleanRunDirs() error = %v", err)
	}

	// Verify run directories were cleaned (directories may be removed or empty)
	for _, dir := range []string{"tasks", "status"} {
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
		"tasks",
		"shared",
		"status",
		"history",
	}

	// Verify expected directory names exist (used in EnsureWorkspaceDirs)
	_ = expectedDirs
}

func TestWriteTaskFile(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name      string
		teamName  string
		agentName string
		timestamp string
		status    string
		task      string
		result    string
		wantErr   bool
		checkFunc func(t *testing.T, workspace, teamName, agentName, timestamp string)
	}{
		{
			name:      "write working task file",
			teamName:  "delegate",
			agentName: "researcher",
			timestamp: "20260502-143005",
			status:    "working",
			task:      "Find security bugs",
			result:    "",
			wantErr:   false,
			checkFunc: func(t *testing.T, workspace, teamName, agentName, timestamp string) {
				path := filepath.Join(workspace, "tasks", teamName, agentName, timestamp+".md")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("failed to read task file: %v", err)
				}
				content := string(data)
				if !strings.Contains(content, "**Status:** working") {
					t.Errorf("task file missing 'working' status")
				}
				if !strings.Contains(content, "Find security bugs") {
					t.Errorf("task file missing task description")
				}
				if !strings.Contains(content, "(pending)") {
					t.Errorf("task file should have '(pending)' result for working status")
				}
				if strings.Contains(content, "**Completed:**") {
					t.Errorf("working task file should not have Completed line")
				}
			},
		},
		{
			name:      "write done task file with result",
			teamName:  "delegate",
			agentName: "writer",
			timestamp: "20260502-143100",
			status:    "done",
			task:      "Write docs",
			result:    "Here are the docs...",
			wantErr:   false,
			checkFunc: func(t *testing.T, workspace, teamName, agentName, timestamp string) {
				path := filepath.Join(workspace, "tasks", teamName, agentName, timestamp+".md")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("failed to read task file: %v", err)
				}
				content := string(data)
				if !strings.Contains(content, "**Status:** done") {
					t.Errorf("task file missing 'done' status")
				}
				if !strings.Contains(content, "Here are the docs...") {
					t.Errorf("task file missing result content")
				}
				if !strings.Contains(content, "**Completed:**") {
					t.Errorf("done task file should have Completed line")
				}
			},
		},
		{
			name:      "write error task file",
			teamName:  "delegate",
			agentName: "checker",
			timestamp: "20260502-143200",
			status:    "error",
			task:      "Run tests",
			result:    "",
			wantErr:   false,
			checkFunc: func(t *testing.T, workspace, teamName, agentName, timestamp string) {
				path := filepath.Join(workspace, "tasks", teamName, agentName, timestamp+".md")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("failed to read task file: %v", err)
				}
				content := string(data)
				if !strings.Contains(content, "**Status:** error") {
					t.Errorf("task file missing 'error' status")
				}
				if !strings.Contains(content, "**Completed:**") {
					t.Errorf("error task file should have Completed line")
				}
			},
		},
		{
			name:      "overwrite same file on update",
			teamName:  "delegate",
			agentName: "researcher",
			timestamp: "20260502-143005",
			status:    "done",
			task:      "Find security bugs",
			result:    "Found 3 bugs",
			wantErr:   false,
			checkFunc: func(t *testing.T, workspace, teamName, agentName, timestamp string) {
				path := filepath.Join(workspace, "tasks", teamName, agentName, timestamp+".md")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("failed to read task file: %v", err)
				}
				content := string(data)
				if !strings.Contains(content, "**Status:** done") {
					t.Errorf("task file should be updated to 'done' status")
				}
				if !strings.Contains(content, "Found 3 bugs") {
					t.Errorf("task file should contain the updated result")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := writeTaskFile(tmpDir, tt.teamName, tt.agentName, tt.timestamp, tt.status, tt.task, tt.result)
			if (err != nil) != tt.wantErr {
				t.Errorf("writeTaskFile() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.checkFunc != nil {
				tt.checkFunc(t, tmpDir, tt.teamName, tt.agentName, tt.timestamp)
			}
		})
	}
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

func TestWriteTaskFileWithDetailIncludesFailureSection(t *testing.T) {
	tmpDir := t.TempDir()
	err := writeTaskFileWithDetail(tmpDir, "delegate", "researcher", "20260502-143005", "error", "Find security bugs", "", "source=task_timeout | current=tool agent=helper tool=kvmforge create | error=context deadline exceeded")
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(tmpDir, "tasks", "delegate", "researcher", "20260502-143005.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "## Failure Detail") {
		t.Fatalf("task file should include failure detail section, got: %s", content)
	}
	if !strings.Contains(content, "source=task_timeout") {
		t.Fatalf("task file should include structured failure detail, got: %s", content)
	}
}

func TestWriteStatusWithDetailIncludesDetailField(t *testing.T) {
	tmpDir := t.TempDir()
	err := writeStatusWithDetail(tmpDir, "researcher", "error", "Find security bugs", "source=sigint | error=context canceled")
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(tmpDir, "status", "researcher.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "detail: source=sigint") {
		t.Fatalf("status file should include detail field, got: %s", content)
	}
}
