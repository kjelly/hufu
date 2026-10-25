package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/waitbudget"
)

var _ agent.ProviderRequestRecovery = providerRequestAdmission{}

func (a providerRequestAdmission) CanRecoverProviderRequest(request agent.ProviderRequest) bool {
	return a.c != nil && a.c.providerManager.SupportsProviderQuota(request.ModelID)
}

func (a providerRequestAdmission) WaitProviderRequest(ctx context.Context, request agent.ProviderRequest) (func(bool) error, error) {
	if a.c == nil || a.c.providerManager == nil {
		return nil, nil
	}
	waitCtx, cancel := a.quotaWaitContext(ctx)
	finish, err := a.c.providerManager.WaitProviderQuota(waitCtx, request.ModelID, providerQuotaJournal(a))
	if finish == nil {
		cancel()
		return nil, err
	}
	return func(accepted bool) error { defer cancel(); return finish(accepted) }, err
}

func (a providerRequestAdmission) RecoverProviderRequest(ctx context.Context, request agent.ProviderRequest, cause error) (bool, error) {
	if a.c == nil || a.c.providerManager == nil {
		return false, nil
	}
	waitCtx, cancel := a.quotaWaitContext(ctx)
	defer cancel()
	return a.c.providerManager.RecoverProviderQuota(waitCtx, request.ModelID, cause, providerQuotaJournal(a))
}

// A working-time pause must never extend the operator's wall-clock budget.
// The usual budget checks run at step boundaries, so a long quota wait needs
// its own ordinary deadline rather than waiting for the next model step.
func (a providerRequestAdmission) quotaWaitContext(ctx context.Context) (context.Context, context.CancelFunc) {
	owner := a.c.tokenBudgetRoot()
	if owner != nil {
		if limit := owner.Budget().Limits().MaxWallClock; limit > 0 {
			return waitbudget.WithWallDeadline(ctx, owner.sessionTime.Add(limit))
		}
	}
	return ctx, func() {}
}

type providerQuotaJournal struct{ c *Coordinator }

// This metadata event does not transition a task: the same attempt is waiting
// for its next model step, and completed tool effects remain untouched.
type providerQuotaState struct {
	Identity string    `json:"identity"`
	Until    time.Time `json:"until"`
}

func (s providerQuotaJournal) Load(ctx context.Context, identity string) (time.Time, error) {
	if !s.c.hasDurableEventJournal() {
		return time.Time{}, nil
	}
	events, err := queryJournalEvents(ctx, s.c.EventJournal(), EventQuery{Types: []string{string(EventProviderQuotaCooldown)}, BranchID: s.c.activeBranchID()})
	if err != nil {
		return time.Time{}, err
	}
	var until time.Time
	for _, event := range events {
		var state providerQuotaState
		if err := json.Unmarshal(event.Payload, &state); err != nil {
			return until, fmt.Errorf("decode provider quota cooldown: %w", err)
		}
		if state.Identity == identity {
			until = state.Until
		}
	}
	return until, nil
}

func (s providerQuotaJournal) Save(ctx context.Context, identity string, until time.Time) error {
	if s.c.hasDurableEventJournal() {
		payload, err := json.Marshal(providerQuotaState{Identity: identity, Until: until})
		if err != nil {
			return err
		}
		_, err = s.c.EventJournal().Append(ctx, RunEvent{Type: string(EventProviderQuotaCooldown), Actor: "runtime", BranchID: s.c.activeBranchID(), RunID: s.c.executionRunID, Payload: payload})
		if err != nil {
			return err
		}
	}
	message := "Ollama quota cooldown cleared; continuing model requests"
	if !until.IsZero() {
		message = "Ollama quota limited; next check at " + until.UTC().Format(time.RFC3339)
	}
	s.c.report(s.c.newEvent("provider_quota").withMessage(message))
	return nil
}
