# SSH Session Manager Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Create an SSH session manager that tracks active SSH connections in context, allowing the TUI and coordinator to display and manage active sessions.

**Architecture:** Thread-safe session manager using mutex-protected map with context integration for dependency injection. Follows existing patterns in the tools package.

**Tech Stack:** Go 1.26.2, sync.RWMutex, context.Context, charm.land/fantasy

---

### Task 1: Update SSHSession struct with CreatedAt field

**Files:**
- Modify: `internal/tools/types.go:64-70`

- [ ] **Step 1: Add CreatedAt field to SSHSession struct**

Modify the SSHSession struct in `types.go` to include a `CreatedAt` timestamp:

```go
// SSHSession represents an active SSH connection context
type SSHSession struct {
	Host      string    // Remote host in [user@]hostname format
	User      string    // Username (extracted from host)
	Port      int       // SSH port (default 22)
	TaskID    string    // Task ID where this session was created
	CreatedAt time.Time // Session creation timestamp
}
```

- [ ] **Step 2: Add time import**

Add `time` to the imports in `types.go`:

```go
import (
	"context"
	"sync"
	"time"
)
```

- [ ] **Step 3: Verify build**

Run: `go build ./internal/tools/types.go`
Expected: No errors

---

### Task 2: Create SSH Session Manager Implementation

**Files:**
- Create: `internal/tools/ssh_session.go`

- [ ] **Step 1: Create file with package and imports**

```go
//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"sync"
	"time"
)
```

- [ ] **Step 2: Define SSHSessionManager struct**

```go
// SSHSessionManager manages active SSH sessions in memory
type SSHSessionManager struct {
	mu      sync.RWMutex
	sessions map[string]*SSHSession // key: host
}
```

- [ ] **Step 3: Define context key**

```go
// SSHSessionManagerKey is the context key for SSH session manager
type sshSessionManagerKey struct{}

var SSHSessionManagerKey = sshSessionManagerKey{}
```

- [ ] **Step 4: Implement NewSSHSessionManager constructor**

```go
// NewSSHSessionManager creates a new SSH session manager
func NewSSHSessionManager() *SSHSessionManager {
	return &SSHSessionManager{
		sessions: make(map[string]*SSHSession),
	}
}
```

- [ ] **Step 5: Implement Create method**

```go
// Create creates a new SSH session and stores it in the manager
// Returns the created session or error if session already exists
func (m *SSHSessionManager) Create(host string, user string, port int, taskID string) (*SSHSession, error) {
	if m == nil {
		return nil, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.sessions[host]; exists {
		return nil, nil // Session already exists
	}

	session := &SSHSession{
		Host:      host,
		User:      user,
		Port:      port,
		TaskID:    taskID,
		CreatedAt: time.Now(),
	}
	m.sessions[host] = session
	return session, nil
}
```

- [ ] **Step 6: Implement Get method**

```go
// Get retrieves an SSH session by host
// Returns nil if session not found or manager is nil
func (m *SSHSessionManager) Get(host string) *SSHSession {
	if m == nil {
		return nil
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.sessions[host]
}
```

- [ ] **Step 7: Implement List method**

```go
// List returns all active SSH sessions
// Returns empty slice if manager is nil or no sessions exist
func (m *SSHSessionManager) List() []*SSHSession {
	if m == nil {
		return nil
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	sessions := make([]*SSHSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	return sessions
}
```

- [ ] **Step 8: Implement Close method**

```go
// Close removes an SSH session from the manager
// Returns true if session was found and removed, false otherwise
func (m *SSHSessionManager) Close(host string) bool {
	if m == nil {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.sessions[host]; !exists {
		return false
	}

	delete(m.sessions, host)
	return true
}
```

- [ ] **Step 9: Implement Count method**

```go
// Count returns the number of active SSH sessions
func (m *SSHSessionManager) Count() int {
	if m == nil {
		return 0
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	return len(m.sessions)
}
```

- [ ] **Step 10: Implement context helpers**

```go
// SetSSHSessionManager stores the session manager in context
func SetSSHSessionManager(ctx context.Context, manager *SSHSessionManager) context.Context {
	return context.WithValue(ctx, SSHSessionManagerKey, manager)
}

// GetSSHSessionManager retrieves the session manager from context
func GetSSHSessionManager(ctx context.Context) *SSHSessionManager {
	if v, ok := ctx.Value(SSHSessionManagerKey).(*SSHSessionManager); ok {
		return v
	}
	return nil
}
```

- [ ] **Step 11: Implement ExtractUserFromHost helper**

```go
// ExtractUserFromHost extracts username from host string
// e.g., "user@host" -> user="user", host="host"
// If no @ symbol, returns empty user and original host
func ExtractUserFromHost(host string) (user string, cleanHost string) {
	// Implementation will be refined during testing
```

- [ ] **Step 12: Verify build**

Run: `go build ./internal/tools/ssh_session.go`
Expected: No errors

---

