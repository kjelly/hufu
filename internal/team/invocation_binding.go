package team

import (
	"context"
	"errors"
	"sync/atomic"
)

// InvocationIdentity is allocated by the runtime before model execution.
// External orchestration may bind it durably without manufacturing child IDs.
type InvocationIdentity struct {
	InvocationID string `json:"invocation_id"`
	RunID        string `json:"run_id"`
	BranchID     string `json:"branch_id"`
}

type invocationBindingKey struct{}
type invocationBinding struct {
	bind  func(context.Context, InvocationIdentity) error
	state atomic.Uint32 // 0=new, 1=binding, 2=admitted, 3=closed
}

var errInvocationUnbound = errors.New("provider admission requires a committed invocation binding")

// WithInvocationBinding fences every provider consumer of one public runtime
// invocation. The callback runs after the canonical run/branch identity exists
// and before provider startup, run-input resolution, or worker dispatch.
// The context is single-use; each later invocation needs a fresh binding.
func WithInvocationBinding(ctx context.Context, bind func(context.Context, InvocationIdentity) error) context.Context {
	return context.WithValue(ctx, invocationBindingKey{}, &invocationBinding{bind: bind})
}

func claimInvocationBinding(ctx context.Context) (*invocationBinding, error) {
	binding, _ := ctx.Value(invocationBindingKey{}).(*invocationBinding)
	if binding == nil {
		return nil, nil
	}
	if binding.bind == nil || !binding.state.CompareAndSwap(0, 1) {
		return nil, errors.New("invocation binding is missing or already used")
	}
	return binding, nil
}

func requireInvocationBinding(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	binding, _ := ctx.Value(invocationBindingKey{}).(*invocationBinding)
	if binding != nil && binding.state.Load() != 2 {
		return errInvocationUnbound
	}
	return nil
}

func (c *Coordinator) commitInvocationBinding(ctx context.Context, binding *invocationBinding) error {
	if err := c.checkRunAdmission(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.executionEventsMu.Lock()
	runID := c.executionRunID
	c.executionEventsMu.Unlock()
	identity := InvocationIdentity{InvocationID: c.currentInvocationID(), RunID: runID, BranchID: c.activeBranchID()}
	if identity.InvocationID == "" || identity.RunID == "" || identity.BranchID == "" || !c.hasDurableEventJournal() {
		return errors.New("invocation binding requires durable runtime identity")
	}
	if err := c.verifyInvocationBindingIdentity(ctx, identity); err != nil {
		return err
	}
	if err := binding.bind(ctx, identity); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	binding.state.Store(2)
	return nil
}

func (c *Coordinator) verifyInvocationBindingIdentity(ctx context.Context, identity InvocationIdentity) error {
	journal := c.EventJournal()
	if err := journal.VerifyHashChain(ctx); err != nil {
		return err
	}
	events, err := journal.ReadEvents(ctx)
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.Type == "run_started" && event.RunID == identity.RunID && event.InvocationID == identity.InvocationID && event.BranchID == identity.BranchID {
			return nil
		}
	}
	return errors.New("invocation identity has no durable run_started event")
}
