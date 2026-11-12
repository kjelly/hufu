package team

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/utils"
)

const EventFinalAcceptanceRetryStarted EventType = "final_acceptance_retry_started"

type finalAcceptanceRetryKey struct{}
type finalAcceptanceRetryRequest struct {
	RunID   string
	Timeout time.Duration
}

// WithFinalAcceptanceRetry selects an explicit, bounded final-review retry.
// It never authorizes new work or changes the frozen execution policy.
func WithFinalAcceptanceRetry(ctx context.Context, runID string, timeout time.Duration) context.Context {
	return context.WithValue(WithSessionResume(ctx), finalAcceptanceRetryKey{}, finalAcceptanceRetryRequest{runID, timeout})
}
func finalAcceptanceRetryRequested(ctx context.Context) bool {
	_, ok := ctx.Value(finalAcceptanceRetryKey{}).(finalAcceptanceRetryRequest)
	return ok
}

// FinalAcceptanceRetryAssessment binds a retry to the exact terminal candidate,
// frozen inputs, source head and task evidence. Publication remains append-only.
type FinalAcceptanceRetryAssessment struct {
	SourceRunID        string `json:"source_run_id"`
	Branch             string `json:"branch"`
	SourceHead         string `json:"source_head"`
	SourceHash         string `json:"source_hash"`
	PolicySHA256       string `json:"policy_sha256"`
	ResponseHash       string `json:"response_hash"`
	InputsHash         string `json:"inputs_hash"`
	TasksHash          string `json:"tasks_hash"`
	Retained           int    `json:"retained"`
	TimeoutSeconds     int64  `json:"timeout_seconds"`
	TimeoutNanoseconds int64  `json:"timeout_nanoseconds"`
	MaxReviewCalls     int    `json:"max_review_calls"`
}

func finalAcceptanceCandidate(source *ResumeSource, request finalAcceptanceRetryRequest) (string, error) {
	if source == nil || source.Session == nil || request.RunID == "" || request.Timeout <= 0 {
		return "", fmt.Errorf("final acceptance retry requires an exact source run and positive timeout")
	}
	session := source.Session
	r := session.RunResult
	if session.WorkflowState != PhaseDone || session.RecoveryRequired || r == nil || r.RunID != request.RunID || (r.StopReason != StopReasonAcceptanceFailed && r.StopReason != StopReasonCancelled) || r.Acceptance == nil || r.Acceptance.RequestAcceptance == nil {
		return "", fmt.Errorf("final acceptance retry requires the latest completed workflow with unavailable request acceptance")
	}
	a := r.Acceptance
	review := a.RequestAcceptance
	retryable := review.ErrorKind == "timeout" || review.ErrorKind == "cancelled"
	// Legacy records lack typed error kinds. Accept only exact runtime-owned
	// transport deadline errors, never infer a verdict from arbitrary prose.
	if review.ErrorKind == "" {
		retryable = review.Error == "request acceptance review: context deadline exceeded" || review.Error == "request acceptance evidence: context deadline exceeded"
	}
	if !retryable {
		return "", fmt.Errorf("only timed-out or cancelled request reviews can be retried")
	}

	if review.Error == "" || len(a.Errors) != 1 || a.Errors[0] != "request acceptance: "+utils.RedactSecrets(review.Error) {
		return "", fmt.Errorf("only an unavailable request review may be retried; other acceptance failures require a follow-up")
	}
	for _, check := range review.Checks {
		if check.Status != "satisfied" {
			return "", fmt.Errorf("negative or unknown acceptance verdicts cannot be retried against the same answer")
		}
	}
	if review.MaterialReview != nil {
		for _, check := range review.MaterialReview.Checks {
			if !check.Adequate {
				return "", fmt.Errorf("negative material verdict cannot be retried against the same answer")
			}
		}
	}
	if len(UnresolvedTaskReferences(session.Tasks)) != 0 || r.Stats.TasksUnresolved != 0 || len(r.UnresolvedTasks) != 0 {
		return "", fmt.Errorf("acceptance-only retry cannot resume unresolved work")
	}
	if r.RunInputs == nil || review.SnapshotID != r.RunInputs.ID || review.SnapshotHash != r.RunInputs.SnapshotHash || ValidateRunInputSnapshot(r.RunInputs) != nil {
		return "", fmt.Errorf("final acceptance frozen input binding changed")
	}
	contract, err := RequestAcceptanceContractFromSnapshot(r.RunInputs)
	if err != nil || !sameCanonicalEvidence(contract, &review.Contract) {
		return "", fmt.Errorf("final acceptance contract differs from frozen inputs")
	}
	return savedAcceptanceResponse(r.Response, review)
}

