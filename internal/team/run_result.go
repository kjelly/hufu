package team

import (
	"fmt"

	"github.com/anomalyco/hufu/internal/agent"
)

type RunOutcome string

const (
	RunOutcomeCompleted RunOutcome = "completed"
	RunOutcomePartial   RunOutcome = "partial"
	RunOutcomeBlocked   RunOutcome = "blocked"
	RunOutcomeFailed    RunOutcome = "failed"
	RunOutcomeCancelled RunOutcome = "cancelled"
)

func (r RunOutcome) String() string {
	return string(r)
}

func IsRunOutcomeSuccess(outcome RunOutcome) bool {
	return outcome == RunOutcomeCompleted
}

type AcceptanceSpec = agent.AcceptanceSpec

// cloneAcceptanceSpec detaches every caller-owned slice from the acceptance
// contract. AcceptanceSpec is part of the run's immutable contract after it is
// accepted; copying only the struct would leave Commands and
// RequiredArtifacts vulnerable to out-of-band mutation through aliases.
func cloneAcceptanceSpec(spec AcceptanceSpec) AcceptanceSpec {
	clone := spec
	if spec.Commands != nil {
		clone.Commands = append([]string(nil), spec.Commands...)
	}
	if spec.RequiredArtifacts != nil {
		clone.RequiredArtifacts = append([]string(nil), spec.RequiredArtifacts...)
	}
	return clone
}

type AcceptanceResult struct {
	Passed            bool     `json:"passed"`
	Errors            []string `json:"errors,omitempty"`
	Commands          []string `json:"commands,omitempty"`
	RequiredArtifacts []string `json:"required_artifacts,omitempty"`
}

