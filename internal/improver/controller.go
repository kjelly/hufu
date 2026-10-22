package improver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/team"
)

var (
	ErrAlreadyAdmitted    = errors.New("stage already admitted; reconcile instead of dispatching again")
	ErrCompletionConflict = errors.New("conflicting stage completion")
)

// StoreJournal adapts the canonical runtime journal without a coordinator or
// provider. The caller owns the store and closes it after controller drain.
type StoreJournal struct{ Store *team.EventStore }

func (j StoreJournal) Append(ctx context.Context, event team.RunEvent) (team.RunEvent, error) {
	return j.Store.AppendPersistedContext(ctx, event)
}
func (j StoreJournal) AppendAtHead(ctx context.Context, event team.RunEvent, head string) (team.RunEvent, error) {
	return j.Store.AppendPersistedAtHead(ctx, event, head)
}
func (j StoreJournal) ReadEvents(ctx context.Context) ([]team.RunEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return j.Store.ReadEvents()
}
func (j StoreJournal) VerifyHashChain(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return j.Store.VerifyHashChain()
}

type Clock interface{ Now() time.Time }
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type Objects interface {
	PutJSON(string, any) (paired.ArtifactRef, error)
	Read(paired.ArtifactRef) ([]byte, error)
}

// Controller is the only writer of its improvement namespace. The local mutex
// protects its monotonic deadline; global-head CAS protects other processes.
type Controller struct {
	mu            sync.Mutex
	journal       team.ConditionalEventJournal
	objects       Objects
	scope         LedgerScope
	plan          paired.ExperimentPlan
	clock         Clock
	localDeadline time.Time
}

func NewController(journal team.ConditionalEventJournal, objects Objects, scope LedgerScope, plan paired.ExperimentPlan, clock Clock) (*Controller, error) {
	if journal == nil || objects == nil {
		return nil, errors.New("controller requires conditional journal and objects")
	}
	if _, err := ReplayLedger(nil, scope, plan); err != nil {
		return nil, err
	}
	raw, err := paired.CanonicalJSON(plan)
	if err != nil {
		return nil, err
	}
	var frozen paired.ExperimentPlan
	if err := json.Unmarshal(raw, &frozen, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	if clock == nil {
		clock = systemClock{}
	}
	return &Controller{journal: journal, objects: objects, scope: scope, plan: frozen, clock: clock}, nil
}

func (c *Controller) Read(ctx context.Context) (LedgerState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, _, err := c.read(ctx)
	return state, err
}

func (c *Controller) read(ctx context.Context) (LedgerState, []team.RunEvent, error) {
	// ReadEvents alone is a cached view. Verification refreshes the shared
	// SQLite chain, so a new process's admission cannot remain invisible.
	if err := c.journal.VerifyHashChain(ctx); err != nil {
		return LedgerState{}, nil, err
	}
	events, err := c.journal.ReadEvents(ctx)
	if err != nil {
		return LedgerState{}, nil, err
	}
	state, err := ReplayLedger(events, c.scope, c.plan)
	if err != nil {
		return LedgerState{}, nil, err
	}
	if state.Created {
		if err := c.verifyObjects(state); err != nil {
			return LedgerState{}, nil, err
		}
		if c.localDeadline.IsZero() {
			now := c.clock.Now()
			c.localDeadline = now.Add(state.Deadline.Sub(now))
		}
	}
	return state, events, nil
}

func (c *Controller) verifyObjects(state LedgerState) error {
	raw, err := c.objects.Read(state.Plan)
	if err != nil {
		return err
	}
	expected, err := paired.CanonicalJSON(c.plan)
	if err != nil || !bytes.Equal(raw, expected) {
		return errors.New("controller: committed plan differs")
	}
	refs := append([]paired.ArtifactRef{c.plan.Config, c.plan.Fixture, c.plan.BaselineSnapshot}, c.plan.Inputs...)
	for _, variant := range state.Variants {
		refs = append(refs, variant.Snapshot)
	}
	for _, stage := range state.Stages {
		for _, attempt := range stage.Attempts {
			if attempt.Settlement != nil {
				refs = append(refs, attempt.Settlement.Evidence)
			}
		}
	}
	if state.Report != nil {
		refs = append(refs, *state.Report)
	}
	for _, ref := range refs {
		if _, err := c.objects.Read(ref); err != nil {
			return err
		}
	}
	return nil
}

type ledgerMutation struct {
	kind    string
	key     string
	payload any
}

func (c *Controller) mutate(ctx context.Context, build func(LedgerState, eventMeta) (ledgerMutation, error)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		state, events, err := c.read(ctx)
		if err != nil {
			return err
		}
		now := c.clock.Now()
		meta := eventMeta{SchemaVersion: 1, ExperimentID: c.scope.ExperimentID, ObservedAt: now.UTC()}
		// Block durably, but still allow evidence reconciliation and settlement
		// under the last valid observation after a wall-clock rollback.
		if state.Created && now.Before(state.ObservedAt) {
			meta.ObservedAt = state.ObservedAt
			if state.StopReason == "" && state.Report == nil {
				err := c.append(ctx, state, events, ledgerMutation{eventStopped, "stop", admissionStopped{meta, "clock_rollback"}})
				if errors.Is(err, team.ErrEventHeadChanged) {
					continue
				}
				if err != nil {
					return err
				}
				continue
			}
		}
		if state.Created && state.Report == nil && state.StopReason == "" && !now.Before(c.localDeadline) {
			err := c.append(ctx, state, events, ledgerMutation{eventStopped, "stop", admissionStopped{meta, "deadline_exceeded"}})
			if errors.Is(err, team.ErrEventHeadChanged) {
				continue
			}
			if err != nil {
				return err
			}
			continue
		}
		change, err := build(state, meta)
		if err != nil || change.kind == "" {
			return err
		}
		err = c.append(ctx, state, events, change)
		if errors.Is(err, team.ErrEventHeadChanged) {
			continue // reread and recompute; never retry the stale append
		}
		return err // any ambiguous acknowledgement returns without dispatch
	}
}