// AssessFinalAcceptanceRetry uses ordinary resume policy and retained-evidence
// admission, but permits a finished DAG only for this explicit review operation.
func (c *Coordinator) AssessFinalAcceptanceRetry(ctx context.Context, source *ResumeSource, runID string, timeout time.Duration) (*FinalAcceptanceRetryAssessment, error) {
	request := finalAcceptanceRetryRequest{runID, timeout}
	if _, err := finalAcceptanceCandidate(source, request); err != nil {
		return nil, err
	}
	if c.decisionTerminalEnabled() {
		return nil, fmt.Errorf("acceptance-only retry is unavailable for decision terminal preparation")
	}
	if !c.requestAcceptanceEnabled() {
		return nil, fmt.Errorf("request acceptance must remain enabled")
	}
	admission, err := c.AssessResumeSource(WithFinalAcceptanceRetry(ctx, runID, timeout), source)
	if err != nil {
		return nil, err
	}
	if admission.Resumable != 0 || admission.TerminalUnresolved != 0 {
		return nil, fmt.Errorf("acceptance-only retry requires settled accepted tasks")
	}
	tasksHash, err := collectionDigest(canonicalCollectionSources(source.Session.Tasks))
	if err != nil {
		return nil, err
	}
	return &FinalAcceptanceRetryAssessment{SourceRunID: runID, Branch: source.BranchID, SourceHead: source.eventHead, SourceHash: source.eventHash, PolicySHA256: admission.PolicySHA256, ResponseHash: source.Session.RunResult.Acceptance.RequestAcceptance.ResponseHash, InputsHash: source.Session.RunResult.RunInputs.SnapshotHash, TasksHash: tasksHash, Retained: admission.Accepted, TimeoutSeconds: int64(timeout / time.Second), TimeoutNanoseconds: int64(timeout), MaxReviewCalls: 4}, nil
}

func (c *Coordinator) retryFinalAcceptance(parent context.Context, request finalAcceptanceRetryRequest) (string, error) {
	var assessment *FinalAcceptanceRetryAssessment
	var response string
	ctx, end, err := c.beginAdmittedPublicInvocation(parent, func() error {
		source, err := ReadResumeSource(c.session.Workspace, c.activeBranchID(), request.RunID)
		if err != nil {
			return err
		}
		assessment, err = c.AssessFinalAcceptanceRetry(parent, source, request.RunID, request.Timeout)
		if err != nil {
			return err
		}
		response, err = finalAcceptanceCandidate(source, request)
		return err
	})
	if err != nil {
		return "", err
	}
	defer end()
	fail := func(err error) (string, error) { return "", c.finalizePublicInvocationFailureError(err) }
	if err := c.checkRunAdmission(); err != nil {
		return fail(err)
	}
	if err := c.admitWorkspaceVersion(ctx); err != nil {
		return fail(err)
	}
	if err := c.validateInvocationWorkspace(ctx); err != nil {
		return fail(err)
	}
	if err := c.prepareInvocationInputs(ctx, response); err != nil {
		return fail(err)
	}
	if err := c.emitEvent(string(EventFinalAcceptanceRetryStarted), "runtime", "", assessment); err != nil {
		return fail(err)
	}
	acceptance, acceptanceErr := c.runAcceptance(ctx, response)
	state := AcceptanceFailed
	if acceptanceErr == nil && acceptance != nil {
		state = acceptance.State
	}
	evaluated := EvaluateRunOutcome(RunEvaluationInput{Acceptance: state, Response: response, Stats: SummarizeRunStats(c.taskTracker.TodoList().Items()), Metrics: c.Metrics(), GoalMode: c.GoalMode(), Cancelled: ctx.Err() != nil})
	evaluated.AcceptanceRetry = assessment
	result := c.requestTerminalResult(ctx, TerminalEntryResumeCompletion, "acceptance_retry", false, &evaluated, acceptance)
	c.report(c.newEvent("done").withMessage("final acceptance retry completed").withData(runResultStatusData(result)).withTodoID(CoordTodoID))
	c.SetCurrentStage("idle")
	if !IsRunOutcomeSuccess(result.Outcome) {
		return result.Response, runCompletionError(result)
	}
	return result.Response, nil
}

// Only exact runtime-generated appendices may be removed. The review's hash
// remains authoritative, including for legacy finish failures that appended a
// workflow-closed warning before the acceptance section.
func savedAcceptanceResponse(response string, review *RequestAcceptanceResult) (string, error) {
	if runInputHash([]byte(response)) != review.ResponseHash {
		response = strings.TrimSuffix(response, FormatRequestAcceptance(review))
	}
	if runInputHash([]byte(response)) != review.ResponseHash {
		suffix := fmt.Sprintf("\n\n⚠️ ACCEPTANCE CHECK FAILED: %s\nThe run ends without success because the runtime workflow is done and accepts no new tasks.", review.Error)
		response = strings.TrimSuffix(response, suffix)
	}
	if strings.TrimSpace(response) == "" || runInputHash([]byte(response)) != review.ResponseHash {
		return "", fmt.Errorf("saved answer does not reproduce its reviewed response hash")
	}
	return response, nil
}