type TaskReference struct {
	ID     string `json:"id"`
	Agent  string `json:"agent,omitempty"`
	Desc   string `json:"desc"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type ContinuationInfo struct {
	TurnCount int    `json:"turn_count"`
	MaxTurns  int    `json:"max_turns"`
	Reason    string `json:"reason,omitempty"`
}

// ContinuationCheckpoint is the durable handoff point for a coordinator
// continuation. It is intentionally small so a restart can identify whether
// a continuation was interrupted without replaying the model transcript.
type ContinuationCheckpoint struct {
	TurnCount int    `json:"turn_count"`
	MaxTurns  int    `json:"max_turns"`
	Reason    string `json:"reason,omitempty"`
	Status    string `json:"status"` // pending, resumed, completed, aborted
}

// AcceptanceContractRevision records an immutable acceptance-contract change.
type AcceptanceContractRevision struct {
	Revision  int             `json:"revision"`
	Timestamp string          `json:"timestamp"`
	OldSpec   *AcceptanceSpec `json:"old_spec,omitempty"`
	NewSpec   AcceptanceSpec  `json:"new_spec"`
	Reason    string          `json:"reason,omitempty"`
}

// RunMetrics is a queryable snapshot of reliability counters for a run.
type RunMetrics struct {
	RetriesByFailureClass map[TaskFailureClass]int `json:"retries_by_failure_class,omitempty"`
	Compactions           int                      `json:"compactions"`
}

type TaskResolution struct {
	Status     string        `json:"status"` // "unresolved", "superseded", "reconciled", "waived"
	ResolvedBy string        `json:"resolved_by,omitempty"`
	Reason     string        `json:"reason,omitempty"`
	Evidence   []EvidenceRef `json:"evidence,omitempty"`
}

type RunStats struct {
	TasksTotal      int `json:"tasks_total"`
	TasksDone       int `json:"tasks_done"`
	TasksUnresolved int `json:"tasks_unresolved"`
	AttemptsTotal   int `json:"attempts_total"`
	AttemptsFailed  int `json:"attempts_failed"`
}

type RunResult struct {
	Outcome         RunOutcome        `json:"outcome"`
	GoalSatisfied   bool              `json:"goal_satisfied"`
	Response        string            `json:"response"`
	Reason          string            `json:"reason,omitempty"`
	ExitCode        int               `json:"exit_code,omitempty"`
	Acceptance      *AcceptanceResult `json:"acceptance,omitempty"`
	UnresolvedTasks []TaskReference   `json:"unresolved_tasks,omitempty"`
	Continuation    *ContinuationInfo `json:"continuation,omitempty"`
	Stats           RunStats          `json:"stats"`
	Metrics         RunMetrics        `json:"metrics,omitempty"`
}

type TaskFailureClass string

const (
	FailureExecution TaskFailureClass = "execution"
	FailureProtocol  TaskFailureClass = "protocol"
	FailureVerify    TaskFailureClass = "verification"
	FailurePolicy    TaskFailureClass = "policy"
	FailureTimeout   TaskFailureClass = "timeout"
)

// SummarizeRunStats aggregates canonical statistics over a slice of TodoItems.
func SummarizeRunStats(items []*TodoItem) RunStats {
	stats := RunStats{}
	for _, item := range items {
		if item == nil {
			continue
		}
		stats.TasksTotal++
		stats.AttemptsTotal += 1 + item.Retries
		// Every retry represents a failed attempt, including a task that
		// eventually succeeded. The final attempt is failed as well when the
		// task remains in an error/blocked state.
		stats.AttemptsFailed += item.Retries
		switch item.Status {
		case TaskDone:
			stats.TasksDone++
		case TaskError, TaskBlocked:
			if item.Resolution != nil && (item.Resolution.Status == "superseded" || item.Resolution.Status == "reconciled" || item.Resolution.Status == "waived") {
				// Resolved failure, not counted as unresolved task
			} else {
				stats.TasksUnresolved++
				stats.AttemptsFailed++
			}
		case TaskSkipped:
		default:
			if item.Status == TaskPending || item.Status == TaskInProgress || item.Status == TaskPlanned || item.Status == TaskVerifying || item.Status == TaskPaused || item.Status == TaskProtocolIncomplete {
				stats.TasksUnresolved++
			}
		}
	}
	return stats
}

// toTaskReference converts a TodoItem to a TaskReference.
func toTaskReference(item *TodoItem) TaskReference {
	if item == nil {
		return TaskReference{}
	}
	errStr := item.Detail
	if errStr == "" && item.TypedResult != nil && item.TypedResult.Summary != "" {
		errStr = item.TypedResult.Summary
	}
	return TaskReference{
		ID:     item.ID,
		Agent:  item.Agent,
		Desc:   item.Desc,
		Status: string(item.Status),
		Error:  errStr,
	}
}

// toTaskReferences converts a slice of TodoItems to TaskReferences.
func toTaskReferences(items []*TodoItem) []TaskReference {
	if len(items) == 0 {
		return nil
	}
	refs := make([]TaskReference, 0, len(items))
	for _, item := range items {
		if item != nil {
			refs = append(refs, toTaskReference(item))
		}
	}
	return refs
}

// ValidateResolution checks a TaskResolution for validity, evidence requirements, and N-node cycle prevention.
func ValidateResolution(resolution *TaskResolution, itemID string, allItems []*TodoItem, runID string) error {
	if resolution == nil {
		return nil
	}
	switch resolution.Status {
	case "unresolved", "superseded", "reconciled", "waived":
	default:
		return fmt.Errorf("invalid resolution status %q", resolution.Status)
	}

	// 1. The target item being resolved MUST be in terminal failed or blocked status (TaskError / TaskBlocked)
	var targetItem *TodoItem
	for _, it := range allItems {
		if it != nil && it.ID == itemID {
			targetItem = it
			break
		}
	}
	if targetItem != nil {
		if targetItem.Status != TaskError && targetItem.Status != TaskBlocked {
			return fmt.Errorf("task %s has status %q; only failed or blocked tasks can be resolved", itemID, targetItem.Status)
		}
	}

	if resolution.Status == "superseded" || resolution.Status == "reconciled" {
		if resolution.ResolvedBy == "" {
			return fmt.Errorf("resolution status %q requires resolved_by task ID", resolution.Status)
		}
		if resolution.ResolvedBy == itemID {
			return fmt.Errorf("task %s cannot resolve itself", itemID)
		}

		// 2. Resolver task MUST exist and be in TaskDone status
		var resolver *TodoItem
		for _, it := range allItems {
			if it != nil && it.ID == resolution.ResolvedBy {
				resolver = it
				break
			}
		}
		if resolver == nil {
			return fmt.Errorf("resolving task %s not found in todo list", resolution.ResolvedBy)
		}
		if resolver.Status != TaskDone {
			return fmt.Errorf("resolving task %s must be done (current status: %s)", resolution.ResolvedBy, resolver.Status)
		}

		// 3. Objective evidence check: resolver task MUST have passed objective verification (VerifyResult exit code 0) or contain verified TypedResult evidence with a valid system HMAC signature. Model claims or un-signed self-authored evidenceRefs are rejected.
		hasVerification := resolver.VerifyResult != nil && resolver.VerifyResult.ExitCode == 0
		hasTypedEvidence := false
		if resolver.TypedResult != nil && len(resolver.TypedResult.Evidence) > 0 {
			sec, err := GetSystemSecret()
			if err == nil && sec != "" {
				for _, ev := range resolver.TypedResult.Evidence {
					if VerifyEvidenceSignature(ev, sec, resolver.ID, runID) {
						hasTypedEvidence = true
						break
					}
				}
			}
		}
		if !hasVerification && !hasTypedEvidence {
			return fmt.Errorf("resolving task %s lacks objective verification evidence (must have passing verify result or system-signed evidence signature)", resolution.ResolvedBy)
		}

		// 4. Graph Cycle Check (N-node cycle traversal starting from resolver.ID)
		visited := map[string]bool{itemID: true}
		currID := resolution.ResolvedBy
		for currID != "" {
			if visited[currID] {
				return fmt.Errorf("resolution cycle detected involving task %s", currID)
			}
			visited[currID] = true
			var currItem *TodoItem
			for _, it := range allItems {
				if it != nil && it.ID == currID {
					currItem = it
					break
				}
			}
			if currItem != nil && currItem.Resolution != nil && (currItem.Resolution.Status == "superseded" || currItem.Resolution.Status == "reconciled") {
				currID = currItem.Resolution.ResolvedBy
			} else {
				break
			}
		}
	}
	return nil
}
