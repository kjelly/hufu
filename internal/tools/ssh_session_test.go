//go:build linux || darwin
// +build linux darwin

package tools

import (
	"testing"
	"time"
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
	retrieved := manager.Get("example.com")
	if retrieved == nil {
		t.Fatal("Expected to retrieve session")
	}
}

func TestSSHSessionManager_CreateDuplicate(t *testing.T) {
	manager := NewSSHSessionManager()

	// Create first session
	session1, err := manager.Create("example.com", "test", 22, "task-1")
	if err != nil || session1 == nil {
		t.Fatalf("First create failed: %v", err)
	}

	// Try to create duplicate - should return existing session
	session2, err := manager.Create("example.com", "test", 22, "task-2")
	if err != nil {
		t.Fatalf("Create should not return error for duplicate: %v", err)
	}
	if session2 == nil {
		t.Fatal("Expected to get existing session")
	}

	// Should be the same session object
	if session1 != session2 {
		t.Error("Expected to get same session object")
	}

	// Count should still be 1
	if manager.Count() != 1 {
		t.Errorf("Expected 1 session, got %d", manager.Count())
	}
}

func TestSSHSessionManager_Close(t *testing.T) {
	manager := NewSSHSessionManager()

	// Create a session
	manager.Create("example.com", "test", 22, "task-1")

	if manager.Count() != 1 {
		t.Fatalf("Expected 1 session after create")
	}

	// Close the session
	closed := manager.Close("example.com")
	if !closed {
		t.Error("Expected Close to return true")
	}

	if manager.Count() != 0 {
		t.Errorf("Expected 0 sessions after close, got %d", manager.Count())
	}

	// Close non-existent session
	closed2 := manager.Close("nonexistent.com")
	if closed2 {
		t.Error("Expected Close to return false for non-existent session")
	}
}

func TestSSHSessionManager_List(t *testing.T) {
	manager := NewSSHSessionManager()

	// Create multiple sessions
	manager.Create("host1.com", "user1", 22, "task-1")
	manager.Create("host2.com", "user2", 22, "task-2")
	manager.Create("host3.com", "user3", 2222, "task-3")

	sessions := manager.List()
	if len(sessions) != 3 {
		t.Errorf("Expected 3 sessions, got %d", len(sessions))
	}
}

func TestSSHSessionManager_Count(t *testing.T) {
	manager := NewSSHSessionManager()

	if manager.Count() != 0 {
		t.Errorf("Expected 0 sessions initially, got %d", manager.Count())
	}

	manager.Create("host1.com", "user", 22, "task-1")
	manager.Create("host2.com", "user", 22, "task-2")

	if manager.Count() != 2 {
		t.Errorf("Expected 2 sessions, got %d", manager.Count())
	}

	manager.Close("host1.com")

	if manager.Count() != 1 {
		t.Errorf("Expected 1 session after close, got %d", manager.Count())
	}
}

func TestSSHSessionManager_NilManager(t *testing.T) {
	var manager *SSHSessionManager

	// Test all methods with nil manager
	session, err := manager.Create("host", "user", 22, "task")
	if err != nil || session != nil {
		t.Error("Expected nil session and no error with nil manager")
	}

	if manager.Get("host") != nil {
		t.Error("Expected nil from Get with nil manager")
	}

	if manager.Close("host") {
		t.Error("Expected false from Close with nil manager")
	}

	sessions := manager.List()
	if sessions == nil {
		t.Error("Expected empty slice from List with nil manager")
	}

	if manager.Count() != 0 {
		t.Error("Expected 0 from Count with nil manager")
	}

	if manager.SetPassword("host", "pass", 5*time.Minute) {
		t.Error("Expected false from SetPassword with nil manager")
	}

	if _, ok := manager.GetPassword("host"); ok {
		t.Error("Expected false from GetPassword with nil manager")
	}

	if manager.ClearPassword("host") {
		t.Error("Expected false from ClearPassword with nil manager")
	}

	if manager.UpdateLastUsed("host"); manager != nil {
		// UpdateLastUsed should not panic
	}
}

func TestSSHSessionManager_PasswordCaching(t *testing.T) {
	manager := NewSSHSessionManager()

	// Create session first
	manager.Create("example.com", "test", 22, "task-1")

	// Set password
	success := manager.SetPassword("example.com", "secret123", 5*time.Minute)
	if !success {
		t.Fatal("Expected SetPassword to succeed")
	}

	// Get password
	password, ok := manager.GetPassword("example.com")
	if !ok {
		t.Fatal("Expected to retrieve password")
	}
	if password != "secret123" {
		t.Errorf("Expected password 'secret123', got '%s'", password)
	}
}

