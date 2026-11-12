package team

import (
	"context"
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

func finalAcceptanceRetryFixture(t *testing.T) (*Coordinator, *ResumeSource) {
	t.Helper()
	c := requestAcceptanceFixture(t)
	if err := installRequestAcceptanceInput(c.session); err != nil {
		t.Fatal(err)
	}
	c.providerBoundaryStarted = true
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(_ context.Context, operation, _ string) ([]byte, error) {
		switch operation {
		case "contract":
			return faithfulRequestContract(), nil
		case "extract":
			return requestExtraction("Explain the result.", "Explain the result.", "response"), nil
		default:
			return nil, context.DeadlineExceeded
		}
	})))
	ctx, end, err := c.beginPublicInvocationExecutionRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.checkRunAdmission(); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveRunInputsForInvocation(ctx, "Explain the result."); err != nil {
		t.Fatal(err)
	}
	item := completedRequestEvidence(t, c, "complete native observation", false)
	if err := c.emitTaskEventsFromCheckpoint(c.taskTracker.TodoList().Items()); err != nil {
		t.Fatal(err)
	}
	if err := c.emitEvent(string(EventWorkflowStateChanged), "runtime", "", map[string]any{"state": PhaseDone}); err != nil {
		t.Fatal(err)
	}
	result := c.finalizeDirectRun(ctx, item.ID, true, "The result is explained with its limitations.")
	if result == nil || result.StopReason != StopReasonAcceptanceFailed {
		t.Fatalf("expected unavailable acceptance: %+v", result)
	}
	end()
	source, err := ReadResumeSource(c.session.Workspace, c.activeBranchID(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := finalAcceptanceCandidate(source, finalAcceptanceRetryRequest{source.Session.RunResult.RunID, time.Minute}); err != nil {
		t.Fatal(err)
	}
	return c, source
}

func acceptanceRetryCoordinator(t *testing.T, c *Coordinator, source *ResumeSource) *Coordinator {
	t.Helper()
	fresh := newTestCoordinator(t)
	fresh.session, fresh.projectDir, fresh.providerManager = c.session, c.projectDir, c.providerManager
	fresh.providerBoundaryStarted = true
	fresh.providerBoundaryStart = func(context.Context, string) error { return nil }
	fresh.maxConcurrent = c.maxConcurrent
	fresh.sidecarModel, fresh.guardModel, fresh.judgeModel, fresh.planReviewerModel = c.sidecarModel, c.guardModel, c.judgeModel, c.planReviewerModel
	fresh.recoveryBranch = source.BranchID
	fresh.executionRunID = ""
	data, err := json.Marshal(source.Session, collectionDurationFormat)
	if err != nil {
		t.Fatal(err)
	}
	var session SessionData
	if err := json.Unmarshal(data, &session, collectionDurationFormat); err != nil {
		t.Fatal(err)
	}
	fresh.SetSessionData(&session)
	fresh.taskTracker.TodoList().Restore(session.Tasks)
	return fresh
}

func TestFinalAcceptanceRetryPreservesCompletedWork(t *testing.T) {
	for _, outcome := range []string{"passed", "timeout", "cancelled", "negative"} {
		t.Run(outcome, func(t *testing.T) {
			c, source := finalAcceptanceRetryFixture(t)
			before := acceptanceRetryEvents(t, c)
			oldTasks, _ := collectionDigest(canonicalCollectionSources(source.Session.Tasks))
			oldResult, _ := collectionDigest(source.Session.RunResult)
			fresh := acceptanceRetryCoordinator(t, c, source)
			calls := 0
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			fresh.SetRequestAcceptanceModel(requestAcceptanceModelFunc(func(ctx context.Context, operation, prompt string) ([]byte, error) {
				calls++
				if operation != "review" {
					t.Fatalf("unexpected resolver operation %s", operation)
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > time.Minute || time.Until(deadline) < 50*time.Second {
					t.Fatal("retry timeout not enforced")
				}
				if !strings.Contains(prompt, "The result is explained with its limitations.") {
					t.Fatal("original candidate lost")
				}
				if outcome == "cancelled" {
					cancel()
					return nil, context.Canceled
				}
				if outcome == "timeout" {
					return nil, context.DeadlineExceeded
				}
				if outcome == "negative" {
					return []byte(`{"request_covered":false,"checks":[{"id":"requirement-1","status":"missing","reason":"The explanation is insufficient.","evidence":[]}]}`), nil
				}
				return passingRequestReview(requestAcceptanceSource{ID: requestAcceptanceFinalResponseSource}, "The result is explained with its limitations."), nil
			}))
			worker := scriptWorker(textTurn("must not run"))
			fresh.workerAgentOverride = worker
			fresh.runOrchestratorOverride = func(context.Context, *agent.AgentDef, string) (string, []fantasy.StepResult, error) {
				t.Fatal("coordinator must not run")
				return "", nil, nil
			}
			assessment, err := fresh.AssessFinalAcceptanceRetry(t.Context(), source, source.Session.RunResult.RunID, time.Minute)
			if err != nil || assessment.Retained != 1 || calls != 0 {
				t.Fatalf("preview %+v %v", assessment, err)
			}
			if got := acceptanceRetryEvents(t, c); !reflect.DeepEqual(before, got) {
				t.Fatal("preview mutated events")
			}
			response, err := fresh.Run(WithFinalAcceptanceRetry(parent, source.Session.RunResult.RunID, time.Minute), "ignored coordinator instruction")
			if (err == nil) != (outcome == "passed") || calls != 1 || worker.Calls() != 0 {
				t.Fatalf("calls=%d worker=%d err=%v", calls, worker.Calls(), err)
			}
			result := fresh.LastRunResult()
			if result == nil || result.AcceptanceRetry == nil || result.RunID == source.Session.RunResult.RunID || !strings.Contains(response, "The result is explained with its limitations.") {
				t.Fatal("missing durable retry outcome")
			}
			after, err := ReadResumeSource(c.session.Workspace, source.BranchID, result.RunID)
			if err != nil {
				t.Fatal(err)
			}
			taskHash, _ := collectionDigest(canonicalCollectionSources(after.Session.Tasks))
			if taskHash != oldTasks {
				t.Fatal("completed tasks or receipts changed")
			}
			events := acceptanceRetryEvents(t, fresh)
			if !reflect.DeepEqual(before, events[:len(before)]) {
				t.Fatal("historical events changed")
			}
			originalHash, _ := collectionDigest(source.Session.RunResult)
			if originalHash != oldResult {
				t.Fatal("original run changed")
			}
			if outcome == "passed" && (!after.Session.RunResult.GoalSatisfied || after.Session.RunResult.AcceptanceRetry == nil || after.Session.RunResult.AcceptanceRetry.SourceRunID != source.Session.RunResult.RunID) {
				t.Fatal("successful retry did not replay")
			}
		})
	}
}

func TestFinalAcceptanceRetryRejectsUnsafeSources(t *testing.T) {
	for _, name := range []string{"old_run", "changed_response", "changed_contract", "negative", "material_veto", "unresolved", "already_passed", "other_acceptance_failure", "protocol_error", "policy_drift"} {
		t.Run(name, func(t *testing.T) {
			c, source := finalAcceptanceRetryFixture(t)
			cloneData, _ := json.Marshal(source.Session, json.Deterministic(true), collectionDurationFormat)
			var session SessionData
			if err := json.Unmarshal(cloneData, &session, collectionDurationFormat); err != nil {
				t.Fatal(err)
			}
			local := *source
			local.Session = &session
			runID := session.RunResult.RunID
			switch name {
			case "old_run":
				runID = "other-run"
			case "changed_response":
				session.RunResult.Response = "altered"
			case "changed_contract":
				session.RunResult.Acceptance.RequestAcceptance.Contract.Requirements[0].Statement = "changed"
			case "protocol_error":
				session.RunResult.Acceptance.RequestAcceptance.ErrorKind = ""
				session.RunResult.Acceptance.RequestAcceptance.Error = "invalid review JSON"
				session.RunResult.Acceptance.Errors = []string{"request acceptance: invalid review JSON"}
			case "negative":
				session.RunResult.Acceptance.RequestAcceptance.Checks = []RequestAcceptanceCheck{{ID: "requirement-1", Status: "missing"}}
			case "material_veto":
				session.RunResult.Acceptance.RequestAcceptance.MaterialReview = &RequestMaterialReview{Checks: []RequestMaterialCheck{{ID: "requirement-1", Adequate: false}}}
			case "unresolved":
				session.Tasks[0].Status = TaskPending
			case "already_passed":
				session.RunResult.StopReason = StopReasonCompleted
			case "other_acceptance_failure":
				session.RunResult.Acceptance.Errors = append(session.RunResult.Acceptance.Errors, "required artifact missing")
			}
			local.sessionSHA256, _ = collectionDigest(local.Session)
			fresh := acceptanceRetryCoordinator(t, c, source)
			if name == "policy_drift" {
				fresh.session.Config.Generation.Temperature = "0.9"
			}
			_, err := fresh.AssessFinalAcceptanceRetry(t.Context(), &local, runID, time.Minute)
			if err == nil {
				t.Fatal("unsafe acceptance retry admitted")
			}
		})
	}
}

func acceptanceRetryEvents(t *testing.T, c *Coordinator) []RunEvent {
	t.Helper()
	store, err := OpenEventStoreReadOnly(c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	return mustReadEvents(t, store)
}

func TestFinalAcceptanceRetryRestoresOnlyHashBoundAnswer(t *testing.T) {
	review := &RequestAcceptanceResult{Error: "request acceptance review: context deadline exceeded", ResponseHash: runInputHash([]byte("complete original answer"))}
	footer := "\n\n⚠️ ACCEPTANCE CHECK FAILED: " + review.Error + "\nThe run ends without success because the runtime workflow is done and accepts no new tasks."
	for _, tc := range []struct {
		name, text string
		valid      bool
	}{
		{"plain", "complete original answer", true},
		{"appendix", "complete original answer" + FormatRequestAcceptance(review), true},
		{"legacy_finish", "complete original answer" + footer + FormatRequestAcceptance(review), true},
		{"changed_answer", "changed original answer" + footer + FormatRequestAcceptance(review), false},
		{"unknown_footer", "complete original answer\nunknown diagnostic" + FormatRequestAcceptance(review), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer, err := savedAcceptanceResponse(tc.text, review)
			if (err == nil) != tc.valid || tc.valid && answer != "complete original answer" {
				t.Fatalf("answer=%q err=%v", answer, err)
			}
		})
	}
}
