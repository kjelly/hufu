package team

import (
	"context"
	"fmt"
	"strings"
)

const (
	RepairRetry                  = RepairActionRetry
	RepairEscalate  RepairAction = "escalate"
	RepairReconcile              = RepairActionReconcile
	RepairReplan                 = RepairActionReplan
	RepairRollback               = RepairActionRollback
	RepairBlock                  = RepairActionBlock
)

type RepairDecision struct {
	Action RepairAction `json:"action"`
	Reason string       `json:"reason"`
}

type RepairResult struct {
	Decision RepairDecision `json:"decision"`
	State    string         `json:"state,omitempty"`
	Attempt  int            `json:"attempt,omitempty"`
	Err      error          `json:"-"`
}

// RepairRequest contains coordinator-owned callbacks. The controller never
// invents tools, paths, or rollback commands; it only chooses a safe action
// and enforces checkpoint-before-action ordering.
type RepairRequest struct {
	Task              TaskDef
	Failure           error
	Attempt           int
	MaxAttempts       int
	RecoveryState     string
	BudgetExhausted   bool
	AllowRollback     bool
	RollbackRequested bool

	Checkpoint func(context.Context) error
	Retry      func(context.Context) error
	Escalate   func(context.Context) error
	Reconcile  func(context.Context) (string, error)
	Replan     func(context.Context) error
	Rollback   func(context.Context) error
	Observe    func(RepairDecision)
}

// RepairController centralizes safe recovery selection while retaining the
// existing retry/reconcile/replan implementations behind callbacks.
type RepairController struct{}

func NewRepairController() *RepairController { return &RepairController{} }

func (r *RepairController) Decide(req RepairRequest) RepairDecision {
	task := req.Task
	if req.BudgetExhausted {
		return RepairDecision{Action: RepairBlock, Reason: "repair budget exhausted"}
	}
	if req.RollbackRequested {
		if req.AllowRollback {
			return RepairDecision{Action: RepairRollback, Reason: "authorized rollback requested"}
		}
		return RepairDecision{Action: RepairBlock, Reason: "rollback is not explicitly authorized"}
	}
	if task.Execution.AllowsReplay != nil && !*task.Execution.AllowsReplay {
		if task.ReconcileTool != "" || task.Recovery == RecoveryReconcile {
			return RepairDecision{Action: RepairReconcile, Reason: "task is not replayable; reconcile before retry"}
		}
		return RepairDecision{Action: RepairBlock, Reason: "task is not replayable and has no reconcile path"}
	}
	if task.Recovery == RecoveryManual || task.Recovery == RecoveryNever {
		return RepairDecision{Action: RepairBlock, Reason: "task recovery requires human intervention"}
	}
	if task.SideEffect == SideEffectExternalWrite || task.SideEffect == SideEffectInfraMutation || task.SideEffect == SideEffectCredential {
		if (task.Recovery == RecoveryReconcile || task.ReconcileTool != "") && req.RecoveryState != RecoveryStateNotStarted {
			return RepairDecision{Action: RepairReconcile, Reason: "external side effect requires reconcile before replay"}
		}
		if req.RecoveryState == RecoveryStateNotStarted && (task.Recovery == RecoveryReconcile || task.ReconcileTool != "") {
			if req.MaxAttempts > 0 && req.Attempt >= req.MaxAttempts {
				return RepairDecision{Action: RepairReplan, Reason: "task attempt budget exhausted; construct a new plan"}
			}
			if task.Escalate {
				return RepairDecision{Action: RepairEscalate, Reason: "reconciliation proved the operation did not start; escalation is permitted"}
			}
			return RepairDecision{Action: RepairRetry, Reason: "reconciliation proved the operation did not start"}
		}
		return RepairDecision{Action: RepairBlock, Reason: "external side effect has no safe reconcile path"}
	}
	if req.MaxAttempts > 0 && req.Attempt >= req.MaxAttempts {
		return RepairDecision{Action: RepairReplan, Reason: "task attempt budget exhausted; construct a new plan"}
	}
	if (task.Recovery == RecoveryReconcile || task.ReconcileTool != "") && req.RecoveryState != RecoveryStateNotStarted {
		return RepairDecision{Action: RepairReconcile, Reason: "task declares reconcile recovery"}
	}
	if task.Escalate {
		return RepairDecision{Action: RepairEscalate, Reason: "task permits model escalation on retry"}
	}
	return RepairDecision{Action: RepairRetry, Reason: "retry is permitted by task recovery policy"}
}

func (r *RepairController) Execute(ctx context.Context, req RepairRequest) RepairResult {
	decision := r.Decide(req)
	if req.Observe != nil {
		req.Observe(decision)
	}
	result := RepairResult{Decision: decision, Attempt: req.Attempt}
	if decision.Action == RepairBlock {
		result.State = "blocked"
		result.Err = fmt.Errorf("repair blocked: %s", decision.Reason)
		return result
	}
	if req.Checkpoint == nil {
		result.State = "blocked"
		result.Err = fmt.Errorf("repair blocked: checkpoint callback is required")
		return result
	}
	if err := req.Checkpoint(ctx); err != nil {
		result.State = "blocked"
		result.Err = fmt.Errorf("repair checkpoint failed: %w", err)
		return result
	}

	switch decision.Action {
	case RepairRetry:
		result.Err = runRepairCallback(ctx, req.Retry, "retry")
		result.State = stateFromRepairError(result.Err, "executing")
	case RepairEscalate:
		result.Err = runRepairCallback(ctx, req.Escalate, "escalation")
		result.State = stateFromRepairError(result.Err, "executing")
	case RepairReconcile:
		if req.Reconcile == nil {
			result.State = "blocked"
			result.Err = fmt.Errorf("repair blocked: reconcile callback is required")
			return result
		}
		state, err := req.Reconcile(ctx)
		result.State, result.Err = strings.TrimSpace(state), err
		if result.State == "" {
			result.State = RecoveryStateUnknown
		}
		if result.Err != nil || result.State == RecoveryStateUnknown || result.State == RecoveryStatePartial {
			if result.Err == nil {
				result.Err = fmt.Errorf("reconcile state is %s", result.State)
			}
			result.State = "blocked"
		}
	case RepairReplan:
		result.Err = runRepairCallback(ctx, req.Replan, "replan")
		result.State = stateFromRepairError(result.Err, "planned")
	case RepairRollback:
		if !req.AllowRollback || req.Rollback == nil {
			result.State = "blocked"
			result.Err = fmt.Errorf("repair blocked: rollback is not explicitly authorized")
			return result
		}
		result.Err = req.Rollback(ctx)
		result.State = stateFromRepairError(result.Err, "rolled_back")
	default:
		result.State = "blocked"
		result.Err = fmt.Errorf("repair blocked: unsupported action %q", decision.Action)
	}
	return result
}

func runRepairCallback(ctx context.Context, callback func(context.Context) error, name string) error {
	if callback == nil {
		return fmt.Errorf("%s callback is required", name)
	}
	return callback(ctx)
}

func stateFromRepairError(err error, success string) string {
	if err != nil {
		return "blocked"
	}
	return success
}
