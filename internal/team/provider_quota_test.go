package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/waitbudget"
)

func TestProviderQuotaWaitHonorsTotalDurationWithoutConsumingModelTimeout(t *testing.T) {
	t.Setenv("OLLAMA_API_KEY", "")
	c := newTestCoordinator(t)
	pm, err := agent.NewProviderManager("http://localhost:11434/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.providerManager = pm
	c.SetBudget(300, 0)
	synctest.Test(t, func(t *testing.T) {
		c.sessionTime = time.Now()
		ctx, cancel := waitbudget.WithTimeout(t.Context(), time.Second)
		defer cancel()
		admission := providerRequestAdmission{c}
		request := agent.ProviderRequest{ModelID: "ollama/glm:cloud"}
		if _, err := admission.RecoverProviderRequest(ctx, request, &fantasy.ProviderError{StatusCode: 429}); err != nil {
			t.Fatal(err)
		}
		finish, err := admission.WaitProviderRequest(ctx, request)
		if finish != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("wait=%v", err)
		}
		if time.Since(c.sessionTime) != 5*time.Minute || ctx.Err() != nil {
			t.Fatalf("elapsed=%s model timeout=%v", time.Since(c.sessionTime), ctx.Err())
		}
	})
}

func TestProviderQuotaJournalRestoresLatestDeadlineAcrossRuns(t *testing.T) {
	c := newTestCoordinator(t)
	store, err := NewEventStore(c.session.Workspace, "run-quota", "session-quota")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c.SetEventJournal(eventStoreJournal{store: store})
	journal := providerQuotaJournal{c}
	until := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	if err := journal.Save(t.Context(), "account-digest", until); err != nil {
		t.Fatal(err)
	}
	// A different run in this branch must still respect the same account wait.
	c.executionRunID = "resumed-run"
	if got, err := journal.Load(t.Context(), "account-digest"); err != nil || !got.Equal(until) {
		t.Fatalf("load=%v err=%v", got, err)
	}
	if got, err := journal.Load(t.Context(), "rotated-credential-digest"); err != nil || !got.IsZero() {
		t.Fatal("cooldown crossed credential identities")
	}
	if err := journal.Save(t.Context(), "account-digest", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got, err := journal.Load(t.Context(), "account-digest"); err != nil || !got.IsZero() {
		t.Fatal("cleared cooldown was restored")
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || !IsKnownEventType(events[0].Type) {
		t.Fatalf("events=%v", eventTypes(events))
	}
	var payload map[string]any
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) != 2 || !strings.Contains(string(events[0].Payload), "account-digest") {
		t.Fatalf("unexpected durable quota data=%s", events[0].Payload)
	}
}

func TestProviderQuotaJournalFailsClosed(t *testing.T) {
	c := newTestCoordinator(t)
	c.SetEventJournal(&failingEventJournal{})
	if err := (providerQuotaJournal{c}).Save(t.Context(), "digest", time.Now()); err == nil {
		t.Fatalf("save=%v", err)
	}
}

func TestProviderTransientRecoveryHonorsBudgetAndRetryLimit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retries   int
		cancelled bool
		wantRetry bool
		wantDelay time.Duration
	}{
		{name: "first resend", wantRetry: true, wantDelay: time.Second},
		{name: "second resend", retries: 1, wantRetry: true, wantDelay: 2 * time.Second},
		{name: "exhausted", retries: agent.ProviderTransientRetryLimit},
		{name: "invalid counter", retries: -1},
		{name: "cancelled", cancelled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCoordinator(t)
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.cancelled {
					cancel()
				}
				start := time.Now()
				retry, err := (providerRequestAdmission{c}).RecoverProviderRequest(ctx, agent.ProviderRequest{ModelID: "fixture/model", TransientRetries: tc.retries}, &fantasy.ProviderError{TransientError: true})
				if retry != tc.wantRetry || errors.Is(err, context.Canceled) != tc.cancelled || time.Since(start) != tc.wantDelay {
					t.Fatalf("retry=%v err=%v elapsed=%s", retry, err, time.Since(start))
				}
			})
		})
	}
	c := newTestCoordinator(t)
	c.SetBudget(1, 0)
	synctest.Test(t, func(t *testing.T) {
		c.sessionTime = time.Now()
		retry, err := (providerRequestAdmission{c}).RecoverProviderRequest(t.Context(), agent.ProviderRequest{TransientRetries: 1}, &fantasy.ProviderError{TransientError: true})
		if retry || !errors.Is(err, context.DeadlineExceeded) || time.Since(c.sessionTime) != time.Second {
			t.Fatalf("wall budget bypassed: retry=%v err=%v elapsed=%s", retry, err, time.Since(c.sessionTime))
		}
	})
}

func TestInvocationWatchdogAllowsKnownQuotaWait(t *testing.T) {
	t.Setenv("OLLAMA_API_KEY", "")
	c := newTestCoordinator(t)
	pm, err := agent.NewProviderManager("http://localhost:11434/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.providerManager = pm
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		w := c.newInvocationWatchdog(ctx, cancel)
		w.start()
		defer func() { cancel(context.Canceled); w.wait() }()
		done := make(chan error, 1)
		admission := providerRequestAdmission{c}
		if _, err := admission.RecoverProviderRequest(ctx, agent.ProviderRequest{ModelID: "ollama/glm:cloud"}, &fantasy.ProviderError{StatusCode: 429}); err != nil {
			t.Fatal(err)
		}
		go func() {
			finish, err := admission.WaitProviderRequest(ctx, agent.ProviderRequest{ModelID: "ollama/glm:cloud"})
			if finish != nil {
				err = finish(false)
			}
			done <- err
		}()
		synctest.Wait()
		time.Sleep(6 * time.Minute)
		synctest.Wait()
		if ctx.Err() != nil || w.stalled.Load() || !c.ProviderQuotaWaiting() {
			t.Fatal("known quota wait treated as silent stall")
		}
		cancel(context.Canceled)
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("wait cancel=%v", err)
		}
		if c.ProviderQuotaWaiting() {
			t.Fatal("cancelled quota wait remains active")
		}
	})
}
