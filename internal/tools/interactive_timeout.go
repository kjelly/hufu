//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/waitbudget"
)

// WithInteractiveAwareTimeout excludes interactive prompts and provider quota
// cooldowns from working time. Parent cancellation and run deadlines still apply.
func WithInteractiveAwareTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := waitbudget.WithTimeout(parent, timeout)
	interactiveWait.mu.Lock()
	if interactiveWait.budgets == nil {
		interactiveWait.budgets = make(map[context.Context]func())
	}
	var resume func()
	if askUserActive.Load() > 0 {
		resume = waitbudget.Pause(ctx)
	}
	interactiveWait.budgets[ctx] = resume
	interactiveWait.mu.Unlock()
	release := sync.OnceFunc(func() {
		interactiveWait.mu.Lock()
		defer interactiveWait.mu.Unlock()
		if resume := interactiveWait.budgets[ctx]; resume != nil {
			resume()
		}
		delete(interactiveWait.budgets, ctx)
	})
	stop := context.AfterFunc(ctx, release)
	return ctx, func() { cancel(); release(); stop() }
}
