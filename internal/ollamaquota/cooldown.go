package ollamaquota

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kjelly/hufu/internal/waitbudget"
)

// Store journals only a credential-free account identity and its next check.
// A zero deadline clears the cooldown. Save must commit before returning.
type Store interface {
	Load(context.Context, string) (time.Time, error)
	Save(context.Context, string, time.Time) error
}

type account struct {
	gate          chan struct{}
	loaded        bool
	until         time.Time
	integrity     error
	availableAt   time.Time
	waiting       atomic.Int64
	scheduled     atomic.Int64
	queryDeadline atomic.Int64
}

// Engine is shared by all models using a ProviderManager. No goroutine or
// timer outlives a waiting call. The HTTP client never follows redirects.
type Engine struct {
	mu       sync.Mutex
	accounts map[string]*account
	client   *http.Client
}

// IsWaiting distinguishes a known provider cooldown from a silent model stall.
func (e *Engine) IsWaiting() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now().UnixNano()
	for _, a := range e.accounts {
		if a.waiting.Load() > 0 && (a.scheduled.Load() > now || a.queryDeadline.Load() > now) {
			return true
		}
	}
	return false
}

func New(transport http.RoundTripper) *Engine {
	return &Engine{accounts: make(map[string]*account), client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (e *Engine) account(id string) *account {
	e.mu.Lock()
	a := e.accounts[id]
	if a == nil {
		a = &account{gate: make(chan struct{}, 1)}
		e.accounts[id] = a
	}
	e.mu.Unlock()
	return a
}

func (e *Engine) acquire(ctx context.Context, id string, store Store) (*account, func(), error) {
	a := e.account(id)
	select {
	case a.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	release := func() { <-a.gate }
	if a.integrity != nil {
		release()
		return nil, nil, a.integrity
	}
	if !a.loaded {
		until, err := store.Load(ctx, id)
		if err != nil {
			release()
			return nil, nil, err
		}
		a.setUntil(until)
		a.loaded = true
	}
	return a, release, nil
}

func (a *account) save(ctx context.Context, id string, until time.Time, store Store) error {
	if err := store.Save(ctx, id, until); err != nil {
		if ctx.Err() == nil {
			a.integrity = err
		}
		return err
	}
	a.setUntil(until)
	if until.IsZero() {
		a.availableAt = time.Now()
	}
	return nil
}

func (a *account) setUntil(until time.Time) {
	a.until = until
	deadline := int64(0)
	if !until.IsZero() {
		deadline = until.UnixNano()
	}
	a.scheduled.Store(deadline)
}

func (a *account) query(ctx context.Context, e *Engine, key string) (time.Time, error) {
	a.queryDeadline.Store(time.Now().Add(balanceTimeout).UnixNano())
	defer a.queryDeadline.Store(0)
	return e.query(ctx, key)
}

// Wait rechecks the balance at the persisted deadline before permitting another
// generation. Only one caller probes an account; other callers remain paused.
func (e *Engine) Wait(ctx context.Context, id, key string, store Store) (func(bool) error, error) {
	waiting := e.account(id)
	waiting.waiting.Add(1)
	defer waiting.waiting.Add(-1)
	resume := waitbudget.Pause(ctx)
	defer resume()
	a, release, err := e.acquire(ctx, id, store)
	if err != nil {
		return nil, err
	}
	transferred := false
	defer func() {
		if !transferred {
			release()
		}
	}()
	for !a.until.IsZero() {
		if err := sleep(ctx, time.Until(a.until)); err != nil {
			return nil, err
		}
		until, queryErr := a.query(ctx, e, key)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if queryErr != nil && until.IsZero() {
			// Reserve the next probe before releasing this caller. Holding the gate
			// until its HTTP result prevents concurrent workers from probing together.
			if err := a.save(ctx, id, time.Now().Add(10*time.Minute), store); err != nil {
				return nil, err
			}
			transferred = true
			var finishErr error
			var once sync.Once
			return func(accepted bool) error {
				once.Do(func() {
					defer release()
					if accepted {
						finishErr = a.save(ctx, id, time.Time{}, store)
					}
				})
				return finishErr
			}, nil
		}
		if queryErr != nil {
			if fallback := time.Now().Add(10 * time.Minute); fallback.After(until) {
				until = fallback
			}
		}
		if err := a.save(ctx, id, until, store); err != nil {
			return nil, err
		}
	}
	return nil, ctx.Err()
}

// Rejected is called only for a proven HTTP 429 before any response was
// observed. It schedules a cooldown; Wait owns the cancellable wait itself.
func (e *Engine) Rejected(ctx context.Context, id, key, retryHeader string, store Store) error {
	rejectedAt := time.Now()
	waiting := e.account(id)
	waiting.waiting.Add(1)
	defer waiting.waiting.Add(-1)
	resume := waitbudget.Pause(ctx)
	defer resume()
	a, release, err := e.acquire(ctx, id, store)
	if err != nil {
		return err
	}
	defer release()
	// A delayed error from an earlier call must not recreate a cooldown that
	// a newer balance check or successful probe has already cleared.
	if !a.availableAt.IsZero() && !a.availableAt.Before(rejectedAt) {
		return nil
	}
	if a.until.After(time.Now()) {
		return nil
	}
	until, queryErr := a.query(ctx, e, key)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if header := retryAfter(retryHeader, time.Now()); header.After(until) {
		until = header
	}
	if until.IsZero() || queryErr != nil {
		if fallback := time.Now().Add(10 * time.Minute); fallback.After(until) {
			until = fallback
		}
	}
	return a.save(ctx, id, until, store)
}

func sleep(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}