### Task 3: Add ExtractUserFromHost Implementation

**Files:**
- Modify: `internal/tools/ssh_session.go`

- [ ] **Step 1: Add strings import**

Add `strings` to imports:

```go
import (
	"context"
	"strings"
	"sync"
	"time"
)
```

- [ ] **Step 2: Implement ExtractUserFromHost**

```go
// ExtractUserFromHost extracts username from host string
// e.g., "user@host" -> user="user", host="host"
// If no @ symbol, returns empty user and original host
func ExtractUserFromHost(host string) (user string, cleanHost string) {
	parts := strings.SplitN(host, "@", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", host
}
```

- [ ] **Step 3: Verify build**

Run: `go build ./internal/tools/ssh_session.go`
Expected: No errors

---

### Task 4: Write SSH Session Manager Tests

**Files:**
- Create: `internal/tools/ssh_session_test.go`

- [ ] **Step 1: Create test file with package and imports**

```go
//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"testing"
	"time"
)
```

- [ ] **Step 2: Test CRUD operations - Create and Get**

```go
func TestSSHSessionManager_CreateAndGet(t *testing.T) {
	manager := NewSSHSessionManager()
	
	// Create a session
	session, err := manager.Create("test@example.com", "test", 22, "task-1")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if session == nil {
		t.Fatal("Expected session, got nil")
	}
	
	// Verify session fields
	if session.Host != "test@example.com" {
		t.Errorf("Expected host 'test@example.com', got '%s'", session.Host)
	}
	if session.User != "test" {
		t.Errorf("Expected user 'test', got '%s'", session.User)
	}
	if session.Port != 22 {
		t.Errorf("Expected port 22, got %d", session.Port)
	}
	if session.TaskID != "task-1" {
		t.Errorf("Expected taskID 'task-1', got '%s'", session.TaskID)
	}
	if session.CreatedAt.IsZero() {
		t.Error("Expected CreatedAt to be set")
	}
	
	// Get the session
	retrieved := manager.Get("test@example.com")
	if retrieved == nil {
		t.Fatal("Expected to retrieve session")
	}
	if retrieved.Host != session.Host {
		t.Errorf("Expected host '%s', got '%s'", session.Host, retrieved.Host)
	}
}
```

- [ ] **Step 3: Test duplicate creation**

```go
func TestSSHSessionManager_CreateDuplicate(t *testing.T) {
	manager := NewSSHSessionManager()
	
	// Create first session
	session1, err := manager.Create("test@example.com", "test", 22, "task-1")
	if err != nil || session1 == nil {
		t.Fatalf("First create failed: %v", err)
	}
	
	// Try to create duplicate
	session2, err := manager.Create("test@example.com", "test", 22, "task-2")
	if err != nil {
		t.Fatalf("Create should not return error for duplicate: %v", err)
	}
	if session2 != nil {
		t.Error("Expected nil for duplicate session")
	}
	
	// Verify original session unchanged
	count := manager.Count()
	if count != 1 {
		t.Errorf("Expected 1 session, got %d", count)
	}
}
```

- [ ] **Step 4: Test List method**

```go
func TestSSHSessionManager_List(t *testing.T) {
	manager := NewSSHSessionManager()
	
	// Create multiple sessions
	manager.Create("host1", "user1", 22, "task-1")
	manager.Create("host2", "user2", 2222, "task-2")
	manager.Create("host3", "user3", 22, "task-3")
	
	// List all sessions
	sessions := manager.List()
	if len(sessions) != 3 {
		t.Errorf("Expected 3 sessions, got %d", len(sessions))
	}
	
	// Verify all sessions are present
	hostMap := make(map[string]bool)
	for _, s := range sessions {
		hostMap[s.Host] = true
	}
	
	expectedHosts := []string{"host1", "host2", "host3"}
	for _, host := range expectedHosts {
		if !hostMap[host] {
			t.Errorf("Expected host '%s' in list", host)
		}
	}
}
```

- [ ] **Step 5: Test Close method**

```go
func TestSSHSessionManager_Close(t *testing.T) {
	manager := NewSSHSessionManager()
	
	// Create a session
	manager.Create("test@example.com", "test", 22, "task-1")
	
	// Close existing session
	closed := manager.Close("test@example.com")
	if !closed {
		t.Error("Expected Close to return true for existing session")
	}
	
	// Verify session is removed
	session := manager.Get("test@example.com")
	if session != nil {
		t.Error("Expected session to be removed after Close")
	}
	
	// Try to close non-existent session
	closed = manager.Close("nonexistent@example.com")
	if closed {
		t.Error("Expected Close to return false for non-existent session")
	}
}
```

- [ ] **Step 6: Test Count method**

