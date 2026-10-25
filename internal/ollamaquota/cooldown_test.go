package ollamaquota

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kjelly/hufu/internal/waitbudget"
)

type memoryStore struct {
	mu    sync.Mutex
	until time.Time
	saves []time.Time
	err   error
}

func (s *memoryStore) Load(context.Context, string) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.until, s.err
}
func (s *memoryStore) Save(_ context.Context, _ string, until time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.until = until
	s.saves = append(s.saves, until)
	return nil
}

func TestCooldownWaitsUntilLatestResetAndRechecksBalance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		session := start.Add(time.Hour)
		weekly := start.Add(24 * time.Hour)
		calls := 0
		e := New(roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return balanceResponse(legacyBalance(0, 0, session, weekly)), nil
			}
			if time.Now().Before(weekly) {
				t.Fatal("queried before the later reset")
			}
			return balanceResponse(legacyBalance(100, 100, weekly.Add(time.Hour), weekly.Add(24*time.Hour))), nil
		}))
		store := &memoryStore{}
		ctx, cancel := waitbudget.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := e.Rejected(ctx, "account", "key", "", store); err != nil {
			t.Fatal(err)
		}
		if finish, err := e.Wait(ctx, "account", "key", store); err != nil || finish != nil {
			t.Fatalf("wait: finish=%v err=%v", finish != nil, err)
		}
		if calls != 2 || !time.Now().Equal(weekly) || ctx.Err() != nil {
			t.Fatalf("calls=%d elapsed=%s err=%v", calls, time.Since(start), ctx.Err())
		}
		if len(store.saves) != 2 || !store.saves[0].Equal(weekly) || !store.saves[1].IsZero() {
			t.Fatalf("journal=%v", store.saves)
		}
	})
}

func TestCooldownFallbackAllowsOneProbeEveryTenMinutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		store := &memoryStore{}
		e := New(nil) // Missing key must never make an HTTP request.
		if err := e.Rejected(t.Context(), "account", "", "", store); err != nil {
			t.Fatal(err)
		}
		if !store.until.Equal(start.Add(10 * time.Minute)) {
			t.Fatalf("retry=%v", store.until)
		}
		finish, err := e.Wait(t.Context(), "account", "", store)
		if err != nil || finish == nil {
			t.Fatalf("probe: %v", err)
		}
		if time.Since(start) != 10*time.Minute {
			t.Fatal("incorrect fallback interval")
		}
		done := make(chan error, 1)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() {
			next, err := e.Wait(ctx, "account", "", store)
			if next != nil {
				err = next(false)
			}
			done <- err
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("second probe was not serialized")
		default:
		}
		if err := finish(false); err != nil {
			t.Fatal(err)
		}
		if err := e.Rejected(t.Context(), "account", "", "", store); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if time.Since(start) != 20*time.Minute {
			t.Fatalf("second probe at %s", time.Since(start))
		}
	})
}

func TestCooldownRestoresWaitAfterRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		until := start.Add(10 * time.Minute)
		store := &memoryStore{until: until}
		e := New(nil)
		finish, err := e.Wait(t.Context(), "account", "", store)
		if err != nil || finish == nil || !time.Now().Equal(until) {
			t.Fatalf("restored wait: now=%v err=%v", time.Now(), err)
		}
		if err := finish(true); err != nil {
			t.Fatal(err)
		}
		if !store.until.IsZero() {
			t.Fatal("successful probe did not clear wait")
		}
	})
}

func TestCooldownDoesNotHideStalledProbeBehindOtherWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &memoryStore{until: time.Now().Add(10 * time.Minute)}
		e := New(nil)
		finish, err := e.Wait(t.Context(), "account", "", store)
		if err != nil || finish == nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := e.Wait(ctx, "account", "", store); done <- err }()
		synctest.Wait()
		if !e.IsWaiting() {
			t.Fatal("known pending cooldown not visible")
		}
		time.Sleep(10 * time.Minute)
		if e.IsWaiting() {
			t.Fatal("blocked probe indefinitely suppressed stall detection")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if err := finish(false); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCooldownIgnoresQueuedRejectionsAfterSuccessfulProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := New(nil)
		store := &memoryStore{until: time.Now().Add(10 * time.Minute)}
		finish, err := e.Wait(t.Context(), "account", "", store)
		if err != nil || finish == nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- e.Rejected(t.Context(), "account", "", "", store) }()
		synctest.Wait()
		if err := finish(true); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if !store.until.IsZero() {
			t.Fatal("stale rejection recreated a cleared cooldown")
		}
	})
}

func TestCooldownWaitHonorsCancellationAndPersistenceFailures(t *testing.T) {
	for _, tt := range []string{"cancel", "load failure", "save failure"} {
		t.Run(tt, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sentinel := errors.New("journal unavailable")
				store := &memoryStore{until: time.Now().Add(time.Hour)}
				e := New(nil)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tt == "load failure" {
					store.err = sentinel
				} else if tt == "save failure" {
					if finish, err := e.Wait(ctx, "unused", "", &memoryStore{}); err != nil || finish != nil {
						t.Fatal(err)
					}
					store.until = time.Time{}
					store.err = sentinel
					_, release, err := e.acquire(ctx, "account", &memoryStore{})
					if err != nil {
						t.Fatal(err)
					}
					release()
					if err := e.Rejected(ctx, "account", "", "", store); !errors.Is(err, sentinel) {
						t.Fatalf("save error=%v", err)
					}
					return
				} else {
					cancel()
				}
				finish, err := e.Wait(ctx, "account", "", store)
				if finish != nil {
					t.Fatal("failed wait allowed a probe")
				}
				want := context.Canceled
				if tt == "load failure" {
					want = sentinel
				}
				if !errors.Is(err, want) {
					t.Fatalf("wait error=%v want %v", err, want)
				}
			})
		})
	}
}
