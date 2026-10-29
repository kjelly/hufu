package team

import (
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func appendPriorReport(t *testing.T, c *Coordinator, report string) RunEvent {
	t.Helper()
	data, err := json.Marshal(RunFinishedEventPayload{Outcome: RunOutcomeCompleted, Response: report})
	if err != nil {
		t.Fatal(err)
	}
	event, err := c.EventJournal().Append(t.Context(), RunEvent{
		RunID: "run-prior", Type: string(EventRunFinished), Actor: "coordinator", Payload: data,
	})
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func TestPreviousResponsePreservesReportWithNativeVerificationDuration(t *testing.T) {
	finished := RunEvent{ID: "completed-report", Type: string(EventRunFinished),
		Payload: []byte(`{"outcome":"completed","response":"complete verified response","acceptance":{"verification_evidence":[{"duration":1500000000}]}}`)}
	item, err := PreviousResponseContextItem(finished)
	if err != nil || !strings.Contains(item.Content, "complete verified response") {
		t.Fatalf("native report response: %v", err)
	}
	boundary := RunEvent{ID: "new-boundary", Type: string(EventWorkflowCycleStarted), RunID: "new-cycle"}
	previous, err := previousWorkflowResponse([]RunEvent{finished, boundary}, "new-cycle")
	if err != nil || previous.Content != item.Content || previous.ID != item.ID {
		t.Fatalf("follow-up lost native report response: %v", err)
	}
}

func followUpWorkerInput(c *Coordinator, name string, phase Phase, trigger ContextTrigger) WorkerContextInput {
	def := &agent.AgentDef{Name: name, Role: "researcher"}
	task := TaskDef{Agent: name, Goal: "Investigate the charging details mentioned above", Phase: phase}
	attempt := 1
	var failure *ContextFailure
	if trigger == ContextTriggerRetry {
		attempt = 2
		failure = &ContextFailure{Class: FailureEnvironment}
	}
	request := c.newTaskContextRequest(task, "3", attempt, trigger, name, def.Role, failure)
	input := buildWorkerContextInput(request, task, def, "", "", "", "", nil)
	input.OriginalRequest = c.originalRequestFor(name)
	input.MaxAuxChars = 128
	input.ModelContext = ModelContextSpec{ModelID: "test", ContextWindow: 131072, MaxOutputTokens: 512}
	return input
}

func TestFollowUpWorkerReceivesCompleteReportAcrossDispatchRetryAndResume(t *testing.T) {
	c := completedCycleCoordinator(t)
	c.session.Config.Delegation.ShareRequestWith = []string{"preparer", "verifier"}
	// Exceed the old request/auxiliary limits, with a distinct final finding.
	report := "Earlier question: a specific case\n" + strings.Repeat("finding with its source and uncertainty\n", 1500) + "LAST_FINDING_MUST_SURVIVE"
	source := appendPriorReport(t, c, report)
	if _, err := c.beginFollowUpWorkflow(t.Context(), "Investigate that prosecution in detail"); err != nil {
		t.Fatal(err)
	}
	// Current-cycle conversation and mutable checkpoints cannot replace the
	// report selected before the durable boundary.
	if err := c.recordSessionMessage("intermediate reply, not the earlier report", "assistant"); err != nil {
		t.Fatal(err)
	}
	c.sessionData.Entries = nil
	for _, mode := range []string{"dispatch", "retry", "verify", "resume", "fresh_chat"} {
		t.Run(mode, func(t *testing.T) {
			phase, trigger := PhasePrepare, ContextTriggerTaskDispatch
			if mode == "retry" {
				trigger = ContextTriggerRetry
			}
			if mode == "verify" {
				phase = PhaseVerify
			}
			if mode == "resume" {
				c.sessionData = ReduceToSessionData(mustCycleEvents(t, c))
				c.initialPrompt = ResumeInstruction
			}
			if mode == "fresh_chat" {
				c.freshSessionMemory.Store(true)
				defer c.freshSessionMemory.Store(false)
			}
			input := followUpWorkerInput(c, "preparer", phase, trigger)
			// Simulate unrelated archive recall plus an excluded progress row.
			input.CanonicalMemory = &CanonicalContextBundle{SharedSession: []contextstore.ContextItem{
				{ID: "unrelated-archive", Kind: contextstore.ContextSummary, Content: "An unrelated welfare topic", Lifecycle: contextstore.LifecycleConfirmed},
				{ID: "old-progress", Kind: contextstore.ContextProgress, Content: "short prior research summary", Lifecycle: contextstore.LifecycleConfirmed},
			}}
			compiled, err := c.ContextCompiler().CompileWorkerContext(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(compiled.Prompt, report) || strings.Contains(compiled.Prompt, "intermediate reply") {
				t.Fatal("worker did not receive the exact complete preceding response")
			}
			manifest, err := BuildContextInjectionManifest(input.Request, compiled, nil, "preparer", time.Now(), agent.DefaultMemoryLearningPolicy())
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range manifest.Items {
				if item.Kind == "previous_response" {
					found = true
					if item.ID != previousResponsePrefix+source.ID || !item.Included || item.Compressed || item.ContentHash == "" || item.Source != "event_store:"+source.ID || item.Reason != ContextIncludedRequired {
						t.Fatalf("report attribution = %+v", item)
					}
				}
			}
			if !found {
				t.Fatal("manifest cannot prove report delivery")
			}
		})
	}
}

func TestFollowUpReportCannotBeSilentlyTruncatedOrOmitted(t *testing.T) {
	c := completedCycleCoordinator(t)
	c.session.Config.Delegation.ShareRequestWith = []string{"preparer"}
	appendPriorReport(t, c, strings.Repeat("complete report evidence\n", 2000))
	if _, err := c.beginFollowUpWorkflow(t.Context(), "follow up"); err != nil {
		t.Fatal(err)
	}
	input := followUpWorkerInput(c, "preparer", PhasePrepare, ContextTriggerTaskDispatch)
	input.ModelContext.ContextWindow = 4096
	input.ModelContext.SafetyMarginTokens = 64
	_, err := c.ContextCompiler().CompileWorkerContext(t.Context(), input)
	budgetErr, ok := errors.AsType[*RequiredContextBudgetError](err)
	if !ok || budgetErr.Kind != "previous_response" {
		t.Fatalf("missing fail-closed report budget error: %v", err)
	}
}

func TestFollowUpReportHonorsWorkerAndAuxiliaryBoundaries(t *testing.T) {
	c := completedCycleCoordinator(t)
	c.session.Config.Delegation.ShareRequestWith = []string{"preparer"}
	if _, err := c.beginFollowUpWorkflow(t.Context(), "follow up"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, worker string
		trigger      ContextTrigger
		fresh        bool
	}{
		{name: "unlisted", worker: "other", trigger: ContextTriggerTaskDispatch},
		{name: "auxiliary", worker: "preparer", trigger: ContextTriggerSidecarTask},
		{name: "explicit no history", worker: "preparer", trigger: ContextTriggerTaskDispatch, fresh: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.fresh {
				c.executionProfile = ExecutionProfile{Name: "test", DisableHistoricalMemory: true}
				defer func() { c.executionProfile = ExecutionProfile{} }()
			}
			input := followUpWorkerInput(c, tc.worker, PhasePrepare, tc.trigger)
			if tc.trigger == ContextTriggerSidecarTask {
				// The production auxiliary builder sets its own validated request.
				input.Request = ContextRequest{Trigger: ContextTriggerSidecarTask}
			}
			compiled, err := c.ContextCompiler().CompileWorkerContext(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range compiled.IncludedItems {
				if item.Kind == "previous_response" {
					t.Fatal("report leaked across its disclosure boundary")
				}
			}
		})
	}
}

func TestPreviousWorkflowResponseStopsAtCycleAndBranchBoundaries(t *testing.T) {
	response := func(id, text string) RunEvent {
		data, err := json.Marshal(RunFinishedEventPayload{Outcome: RunOutcomeCompleted, Response: text})
		if err != nil {
			t.Fatal(err)
		}
		return RunEvent{ID: id, BranchID: "main", Type: string(EventRunFinished), Payload: data}
	}
	events := []RunEvent{
		response("first", "first report"),
		{ID: "cycle1", BranchID: "main", Type: string(EventWorkflowCycleStarted), RunID: "cycle1"},
		response("second", "second report with ``` embedded fence"),
		{ID: "cycle2", BranchID: "main", Type: string(EventWorkflowCycleStarted), RunID: "cycle2"},
		response("later", "current cycle reply"),
	}
	item, err := previousWorkflowResponse(events, "cycle2")
	if err != nil || item.ID != previousResponsePrefix+"second" || strings.Contains(item.Content, "first report") || !strings.Contains(item.Content, "````text") {
		t.Fatalf("cycle selection or historical fencing failed: %+v, %v", item, err)
	}
	if _, err := previousWorkflowResponse(append(slices.Clone(events[:2]), events[3]), "cycle2"); err == nil {
		t.Fatal("missing report was replaced with an older cycle's response")
	}
	sibling := response("sibling", "private sibling report")
	sibling.BranchID = "sibling"
	tree := &SessionTree{ActiveBranch: "main", Branches: map[string]*SessionBranch{
		"main": {ID: "main"}, "sibling": {ID: "sibling"},
	}}
	lineage, err := ProjectValidatedEventsForBranch(append(events, sibling), tree, "main")
	if err != nil {
		t.Fatal(err)
	}
	item, err = previousWorkflowResponse(lineage, "cycle2")
	if err != nil || strings.Contains(item.Content, "private sibling") {
		t.Fatalf("branch selection leaked: %+v, %v", item, err)
	}
}

func TestPreviousWorkflowResponsePrefersCompletedReportOverFailedResume(t *testing.T) {
	encode := func(value any) []byte {
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	events := []RunEvent{
		{ID: "completed", Type: string(EventRunFinished), Payload: encode(RunFinishedEventPayload{Outcome: RunOutcomeCompleted, Response: "complete earlier report"})},
		{ID: "failed-resume", Type: string(EventRunFinished), Payload: encode(RunFinishedEventPayload{Outcome: RunOutcomeFailed, Response: "recovery error"})},
		{ID: "cycle", Type: string(EventWorkflowCycleStarted), RunID: "follow-up"},
	}
	item, err := previousWorkflowResponse(events, "follow-up")
	if err != nil || !strings.Contains(item.Content, "complete earlier report") || strings.Contains(item.Content, "recovery error") {
		t.Fatalf("failed resume hid completed research: %+v, %v", item, err)
	}
	if _, err := previousWorkflowResponse(nil, "follow-up"); err == nil {
		t.Fatal("missing durable boundary was accepted")
	}
	if _, err := previousWorkflowResponse(events[2:], "follow-up"); err == nil {
		t.Fatal("missing durable report was accepted")
	}
}
