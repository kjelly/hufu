package team

import (
	"context"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
)

func TestProviderSemaphore(t *testing.T) {
	coord := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{
		Providers: map[string]config.ProviderConfig{"ollama": {MaxConcurrent: 2}},
	}}}

	sem := coord.providerSemaphore("ollama")
	if sem == nil {
		t.Fatal("expected a semaphore for a provider with max-concurrent configured")
	}
	if cap(sem) != 2 {
		t.Errorf("cap = %d, want 2", cap(sem))
	}
	if coord.providerSemaphore("ollama") != sem {
		t.Error("providerSemaphore must return the same channel on repeated calls (shared limiter)")
	}
	if coord.providerSemaphore("unconfigured") != nil {
		t.Error("expected nil semaphore for a provider with no max-concurrent configured")
	}
	if coord.providerSemaphore("") != nil {
		t.Error("expected nil semaphore for an empty provider name")
	}

	nilSessionCoord := &Coordinator{}
	if nilSessionCoord.providerSemaphore("ollama") != nil {
		t.Error("expected nil semaphore when the coordinator has no session")
	}
}

func TestAcquireSemNilChannelAlwaysAvailable(t *testing.T) {
	slot, err := acquireSem(context.Background(), nil)
	if err != nil {
		t.Fatalf("acquireSem(nil) error = %v", err)
	}
	slot.release() // must not block or panic on a nil channel
	slot.release() // and must be safe to call twice
}

func TestAcquireSemLimitsConcurrency(t *testing.T) {
	ch := make(chan struct{}, 1)

	slot1, err := acquireSem(context.Background(), ch)
	if err != nil {
		t.Fatalf("first acquireSem error = %v", err)
	}

	timeoutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := acquireSem(timeoutCtx, ch); err == nil {
		t.Error("expected acquireSem to block (and time out) while the channel is full")
	}

	slot1.release()

	slot2, err := acquireSem(context.Background(), ch)
	if err != nil {
		t.Fatalf("acquireSem after release error = %v", err)
	}
	slot2.release()
}
