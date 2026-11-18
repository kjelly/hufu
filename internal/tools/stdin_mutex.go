package tools

import (
	"context"
	"sync"
)

// PromptMutex preserves Lock/Unlock for terminal owners and permits a queued
// prompt to leave on cancellation without starting a second stdin reader.
type PromptMutex struct {
	once sync.Once
	gate chan struct{}
}

func (m *PromptMutex) init()   { m.once.Do(func() { m.gate = make(chan struct{}, 1) }) }
func (m *PromptMutex) Lock()   { m.init(); m.gate <- struct{}{} }
func (m *PromptMutex) Unlock() { <-m.gate }
func (m *PromptMutex) LockContext(ctx context.Context) error {
	m.init()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			m.Unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
