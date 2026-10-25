//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"time"

	"github.com/kjelly/hufu/internal/waitbudget"
)

// WithInteractiveAwareTimeout excludes interactive prompts and provider quota
// cooldowns from working time. Parent cancellation and run deadlines still apply.
func WithInteractiveAwareTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return waitbudget.WithTimeout(parent, timeout, InteractiveWaitTotal)
}
