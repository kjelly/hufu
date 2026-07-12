//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"errors"
	"testing"
	"time"
)

// resetInteractiveWait clears the global prompt-wait accounting between tests.
func resetInteractiveWait() {
	askUserActive.Store(0)
	interactiveWaitStartNs.Store(0)
	interactiveWaitTotalNs.Store(0)
}

func TestInteractiveAwareTimeoutExpires(t *testing.T) {
	resetInteractiveWait()
	ctx, cancel := WithInteractiveAwareTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("context did not expire")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("Err() = %v, want DeadlineExceeded", ctx.Err())
	}
}

func TestInteractiveAwareTimeoutCompensatesForPromptWait(t *testing.T) {
	resetInteractiveWait()
	ctx, cancel := WithInteractiveAwareTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	// Simulate a prompt that spans well past the base deadline.
	SetAskUserActive(true)
	time.Sleep(300 * time.Millisecond)

	select {
	case <-ctx.Done():
		SetAskUserActive(false)
		t.Fatalf("context expired while a prompt was active: %v", ctx.Err())
	default:
	}

	SetAskUserActive(false)

	// The countdown resumes with roughly the time that was left when the
	// prompt started, so the context must survive a little longer and then
	// expire.
	select {
	case <-ctx.Done():
		t.Fatalf("context expired immediately after the prompt resolved: %v", ctx.Err())
	case <-time.After(20 * time.Millisecond):
	}

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("context never expired after compensation was spent")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("Err() = %v, want DeadlineExceeded", ctx.Err())
	}
}

func TestInteractiveAwareTimeoutParentCancel(t *testing.T) {
	resetInteractiveWait()
	parent, parentCancel := context.WithCancel(context.Background())
	ctx, cancel := WithInteractiveAwareTimeout(parent, time.Hour)
	defer cancel()

	parentCancel()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("parent cancellation did not propagate")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("Err() = %v, want Canceled", ctx.Err())
	}
}

func TestInteractiveAwareTimeoutCancelFunc(t *testing.T) {
	resetInteractiveWait()
	ctx, cancel := WithInteractiveAwareTimeout(context.Background(), time.Hour)
	cancel()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("cancel func did not stop the context")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("Err() = %v, want Canceled", ctx.Err())
	}
	cancel() // second cancel must be safe
}

func TestInteractiveAwareTimeoutValuePassthrough(t *testing.T) {
	resetInteractiveWait()
	type key struct{}
	parent := context.WithValue(context.Background(), key{}, "v")
	ctx, cancel := WithInteractiveAwareTimeout(parent, time.Hour)
	defer cancel()
	if got := ctx.Value(key{}); got != "v" {
		t.Fatalf("Value() = %v, want v", got)
	}
	if d, ok := ctx.Deadline(); !ok || d.IsZero() {
		t.Fatalf("Deadline() = %v, %v; want a real deadline", d, ok)
	}
}

func TestInteractiveWaitTotalAccumulates(t *testing.T) {
	resetInteractiveWait()
	SetAskUserActive(true)
	time.Sleep(30 * time.Millisecond)
	during := InteractiveWaitTotal()
	if during < 20*time.Millisecond {
		t.Fatalf("InteractiveWaitTotal during prompt = %v, want >= 20ms", during)
	}
	SetAskUserActive(false)
	after := InteractiveWaitTotal()
	if after < during {
		t.Fatalf("InteractiveWaitTotal after prompt = %v, want >= %v", after, during)
	}
	settled := InteractiveWaitTotal()
	if settled-after > 10*time.Millisecond {
		t.Fatalf("InteractiveWaitTotal kept growing without an active prompt: %v -> %v", after, settled)
	}
}
