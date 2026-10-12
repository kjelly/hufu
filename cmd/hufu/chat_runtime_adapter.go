package main

import (
	"context"
	"errors"
	"sync"

	"github.com/kjelly/hufu/internal/conversation"
)

var errChatControlUnavailable = errors.New("not available in this chat session")

// chatRuntimeAdapter implements conversation.RuntimePort on top of a loaded
// teamContext. RunTurn, ResetContext, and SwitchTeam run on the controller's
// actor goroutine; RequestWrapUp and Current may be called from any goroutine.
type chatRuntimeAdapter struct {
	mu       sync.Mutex
	tc       *teamContext
	teamName string
	seq      chatTurnSequencer

	newDeps func(tc *teamContext, teamName string) chatTurnDeps
	// resetHook and switchHook implement the P3 control commands; nil means
	// the command is unavailable.
	resetHook  func(ctx context.Context, tc *teamContext) error
	switchHook func(ctx context.Context, current *teamContext, name string) (*teamContext, error)
}

func newChatRuntimeAdapter(tc *teamContext, teamName string) *chatRuntimeAdapter {
	return &chatRuntimeAdapter{tc: tc, teamName: teamName, newDeps: newChatTurnDeps}
}

// Current returns the team context the adapter currently drives.
func (a *chatRuntimeAdapter) Current() (*teamContext, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tc, a.teamName
}

func (a *chatRuntimeAdapter) RunTurn(ctx context.Context, turnID, prompt string) conversation.TurnResult {
	tc, teamName := a.Current()
	outcome := runChatTurn(ctx, a.newDeps(tc, teamName), &a.seq, turnID, prompt)
	runIDs := make([]string, 0, len(outcome.RunIdentities))
	for _, identity := range outcome.RunIdentities {
		runIDs = append(runIDs, identity.RunID)
	}
	return conversation.TurnResult{
		Answer:    outcome.Answer,
		Result:    outcome.Result,
		Err:       outcome.Err,
		Cancelled: outcome.Cancelled,
		RunIDs:    runIDs,
	}
}

func (a *chatRuntimeAdapter) RequestWrapUp() {
	tc, _ := a.Current()
	if tc != nil && tc.coordinator != nil {
		tc.coordinator.SetWrapUp()
	}
}

func (a *chatRuntimeAdapter) ResetContext(ctx context.Context) error {
	if a.resetHook == nil {
		return errChatControlUnavailable
	}
	tc, _ := a.Current()
	if err := a.resetHook(ctx, tc); err != nil {
		return err
	}
	a.seq.reset()
	return nil
}

func (a *chatRuntimeAdapter) SwitchTeam(ctx context.Context, name string) error {
	if a.switchHook == nil {
		return errChatControlUnavailable
	}
	current, _ := a.Current()
	next, err := a.switchHook(ctx, current, name)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.tc, a.teamName = next, next.teamName
	a.mu.Unlock()
	a.seq.reset()
	return nil
}