func (c *Controller) append(ctx context.Context, state LedgerState, events []team.RunEvent, change ledgerMutation) error {
	raw, err := paired.CanonicalJSON(change.payload)
	if err != nil {
		return err
	}
	event := team.RunEvent{
		Actor: "improver", Type: change.kind, RunID: c.scope.RunID, SessionID: c.scope.SessionID,
		BranchID: c.scope.BranchID, IdempotencyKey: c.scope.BranchID + ":improvement:" + c.scope.ExperimentID + ":" + change.key,
		Payload: raw,
	}
	if _, err := ReplayLedger(append(slices.Clone(events), event), c.scope, c.plan); err != nil {
		return err
	}
	stored, err := c.journal.AppendAtHead(ctx, event, state.Head)
	if err != nil {
		return err
	}
	// EventStore redaction must not change these content-free contracts.
	return verifyPersistedPayload(raw, stored.Payload)
}

func verifyPersistedPayload(want, got []byte) error {
	// Compare syntax-normalized JSON without floating point conversion.
	aRaw, err := paired.CanonicalJSON(jsontext.Value(want))
	if err != nil {
		return err
	}
	bRaw, err := paired.CanonicalJSON(jsontext.Value(got))
	if err != nil {
		return err
	}
	if !bytes.Equal(aRaw, bRaw) {
		return errors.New("controller: persisted event payload changed")
	}
	return nil
}

func (c *Controller) Create(ctx context.Context) error {
	for _, ref := range append([]paired.ArtifactRef{c.plan.Config, c.plan.Fixture, c.plan.BaselineSnapshot}, c.plan.Inputs...) {
		if _, err := c.objects.Read(ref); err != nil {
			return err
		}
	}
	ref, err := c.objects.PutJSON("experiment_plan", c.plan)
	if err != nil {
		return err
	}
	return c.mutate(ctx, func(state LedgerState, meta eventMeta) (ledgerMutation, error) {
		if state.Created {
			return ledgerMutation{}, errors.New("experiment already exists; use resume")
		}
		return ledgerMutation{eventCreated, "create", runCreated{meta, ref, meta.ObservedAt.Add(time.Duration(c.plan.Limits.PipelineSeconds) * time.Second)}}, nil
	})
}

func (c *Controller) Reserve(ctx context.Context, group string) error {
	return c.mutate(ctx, func(state LedgerState, meta eventMeta) (ledgerMutation, error) {
		if _, exists := state.Envelopes[group]; exists {
			return ledgerMutation{}, nil
		}
		return ledgerMutation{eventReserved, "reserve:" + group, envelopeReserved{meta, group}}, nil
	})
}

func (c *Controller) BindVariant(ctx context.Context, binding VariantBinding) error {
	if _, err := c.objects.Read(binding.Snapshot); err != nil {
		return err
	}
	return c.mutate(ctx, func(state LedgerState, meta eventMeta) (ledgerMutation, error) {
		if old, exists := state.Variants[binding.Slot]; exists && old == binding {
			return ledgerMutation{}, nil
		}
		return ledgerMutation{eventVariant, "variant:" + binding.Slot, variantBound{meta, binding}}, nil
	})
}

