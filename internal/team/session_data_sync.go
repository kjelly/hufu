package team

import (
	"fmt"
	"time"
)

// mutateSessionData runs fn against the shared session state under an exclusive
// lock. It lazily initializes sessionData so every caller may assume a non-nil
// receiver inside fn. fn must not perform I/O, call SaveSession/persistSession,
// or call any method that re-enters mutateSessionData/viewSessionData (Go's
// mutex is not reentrant). Returns whatever fn returns so a caller can
// propagate a validation error from within the critical section.
func (c *Coordinator) mutateSessionData(fn func(*SessionData) error) error {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	if c.sessionData == nil {
		c.sessionData = &SessionData{CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	}
	return fn(c.sessionData)
}

// viewSessionData runs fn against the live session state under a read lock.
// Use it for read-only access to fields that concurrent writers mutate; fn must
// not mutate the state or perform I/O.
func (c *Coordinator) viewSessionData(fn func(*SessionData)) {
	c.sessionMu.RLock()
	defer c.sessionMu.RUnlock()
	if c.sessionData == nil {
		return
	}
	fn(c.sessionData)
}

// persistSession saves the live session state, holding sessionMu for the
// duration of SaveSession so the json.Marshal inside it cannot race a
// concurrent writer. The session store is resolved before the lock is taken:
// SessionStore() acquires c.mu.RLock internally, and nesting it under the
// sessionMu write lock would deadlock against any c.mu holder that touches
// session state. Callers must NOT already hold sessionMu.
func (c *Coordinator) persistSession(wrap string) error {
	if c.session == nil {
		return fmt.Errorf("%s: coordinator session is unavailable", wrap)
	}
	store := c.SessionStore()
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	if c.sessionData == nil {
		return nil
	}
	if err := store.SaveSession(c.session.Workspace, c.sessionData); err != nil {
		return fmt.Errorf("%s: %w", wrap, err)
	}
	return nil
}
