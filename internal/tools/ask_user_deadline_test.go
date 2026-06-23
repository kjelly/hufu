//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"testing"
	"time"
)

// TestAskUserAwareDeadline_NoDeadlineUnchanged verifies that a context
// without a deadline is returned unwrapped (so callers can avoid the
// allocation cost when no deadline exists).
func TestAskUserAwareDeadline_NoDeadlineUnchanged(t *testing.T) {
	base := context.Background()
	if _, hasDeadline := base.Deadline(); hasDeadline {
		t.Fatal("baseline: context.Background() should not have a deadline")
	}

	wrapped := AskUserAwareDeadline(base)
	if _, hasDeadline := wrapped.Deadline(); hasDeadline {
		t.Errorf("AskUserAwareDeadline returned a context with a deadline; want no deadline")
	}
}

// TestAskUserAwareDeadline_InactiveReturnsOriginal verifies that when
// ask_user is not active, the wrapper delegates to the underlying
// context's deadline.
func TestAskUserAwareDeadline_InactiveReturnsOriginal(t *testing.T) {
	SetAskUserActive(false)
	defer SetAskUserActive(false)

	deadline := time.Now().Add(5 * time.Minute)
	base, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	wrapped := AskUserAwareDeadline(base)

	got, ok := wrapped.Deadline()
	if !ok {
		t.Fatal("wrapped.Deadline() returned ok=false; want true when ask_user inactive")
	}
	if !got.Equal(deadline) {
		t.Errorf("wrapped.Deadline() = %v, want %v", got, deadline)
	}
}

// TestAskUserAwareDeadline_ActiveHidesDeadline verifies that when
// ask_user is active, the wrapper returns no deadline (freezing the
// parent's countdown).
func TestAskUserAwareDeadline_ActiveHidesDeadline(t *testing.T) {
	SetAskUserActive(true)
	defer SetAskUserActive(false)

	deadline := time.Now().Add(5 * time.Minute)
	base, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	wrapped := AskUserAwareDeadline(base)

	if _, ok := wrapped.Deadline(); ok {
		t.Errorf("wrapped.Deadline() returned ok=true; want false when ask_user active")
	}
	if err := wrapped.Err(); err != nil {
		t.Errorf("wrapped.Err() = %v; want nil when ask_user active", err)
	}
	if done := wrapped.Done(); done != nil {
		// Done channel should be nil while ask_user is active.
		select {
		case <-done:
			t.Error("wrapped.Done() fired; should be nil when ask_user active")
		default:
			// ok
		}
	}
}

// TestAskUserAwareDeadline_ToggleRestoresDeadline verifies that the
// wrapper's view of the deadline toggles based on IsAskUserActive.
// The underlying context is unchanged — only the view through the
// wrapper flips.
func TestAskUserAwareDeadline_ToggleRestoresDeadline(t *testing.T) {
	SetAskUserActive(false)
	defer SetAskUserActive(false)

	deadline := time.Now().Add(5 * time.Minute)
	base, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	wrapped := AskUserAwareDeadline(base)

	// 1. Inactive: original deadline visible.
	if _, ok := wrapped.Deadline(); !ok {
		t.Fatal("inactive: wrapped.Deadline() should be visible")
	}

	// 2. Activate: deadline hidden.
	SetAskUserActive(true)
	if _, ok := wrapped.Deadline(); ok {
		t.Error("active: wrapped.Deadline() should be hidden")
	}
	if err := wrapped.Err(); err != nil {
		t.Errorf("active: wrapped.Err() = %v; want nil", err)
	}

	// 3. Deactivate: original deadline visible again.
	SetAskUserActive(false)
	got, ok := wrapped.Deadline()
	if !ok {
		t.Fatal("deactivated: wrapped.Deadline() should be visible again")
	}
	if !got.Equal(deadline) {
		t.Errorf("deactivated: wrapped.Deadline() = %v, want %v", got, deadline)
	}
}

// TestAskUserAwareDeadline_ValuePreserved verifies that Value() is
// delegated to the wrapped context so all context.WithValue keys
// remain accessible through the wrapper.
func TestAskUserAwareDeadline_ValuePreserved(t *testing.T) {
	type key struct{}
	base := context.WithValue(context.Background(), key{}, "v")

	wrapped := AskUserAwareDeadline(base)
	if got := wrapped.Value(key{}); got != "v" {
		t.Errorf("wrapped.Value(key) = %v, want %q", got, "v")
	}
}