func (c *Controller) Admit(ctx context.Context, stageID string) (Lease, error) {
	lease := Lease{StageID: stageID, Owner: rand.Text(), ChildWorkspaceID: rand.Text()}
	err := c.mutate(ctx, func(state LedgerState, meta eventMeta) (ledgerMutation, error) {
		stage := state.Stages[stageID]
		if stage == nil {
			return ledgerMutation{}, errors.New("unknown stage")
		}
		if len(stage.Attempts) != 0 && !retryable(&state, stage) {
			return ledgerMutation{}, ErrAlreadyAdmitted
		}
		lease.Generation = int64(len(stage.Attempts)) + 1
		return ledgerMutation{eventAdmitted, leaseKey(lease, "admit"), stageAdmitted{meta, lease, c.plan.Limits.PerStageTokens}}, nil
	})
	if err != nil {
		return Lease{}, err
	}
	return lease, nil
}

func leaseKey(lease Lease, operation string) string {
	return fmt.Sprintf("%s:%d:%s", lease.StageID, lease.Generation, operation)
}

func (c *Controller) Bind(ctx context.Context, lease Lease, identity paired.RuntimeIdentity) error {
	return c.mutate(ctx, func(state LedgerState, meta eventMeta) (ledgerMutation, error) {
		_, attempt, err := currentAttempt(&state, lease)
		if err != nil {
			return ledgerMutation{}, err
		}
		if attempt.Identity != nil && *attempt.Identity == identity {
			return ledgerMutation{}, nil
		}
		return ledgerMutation{eventBound, leaseKey(lease, "bind"), stageBound{meta, lease, identity}}, nil
	})
}

func (c *Controller) Start(ctx context.Context, lease Lease) error {
	return c.mutate(ctx, func(state LedgerState, meta eventMeta) (ledgerMutation, error) {
		_, attempt, err := currentAttempt(&state, lease)
		if err != nil {
			return ledgerMutation{}, err
		}
		if attempt.State != "bound" {
			return ledgerMutation{}, errors.New("dispatch already authorized or binding unavailable; reconcile")
		}
		return ledgerMutation{eventStarted, leaseKey(lease, "start"), stageStarted{meta, lease}}, nil
	})
}

func (c *Controller) Complete(ctx context.Context, lease Lease, result Settlement) error {
	if _, err := c.objects.Read(result.Evidence); err != nil {
		return err
	}
	conflict := false
	err := c.mutate(ctx, func(state LedgerState, meta eventMeta) (ledgerMutation, error) {
		_, attempt, err := currentAttempt(&state, lease)
		if err != nil {
			return ledgerMutation{}, err
		}
		if old := attempt.Settlement; old != nil {
			if *old == result {
				return ledgerMutation{}, nil
			}
			if old.UsageKnown || !result.UsageKnown || result.Tokens < old.Tokens || result.Outcome != old.Outcome {
				conflict = true
				if attempt.Reason == "completion_conflict" {
					return ledgerMutation{}, nil
				}
				return ledgerMutation{eventTransition, leaseKey(lease, "conflict"), stageTransition{meta, lease.StageID, lease.Generation, "blocked", "completion_conflict"}}, nil
			}
		}
		key := leaseKey(lease, "complete")
		if result.UsageKnown {
			key += ":known"
		}
		return ledgerMutation{eventCompleted, key, stageCompleted{meta, lease, result}}, nil
	})
	if err == nil && conflict {
		return ErrCompletionConflict
	}
	return err
}

func (c *Controller) Transition(ctx context.Context, stageID string, generation int64, state, reason string) error {
	return c.mutate(ctx, func(current LedgerState, meta eventMeta) (ledgerMutation, error) {
		stage := current.Stages[stageID]
		if stage != nil && stage.State == state && stage.Reason == reason {
			return ledgerMutation{}, nil
		}
		key := fmt.Sprintf("%s:%d:%s:%s", stageID, generation, state, reason)
		return ledgerMutation{eventTransition, key, stageTransition{meta, stageID, generation, state, reason}}, nil
	})
}

func (c *Controller) Stop(ctx context.Context, reason string) error {
	return c.mutate(ctx, func(state LedgerState, meta eventMeta) (ledgerMutation, error) {
		if state.StopReason != "" {
			return ledgerMutation{}, nil
		}
		return ledgerMutation{eventStopped, "stop", admissionStopped{meta, reason}}, nil
	})
}

func (c *Controller) Finish(ctx context.Context, report paired.ArtifactRef, decision string) error {
	if _, err := c.objects.Read(report); err != nil {
		return err
	}
	return c.mutate(ctx, func(state LedgerState, meta eventMeta) (ledgerMutation, error) {
		if state.Report != nil && *state.Report == report && state.Decision == decision {
			return ledgerMutation{}, nil
		}
		return ledgerMutation{eventFinished, "finish", runFinished{meta, report, decision}}, nil
	})
}
