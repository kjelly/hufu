//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"testing"
)

func TestSSHSessionManager_CreateAndGet(t *testing.T) {
	manager := NewSSHSessionManager()

	// Create a session
	session, err := manager.Create("example.com", "test", 22, "task-1")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if session == nil {
		t.Fatal("Expected session, got nil")
	}

	// Verify session fields
	if session.Host != "example.com" {
		t.Errorf("Expected host 'example.com', got '%s'", session.Host)
	}
	if session.User != "test" {
		t.Errorf("Expected user 'test', got '%s'", session.User)
	}
	if session.Port != 22 {
		t.Errorf("Expected port 22, got %d", session.Port)
	}

	// Get the session
	retrieved := manager.Get("test", "example.com", 22)
	if retrieved == nil {
		t.Fatal("Expected to retrieve session")
	}
}

func TestSSHSessionManager_UserIsolation(t *testing.T) {
	manager := NewSSHSessionManager()

	// Create sessions for different users on the same host
	manager.Create("example.com", "user1", 22, "task-1")
	manager.Create("example.com", "user2", 22, "task-1")

	if manager.Count() != 2 {
		t.Errorf("Expected 2 sessions, got %d", manager.Count())
	}

	// Verify isolation
	s1 := manager.Get("user1", "example.com", 22)
	s2 := manager.Get("user2", "example.com", 22)

	if s1 == nil || s2 == nil {
		t.Fatal("Expected both sessions to be retrievable")
	}
	if s1 == s2 {
		t.Error("Expected sessions for different users to be separate")
	}
}

func TestSSHSessionManager_PortAwareSessions(t *testing.T) {
	manager := NewSSHSessionManager()

	// Create sessions on different ports for the same host and user
	manager.Create("example.com", "user", 22, "task-1")
	manager.Create("example.com", "user", 2222, "task-1")

	if manager.Count() != 2 {
		t.Errorf("Expected 2 sessions, got %d", manager.Count())
	}

	// Verify they are separate
	s1 := manager.Get("user", "example.com", 22)
	s2 := manager.Get("user", "example.com", 2222)

	if s1 == nil || s2 == nil {
		t.Fatal("Expected both sessions to be retrievable")
	}
	if s1 == s2 {
		t.Error("Expected sessions on different ports to be separate")
	}
}

func TestSSHSessionManager_CreateDuplicate(t *testing.T) {
	manager := NewSSHSessionManager()

	// Create first session
	session1, err := manager.Create("example.com", "test", 22, "task-1")
	if err != nil || session1 == nil {
		t.Fatalf("First create failed: %v", err)
	}

	// Try to create duplicate
	session2, err := manager.Create("example.com", "test", 22, "task-2")
	if err != nil {
		t.Fatalf("Create should not return error for duplicate: %v", err)
	}
	if session2 == nil {
		t.Error("Expected existing session for duplicate, got nil")
	}
}

func TestSSHSessionManager_Close(t *testing.T) {
	manager := NewSSHSessionManager()

	// Create a session
	manager.Create("example.com", "test", 22, "task-1")

	// Close existing session
	closed := manager.Close("test", "example.com", 22)
	if !closed {
		t.Error("Expected Close to return true for existing session")
	}

	// Verify session is removed
	session := manager.Get("test", "example.com", 22)
	if session != nil {
		t.Error("Expected session to be removed after Close")
	}
}

func TestSSHSessionManager_Count(t *testing.T) {
	manager := NewSSHSessionManager()

	// Add sessions
	manager.Create("host1", "user1", 22, "task-1")
	manager.Create("host2", "user2", 2222, "task-2")

	if manager.Count() != 2 {
		t.Errorf("Expected count 2, got %d", manager.Count())
	}

	// Close one session
	manager.Close("user1", "host1", 22)

	if manager.Count() != 1 {
		t.Errorf("Expected count 1, got %d", manager.Count())
	}
}

func TestSSHSessionManager_NilManager(t *testing.T) {
	var manager *SSHSessionManager

	// Test methods on nil manager don't panic
	session := manager.Get("user", "host", 22)
	if session != nil {
		t.Error("Expected nil session from nil manager")
	}

	closed := manager.Close("user", "host", 22)
	if closed {
		t.Error("Expected false from nil manager Close")
	}
}

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
}

func TestExtractUserFromHost(t *testing.T) {
	tests := []struct {
		host     string
		wantUser string
		wantHost string
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