```go
func TestSSHSessionManager_Count(t *testing.T) {
	manager := NewSSHSessionManager()
	
	// Initial count should be 0
	if manager.Count() != 0 {
		t.Errorf("Expected count 0, got %d", manager.Count())
	}
	
	// Add sessions
	manager.Create("host1", "user1", 22, "task-1")
	manager.Create("host2", "user2", 2222, "task-2")
	
	if manager.Count() != 2 {
		t.Errorf("Expected count 2, got %d", manager.Count())
	}
	
	// Close one session
	manager.Close("host1")
	
	if manager.Count() != 1 {
		t.Errorf("Expected count 1, got %d", manager.Count())
	}
}
```

- [ ] **Step 7: Test context integration**

```go
func TestSSHSessionManager_ContextIntegration(t *testing.T) {
	manager := NewSSHSessionManager()
	
	// Store in context
	ctx := SetSSHSessionManager(context.Background(), manager)
	
	// Retrieve from context
	retrieved := GetSSHSessionManager(ctx)
	if retrieved == nil {
		t.Fatal("Expected to retrieve manager from context")
	}
	
	// Verify it's the same instance
	if retrieved != manager {
		t.Error("Expected same manager instance")
	}
	
	// Create session via context
	session, _ := retrieved.Create("ctx@test.com", "ctx", 22, "ctx-task")
	if session == nil {
		t.Error("Expected session created via context")
	}
}
```

- [ ] **Step 8: Test nil manager handling**

```go
func TestSSHSessionManager_NilManager(t *testing.T) {
	var manager *SSHSessionManager
	
	// Test methods on nil manager don't panic
	session := manager.Get("host")
	if session != nil {
		t.Error("Expected nil session from nil manager")
	}
	
	sessions := manager.List()
	if sessions != nil {
		t.Error("Expected nil list from nil manager")
	}
	
	closed := manager.Close("host")
	if closed {
		t.Error("Expected false from nil manager Close")
	}
	
	count := manager.Count()
	if count != 0 {
		t.Errorf("Expected count 0 from nil manager, got %d", count)
	}
	
	// Create should return nil, nil
	session, err := manager.Create("host", "user", 22, "task")
	if session != nil || err != nil {
		t.Errorf("Expected nil, nil from nil manager Create, got %v, %v", session, err)
	}
}
```

- [ ] **Step 9: Test ExtractUserFromHost helper**

```go
func TestExtractUserFromHost(t *testing.T) {
	tests := []struct {
		host       string
		wantUser   string
		wantHost   string
	}{
		{"user@example.com", "user", "example.com"},
		{"admin@192.168.1.1", "admin", "192.168.1.1"},
		{"hostname", "", "hostname"},
		{"user@host@domain", "user", "host@domain"}, // SplitN(2) behavior
		{"", "", ""},
	}
	
	for _, tt := range tests {
		user, host := ExtractUserFromHost(tt.host)
		if user != tt.wantUser {
			t.Errorf("ExtractUserFromHost(%q) user = %q, want %q", tt.host, user, tt.wantUser)
		}
		if host != tt.wantHost {
			t.Errorf("ExtractUserFromHost(%q) host = %q, want %q", tt.host, host, tt.wantHost)
		}
	}
}
```

- [ ] **Step 10: Run tests**

Run: `go test ./internal/tools/ssh_session_test.go ./internal/tools/ssh_session.go ./internal/tools/types.go -v`
Expected: All 8 tests pass

---

### Task 5: Final Verification

**Files:**
- All created/modified files

- [ ] **Step 1: Run go vet**

Run: `go vet ./internal/tools/...`
Expected: No issues

- [ ] **Step 2: Run all tests**

Run: `go test ./internal/tools/... -v`
Expected: All tests pass including new SSH session tests

- [ ] **Step 3: Build check**

Run: `go build ./cmd/hufu`
Expected: Clean build

- [ ] **Step 4: Create git commit**

```bash
git add internal/tools/ssh_session.go internal/tools/ssh_session_test.go internal/tools/types.go
git commit -m "feat: add SSH session manager for connection tracking

Implements SSHSessionManager with mutex-protected session storage:
- Create/Get/List/Close/Count methods for session lifecycle
- Context integration via SetSSHSessionManager/GetSSHSessionManager
- ExtractUserFromHost helper for parsing user@host strings
- CreatedAt timestamp in SSHSession struct
- Thread-safe with RWMutex (RLock for reads, Lock for writes)
- Nil-safe methods that handle nil manager gracefully

Adds comprehensive test coverage:
- CRUD operations
- Context integration
- Nil manager edge cases
- ExtractUserFromHost helper

Enables TUI and coordinator to display and manage active SSH sessions."
```

Expected: Clean commit with descriptive message

---

## Self-Review Checklist

Before marking complete:

- [ ] All tests pass (8 test functions minimum)
- [ ] `go vet` reports no issues
- [ ] Code follows existing patterns in tools package
- [ ] Mutex usage is correct (RLock for reads, Lock for writes)
- [ ] Context integration works
- [ ] SSHSession struct has CreatedAt field
- [ ] Commit message is descriptive
- [ ] Build tags `//go:build linux || darwin` are present
- [ ] Nil manager edge cases handled
