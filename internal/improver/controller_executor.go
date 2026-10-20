package improver

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/kjelly/hufu/internal/improve/paired"
)

type StageRequest struct {
	Lease
	Spec     StageSpec
	Deadline time.Time
	Limits   paired.Limits
}

type Reconciliation struct {
	// completed includes positive proof that all child workers/callbacks
	// stopped. running and unknown never authorize redispatch.
	State      string
	Settlement Settlement
}

// StageExecutor owns child lifecycle and evidence verification. Execute must
// call beforeProvider with the runtime's actual identity before ANY provider
// admission, including implicit model work. It must cancel and drain workers
// and callbacks before returning, and must not close parent-owned resources.
// Reconcile reads canonical child evidence; it must never dispatch a model.
type StageExecutor interface {
	Execute(ctx context.Context, request StageRequest, beforeProvider func(paired.RuntimeIdentity) error) (Settlement, error)
	Reconcile(ctx context.Context, request StageRequest) (Reconciliation, error)
}

func (c *Controller) stageRequest(ctx context.Context, lease Lease) (StageRequest, error) {
	state, err := c.Read(ctx)
	if err != nil {
		return StageRequest{}, err
	}
	stage, attempt, err := currentAttempt(&state, lease)
	if err != nil {
		return StageRequest{}, err
	}
	deadline := attempt.AdmittedAt.Add(time.Duration(c.plan.Limits.PerStageSeconds) * time.Second)
	if state.Deadline.Before(deadline) {
		deadline = state.Deadline
	}
	return StageRequest{Lease: lease, Spec: stage.Spec, Deadline: deadline, Limits: c.plan.Limits}, nil
}

// Execute admits exactly one fresh child. An error after admission is never an
// invitation to call Execute again: Resume must reconcile that durable lease.
func (c *Controller) Execute(ctx context.Context, stageID string, executor StageExecutor) error {
	if executor == nil {
		return errors.New("executor unavailable")
	}
	lease, err := c.Admit(ctx, stageID)
	if err != nil {
		return err
	}
	request, err := c.stageRequest(ctx, lease)
	if err != nil {
		return err
	}
	child, cancel := context.WithDeadline(ctx, request.Deadline)
	defer cancel()
	// Cancellation revokes future admission immediately, even while Execute
	// drains. Join the callback before returning to the store/lease owner.
	stopped := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer done()
		stopped <- c.Stop(cleanup, "cancel_requested")
	})
	result, runErr := executor.Execute(child, request, func(identity paired.RuntimeIdentity) error {
		if err := child.Err(); err != nil {
			return err
		}
		if err := c.Bind(child, lease, identity); err != nil {
			return err
		}
		return c.Start(child, lease)
	})
	var stopErr error
	if !stop() {
		stopErr = <-stopped
	}
	cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer done()
	if result.Stopped && result.Evidence.Digest != "" {
		return errors.Join(runErr, stopErr, c.Complete(cleanup, lease, result))
	}
	// Missing/still-running evidence retains the complete reservation. The
	// child adapter's returned error is diagnostic only, never cost evidence.
	return errors.Join(runErr, stopErr, c.Transition(cleanup, stageID, lease.Generation, "blocked", "reconciliation_required"),
		errors.New("child outcome requires reconciliation"))
}

// Reconcile imports completed evidence once, retaining all attempts and costs.
// It never infers "not sent" from an absent started event.
func (c *Controller) Reconcile(ctx context.Context, executor StageExecutor) error {
	if executor == nil {
		return errors.New("executor unavailable")
	}
	state, err := c.Read(ctx)
	if err != nil {
		return err
	}
	specs, _, err := plannedStages(c.plan)
	if err != nil {
		return err
	}
	var failures []error
	for _, spec := range specs {
		stage := state.Stages[spec.ID]
		if len(stage.Attempts) == 0 || slices.Contains([]string{"completed", "cancelled", "failed", "skipped_budget"}, stage.State) {
			continue
		}
		attempt := stage.Attempts[len(stage.Attempts)-1]
		if attempt.Reason == "completion_conflict" {
			failures = append(failures, ErrCompletionConflict)
			continue
		}
		request, err := c.stageRequest(ctx, attempt.Lease)
		if err != nil {
			return err
		}
		observation, err := executor.Reconcile(ctx, request)
		if err == nil && observation.State == "completed" {
			err = c.Complete(ctx, attempt.Lease, observation.Settlement)
		} else {
			err = errors.Join(err, c.Transition(ctx, spec.ID, attempt.Generation, "blocked", "reconciliation_required"),
				errors.New("child is running or its dispatch/usage is unknown"))
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