func TestSSHSessionManager_PasswordExpiry(t *testing.T) {
	manager := NewSSHSessionManager()

	// Create session first
	manager.Create("example.com", "test", 22, "task-1")

	// Set password with very short expiry
	manager.SetPassword("example.com", "secret123", 100*time.Millisecond)

	// Should work immediately
	password, ok := manager.GetPassword("example.com")
	if !ok || password != "secret123" {
		t.Fatal("Expected to retrieve password immediately")
	}

	// Wait for expiry
	time.Sleep(150 * time.Millisecond)

	// Should be expired
	_, ok = manager.GetPassword("example.com")
	if ok {
		t.Error("Expected password to be expired")
	}
}

func TestSSHSessionManager_ClearPassword(t *testing.T) {
	manager := NewSSHSessionManager()

	// Create session and set password
	manager.Create("example.com", "test", 22, "task-1")
	manager.SetPassword("example.com", "secret123", 5*time.Minute)

	// Clear password
	success := manager.ClearPassword("example.com")
	if !success {
		t.Fatal("Expected ClearPassword to succeed")
	}

	// Password should be gone
	_, ok := manager.GetPassword("example.com")
	if ok {
		t.Error("Expected password to be cleared")
	}
}

func TestSSHSessionManager_PasswordNoSession(t *testing.T) {
	manager := NewSSHSessionManager()

	// Try to set password without creating session
	success := manager.SetPassword("example.com", "secret123", 5*time.Minute)
	if success {
		t.Error("Expected SetPassword to fail without session")
	}

	// Try to get password without session
	_, ok := manager.GetPassword("example.com")
	if ok {
		t.Error("Expected GetPassword to fail without session")
	}

	// Try to clear password without session
	success = manager.ClearPassword("example.com")
	if success {
		t.Error("Expected ClearPassword to fail without session")
	}
}

func TestSSHSessionManager_UpdateLastUsed(t *testing.T) {
	manager := NewSSHSessionManager()

	// Create session
	manager.Create("example.com", "test", 22, "task-1")

	// Get initial LastUsedAt
	session := manager.Get("example.com")
	if session == nil {
		t.Fatal("Expected to get session")
	}
	initialTime := session.LastUsedAt

	// Wait a bit
	time.Sleep(10 * time.Millisecond)

	// Update last used
	manager.UpdateLastUsed("example.com")

	// Get session again
	session = manager.Get("example.com")
	if session.LastUsedAt.Before(initialTime) || session.LastUsedAt.Equal(initialTime) {
		t.Error("Expected LastUsedAt to be updated")
	}
}

func TestSSHSessionManager_CleanupIdle(t *testing.T) {
	manager := NewSSHSessionManager()

	// Create sessions
	manager.Create("host1.com", "user", 22, "task-1")
	manager.Create("host2.com", "user", 22, "task-2")

	// Manually set LastUsedAt to simulate old sessions
	sessions := manager.List()
	for _, s := range sessions {
		s.LastUsedAt = time.Now().Add(-1 * time.Hour)
	}

	// Cleanup with 30 minute timeout
	removed := manager.CleanupIdle(30 * time.Minute)
	if removed != 2 {
		t.Errorf("Expected to remove 2 sessions, got %d", removed)
	}

	if manager.Count() != 0 {
		t.Errorf("Expected 0 sessions after cleanup, got %d", manager.Count())
	}
}

func TestSSHSessionManager_CleanupIdlePartial(t *testing.T) {
	manager := NewSSHSessionManager()

	// Create sessions
	manager.Create("host1.com", "user", 22, "task-1")
	manager.Create("host2.com", "user", 22, "task-2")

	// Set only host1 as old
	sessions := manager.List()
	for _, s := range sessions {
		if s.Host == "host1.com" {
			s.LastUsedAt = time.Now().Add(-1 * time.Hour)
		}
	}

	// Cleanup with 30 minute timeout
	removed := manager.CleanupIdle(30 * time.Minute)
	if removed != 1 {
		t.Errorf("Expected to remove 1 session, got %d", removed)
	}

	if manager.Count() != 1 {
		t.Errorf("Expected 1 session after cleanup, got %d", manager.Count())
	}

	// host2 should still exist
	session := manager.Get("host2.com")
	if session == nil {
		t.Error("Expected host2.com to still exist")
	}
}

func TestSSHSessionManager_UpdateLastUsedOnCreate(t *testing.T) {
	manager := NewSSHSessionManager()

	// Create session
	manager.Create("example.com", "test", 22, "task-1")
	session := manager.Get("example.com")
	firstTime := session.LastUsedAt

	// Wait
	time.Sleep(10 * time.Millisecond)

	// Create again (should update LastUsedAt)
	manager.Create("example.com", "test", 22, "task-2")
	session = manager.Get("example.com")

	if !session.LastUsedAt.After(firstTime) {
		t.Error("Expected LastUsedAt to be updated on duplicate Create")
	}
}
