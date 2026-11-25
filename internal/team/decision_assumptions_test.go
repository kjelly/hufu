package team

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

type interruptedAssumptionJournal struct {
	*memoryJournal
	err error
}

func (j *interruptedAssumptionJournal) Append(ctx context.Context, event RunEvent) (RunEvent, error) {
	if event.Type == agent.EventAssumptionSupported && j.err != nil {
		return RunEvent{}, j.err
	}
	return j.memoryJournal.Append(ctx, event)
}

func TestNormalizerRecoversFromInterruptedAssumptionCommit(t *testing.T) {
	c := newTestCoordinator(t)
	if err := c.mutateSessionData(func(*SessionData) error { return nil }); err != nil {
		t.Fatal(err)
	}
	journal := &interruptedAssumptionJournal{memoryJournal: &memoryJournal{}, err: errors.New("decision journal interrupted")}
	c.SetEventJournal(journal)
	compiled, ref := compiledReviewContract(t, true)
	c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "review", ResultContract: &ref}})[0]
	ctx := occurrenceTestContext(c, item.ID, 1)
	record := &DecisionRecord{ID: "assumption-commit-decision", Assumptions: []DecisionAssumption{{ID: "A1", Statement: "declared check"}}}
	index, err := c.decisionIndex()
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Append(DecisionIndexEntry{DecisionID: record.ID, TaskID: item.ID, Assumptions: record.Assumptions}); err != nil {
		t.Fatal(err)
	}
	policy := disciplinePolicy(CommitGatePolicy{}, StopPolicy{CheckpointEvery: 1}, ReplanPolicy{})
	if err := c.armDiscipline(ctx, item.ID, TaskDef{ID: item.ID, Agent: item.Agent}, policy, record); err != nil {
		t.Fatal(err)
	}
	const draft, output = `{"verdict":"APPROVE"}`, `{"verdict":"approve","findings":[]}`
	calls := 0
	bindTestNormalizer(t, c, item, &mockNormalizerProvider{fn: func(context.Context, []byte) ([]byte, error) {
		calls++
		return []byte(output), nil
	}})
	tool := &submitResultTool{coordinator: c, todoID: item.ID}
	for _, checks := range []string{
		`"Assumption_Checks":[{"assumption_id":"undeclared","status":"supported"}]`,
		`"assumption_checks":[{"Assumption_Id":"undeclared","status":"supported"}]`,
	} {
		call := toolCallForTest(`{"status":"success","summary":"done",` + checks + `,"structured_payload":` + draft + `}`)
		response, err := tool.Run(ctx, call)
		if err != nil || !response.IsError || submissionFailureMetadata(response.Metadata) != RepairFailureSource || calls != 0 {
			t.Fatalf("undeclared assumption reached normalization: calls=%d response=%+v err=%v", calls, response, err)
		}
	}
	call := toolCallForTest(`{"status":"success","summary":"done","Assumption_Checks":[{"Assumption_Id":"A1","status":"supported"}],"structured_payload":` + draft + `}`)
	response, err := tool.Run(ctx, call)
	failure := decodeSubmissionFailure(response.Metadata)
	stageRef := failure.Normalization
	if err != nil || !response.IsError || !response.StopTurn || failure.Reason != RepairFailureCommit ||
		stageRef.RawSHA256 != sha256HexBytes([]byte(draft)) || stageRef.OutputSHA256 != sha256HexBytes([]byte(output)) ||
		stageRef.ProviderSHA256 != item.ResultContract.NormalizerSHA256 || c.GetTaskResult(item.ID) != nil {
		t.Fatalf("interrupted assumption submission = %+v, failure=%+v, %v", response, failure, err)
	}
	stageEvidence := assertDirectNormalizationSubmission(t, c, stageRef, call.Input)
	stage, found, err := c.readStagedNormalization(*item.ResultContract, stageRef.OccurrenceSHA256, stageRef.RawSHA256)
	if err != nil || !found || stage.evidence() != stageEvidence || string(stage.Raw) != draft || string(stage.Output) != output || journal.count(agent.EventAssumptionSupported) != 0 {
		t.Fatalf("stage after failed commit = %+v, found=%v, %v", stage, found, err)
	}
	journal.err = nil
	response, err = tool.Run(ctx, call)
	result := c.GetTaskResult(item.ID)
	if err != nil || response.IsError || calls != 1 || journal.count(agent.EventAssumptionSupported) != 1 ||
		result == nil || result.StructuredPayload == nil || result.StructuredPayload.Normalization.evidence() != stageEvidence {
		t.Fatalf("resumed assumption commit = %+v, normalizations=%d result=%+v, %v", response, calls, result, err)
	}
}

func armedAssumptionCoordinator(t *testing.T) (*Coordinator, *memoryJournal) {
	t.Helper()
	c := disciplineCoordinator(t)
	journal := c.eventJournal.(*memoryJournal)

	policy := disciplinePolicy(CommitGatePolicy{}, StopPolicy{CheckpointEvery: 1},
		ReplanPolicy{OnCriticalAssumptionContradicted: agent.ReplanReplan})
	record := &DecisionRecord{
		ID: "dec-1", EvidenceHash: "hash-1",
		Assumptions: []DecisionAssumption{
			{ID: "A1", Statement: "traffic stays flat", Critical: true},
			{ID: "A2", Statement: "the vendor API is stable"},
		},
	}
	if err := c.armDiscipline(context.Background(), "todo-1", TaskDef{ID: "t1"}, policy, record); err != nil {
		t.Fatal(err)
	}
	return c, journal
}

// Only supported and contradicted are reportable. `unknown` is the absence of a
// check and `stale` is the runtime's own conclusion (spec §18.1).
func TestValidCheckStatus(t *testing.T) {
	t.Parallel()
	for status, want := range map[string]bool{
		AssumptionSupported:    true,
		AssumptionContradicted: true,
		AssumptionUnknown:      false,
		AssumptionStale:        false,
		"":                     false,
		"probably":             false,
	} {
		if got := ValidCheckStatus(status); got != want {
			t.Errorf("ValidCheckStatus(%q) = %v, want %v", status, got, want)
		}
	}
}

func TestApplyAssumptionChecks(t *testing.T) {
	t.Parallel()
	c, journal := armedAssumptionCoordinator(t)

	applied, err := c.ApplyAssumptionChecks(context.Background(), "todo-1", []AssumptionCheck{
		{AssumptionID: "A2", Status: AssumptionSupported},
		{AssumptionID: "A1", Status: AssumptionContradicted, Note: "traffic doubled"},
	}, AssumptionSourceTaskResult)
	if err != nil {
		t.Fatalf("ApplyAssumptionChecks = %v", err)
	}
	if applied != 2 {
		t.Fatalf("applied = %d, want 2", applied)
	}
	if journal.count(agent.EventAssumptionContradicted) != 1 || journal.count(agent.EventAssumptionSupported) != 1 {
		t.Fatalf("events = %v, want one of each transition", journal.typesOf())
	}

	// The armed discipline sees the new status, so the next checkpoint acts on it.
	discipline := c.disciplineFor("todo-1")
	if got := CriticalContradiction(discipline.assumptions); got != "A1" {
		t.Fatalf("CriticalContradiction = %q, want A1", got)
	}
	decision := c.recordToolCall(context.Background(), "todo-1", false)
	if decision.Action != CheckpointReplan || decision.Reason != ReasonAssumptionInvalidated {
		t.Fatalf("checkpoint = %#v, want a replan on the contradicted assumption", decision)
	}
}

// A reporter may report on the assumptions a decision rests on, not invent new
// ones after the fact.
func TestApplyAssumptionChecksRejectsUndeclaredAssumptions(t *testing.T) {
	t.Parallel()
	c, _ := armedAssumptionCoordinator(t)
	_, err := c.ApplyAssumptionChecks(context.Background(), "todo-1", []AssumptionCheck{
		{AssumptionID: "A9", Status: AssumptionSupported},
	}, AssumptionSourceTaskResult)
	if err == nil || !strings.Contains(err.Error(), "not declared on this decision") {
		t.Fatalf("ApplyAssumptionChecks = %v, want an undeclared-assumption rejection", err)
	}
}

func TestApplyAssumptionChecksRejectsUnreportableStatus(t *testing.T) {
	t.Parallel()
	c, _ := armedAssumptionCoordinator(t)
	for _, status := range []string{AssumptionUnknown, AssumptionStale, "maybe"} {
		_, err := c.ApplyAssumptionChecks(context.Background(), "todo-1", []AssumptionCheck{
			{AssumptionID: "A1", Status: status},
		}, AssumptionSourceTaskResult)
		if err == nil || !strings.Contains(err.Error(), "not a reportable status") {
			t.Fatalf("status %q = %v, want a rejection", status, err)
		}
	}
}

func TestApplyAssumptionChecksRejectsUnknownSource(t *testing.T) {
	t.Parallel()
	c, _ := armedAssumptionCoordinator(t)
	_, err := c.ApplyAssumptionChecks(context.Background(), "todo-1", []AssumptionCheck{
		{AssumptionID: "A1", Status: AssumptionSupported},
	}, "inference")
	if err == nil || !strings.Contains(err.Error(), "not a source") {
		t.Fatalf("ApplyAssumptionChecks = %v, want an unknown-source rejection", err)
	}
}

// Duplicate claims are rejected as an ambiguous batch, even when they repeat
// the same status; callers can retry the complete claim once, but may not
// smuggle two claims under one transition boundary.
func TestApplyAssumptionChecksIsIdempotent(t *testing.T) {
	t.Parallel()
	c, journal := armedAssumptionCoordinator(t)
	check := []AssumptionCheck{{AssumptionID: "A1", Status: AssumptionSupported}}

	if applied, err := c.ApplyAssumptionChecks(context.Background(), "todo-1", check, AssumptionSourceTaskResult); err != nil || applied != 1 {
		t.Fatalf("first apply = %d, %v", applied, err)
	}
	if applied, err := c.ApplyAssumptionChecks(context.Background(), "todo-1", append(check, check...), AssumptionSourceTaskResult); err == nil || applied != 0 {
		t.Fatalf("duplicate apply = %d, %v, want rejection", applied, err)
	}
	if journal.count(agent.EventAssumptionSupported) != 1 {
		t.Fatalf("events = %d, want 1", journal.count(agent.EventAssumptionSupported))
	}
}

// A task with no armed decision has nothing to check against, and reporting
// against it is a no-op rather than an error.
func TestApplyAssumptionChecksWithoutADecisionIsANoOp(t *testing.T) {
	t.Parallel()
	c := disciplineCoordinator(t)
	applied, err := c.ApplyAssumptionChecks(context.Background(), "todo-unarmed", []AssumptionCheck{
		{AssumptionID: "A1", Status: AssumptionSupported},
	}, AssumptionSourceTaskResult)
	if err != nil || applied != 0 {
		t.Fatalf("ApplyAssumptionChecks = %d, %v, want a silent no-op", applied, err)
	}
}

func TestApplyAssumptionChecksFailsClosedWithoutJournal(t *testing.T) {
	t.Parallel()
	c, _ := armedAssumptionCoordinator(t)
	c.eventJournal = nil
	if _, err := c.ApplyAssumptionChecks(context.Background(), "todo-1", []AssumptionCheck{{
		AssumptionID: "A1", Status: AssumptionSupported,
	}}, AssumptionSourceTaskResult); err == nil {
		t.Fatal("assumption check succeeded without a canonical journal")
	}
}

// A declared verification is an assumption source independent of what the
// worker says about its own work (spec §18.1 source 2).
func TestAssumptionChecksFromVerification(t *testing.T) {
	t.Parallel()
	spec := &VerificationSpec{Type: agent.VerifyCommandExit, AssumptionRefs: []string{"A1", " A2 ", "  "}}
	if err := ValidateVerificationAssumptionRefs(spec); err == nil {
		t.Fatal("blank verification assumption reference was accepted")
	}

	passed := AssumptionChecksFromVerification(spec, true)
	if len(passed) != 2 {
		t.Fatalf("checks = %#v, want the blank ref dropped", passed)
	}
	for _, check := range passed {
		if check.Status != AssumptionSupported {
			t.Fatalf("passing verification produced %q", check.Status)
		}
		if !strings.Contains(check.Note, "passed") {
			t.Fatalf("note = %q, want the verification outcome recorded", check.Note)
		}
	}
	if passed[1].AssumptionID != "A2" {
		t.Fatalf("refs were not trimmed: %q", passed[1].AssumptionID)
	}

	failed := AssumptionChecksFromVerification(spec, false)
	for _, check := range failed {
		if check.Status != AssumptionContradicted {
			t.Fatalf("failing verification produced %q", check.Status)
		}
	}

	if got := AssumptionChecksFromVerification(nil, true); got != nil {
		t.Fatalf("nil spec produced %#v", got)
	}
	if got := AssumptionChecksFromVerification(&VerificationSpec{}, true); got != nil {
		t.Fatalf("a spec with no refs produced %#v", got)
	}
}

// A worker reporting a check must be able to invalidate the decision its own
// task runs under — that is the whole point of the source.
func TestVerificationSourceDrivesTheCheckpoint(t *testing.T) {
	t.Parallel()
	c, journal := armedAssumptionCoordinator(t)

	checks := AssumptionChecksFromVerification(
		&VerificationSpec{Type: agent.VerifyCommandExit, AssumptionRefs: []string{"A1"}}, false)
	if _, err := c.ApplyAssumptionChecks(context.Background(), "todo-1", checks, AssumptionSourceVerification); err != nil {
		t.Fatal(err)
	}
	if journal.count(agent.EventAssumptionContradicted) != 1 {
		t.Fatalf("events = %v, want the contradiction recorded", journal.typesOf())
	}

	decision := c.recordToolCall(context.Background(), "todo-1", false)
	if decision.Action != CheckpointReplan {
		t.Fatalf("checkpoint = %#v, want a replan", decision)
	}
	if journal.count(agent.EventDecisionInvalidated) != 1 {
		t.Fatal("the decision was not superseded")
	}
}

func TestDisciplineTodoIDFrom(t *testing.T) {
	t.Parallel()
	ctx := context.WithValue(context.Background(), todoIDKey{}, "todo-from-context")
	if got := disciplineTodoIDFrom(ctx, TaskDef{ID: "t1"}); got != "todo-from-context" {
		t.Fatalf("disciplineTodoIDFrom = %q, want the context value", got)
	}
	if got := disciplineTodoIDFrom(context.Background(), TaskDef{ID: "t1"}); got != "t1" {
		t.Fatalf("disciplineTodoIDFrom = %q, want the task contract id as fallback", got)
	}
	if got := disciplineTodoIDFrom(context.Background(), TaskDef{}); got != "" {
		t.Fatalf("disciplineTodoIDFrom = %q, want empty", got)
	}
}

// The operator source works after the run that formed the decision has exited.
func TestOperatorAssumptionCheckThroughTheIndex(t *testing.T) {
	t.Parallel()
	index := newIndex(t)
	journal := &memoryJournal{}
	index.SetJournal(journal)
	record := DecisionRecord{
		ID: "dec-1", FinalOption: "migrate",
		Assumptions: []DecisionAssumption{{ID: "A1", Statement: "traffic stays flat", Critical: true}},
	}
	if err := index.Append(IndexEntryFor(record, "Ship?", false, ArtifactRef{})); err != nil {
		t.Fatal(err)
	}

	entry, assumption, err := index.CheckAssumption("dec-1", "A1", AssumptionContradicted, "traffic doubled")
	if err != nil {
		t.Fatalf("CheckAssumption = %v", err)
	}
	if assumption.EffectiveStatus() != AssumptionContradicted {
		t.Fatalf("assumption = %#v", assumption)
	}
	if entry.CriticalAssumptionContradicted() != "A1" {
		t.Fatalf("CriticalAssumptionContradicted = %q", entry.CriticalAssumptionContradicted())
	}
	if !entry.Stale || journal.count(agent.EventDecisionInvalidated) != 1 || journal.count(agent.EventReplanRequested) != 1 {
		t.Fatalf("operator contradiction did not persist invalidation/replan: entry=%#v events=%v", entry, journal.typesOf())
	}
	if len(entry.AssumptionNotes) != 1 || !strings.Contains(entry.AssumptionNotes[0], "traffic doubled") {
		t.Fatalf("notes = %v, want the reason recorded", entry.AssumptionNotes)
	}
	// The decision itself is untouched.
	if entry.FinalOption != "migrate" {
		t.Fatalf("the operator check changed the decision: %#v", entry)
	}

	if _, _, err := index.CheckAssumption("dec-1", "A1", AssumptionStale, ""); err == nil {
		t.Fatal("an unreportable status was accepted")
	}
	if _, _, err := index.CheckAssumption("dec-1", "A1", AssumptionContradicted, "again"); err == nil {
		t.Fatal("a duplicate operator status was accepted")
	}
	if _, _, err := index.CheckAssumption("dec-1", "A9", AssumptionSupported, ""); err == nil {
		t.Fatal("an undeclared assumption was accepted")
	}
	if _, _, err := index.CheckAssumption("dec-missing", "A1", AssumptionSupported, ""); err == nil {
		t.Fatal("an unknown decision was accepted")
	}
	if _, _, err := index.CheckAssumption("dec-1", "", AssumptionSupported, ""); err == nil {
		t.Fatal("a blank assumption ID was accepted")
	}
	data, err := readTaskJournalLog(index.workspace)
	if err != nil {
		t.Fatalf("read operator task journal projection: %v", err)
	}
	if !strings.Contains(string(data), `"op":"decision_assumptions"`) {
		t.Fatalf("task journal omitted operator assumption projection: %s", data)
	}
}

func TestProjectDecisionReplaysAssumptionTransitionAndInvalidation(t *testing.T) {
	t.Parallel()
	j := &memoryJournal{}
	record := &DecisionRecord{SchemaVersion: 1, ID: "dec-replay", Assumptions: []DecisionAssumption{{ID: "A1", Critical: true}}}
	if err := appendDecisionEvent(context.Background(), j, agent.EventDecisionFinalized, decisionEvent{
		DecisionID: record.ID, Record: record,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RecordAssumptionTransition(context.Background(), j, record.Assumptions, AssumptionTransition{
		DecisionID: record.ID, AssumptionID: "A1", From: AssumptionUnknown,
		To: AssumptionContradicted, Source: AssumptionSourceOperator, At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := appendDecisionEvent(context.Background(), j, agent.EventDecisionInvalidated, decisionEvent{
		DecisionID: record.ID, Reason: ReasonAssumptionInvalidated,
	}); err != nil {
		t.Fatal(err)
	}
	state, err := projectDecision(context.Background(), j, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Record == nil || !state.Record.Stale || state.Record.Assumptions[0].EffectiveStatus() != AssumptionContradicted {
		t.Fatalf("replayed decision = %#v, want stale contradicted assumption", state.Record)
	}
}

func TestDecisionIndexRebuildFromCanonicalJournal(t *testing.T) {
	t.Parallel()
	index := newIndex(t)
	j := &memoryJournal{}
	record := &DecisionRecord{SchemaVersion: 1, ID: "dec-rebuild", RunID: "run-1", TaskID: "task-1",
		Profile: "standard", FinalOption: "ship", Probability: 0.8,
		Assumptions: []DecisionAssumption{{ID: "A1", Statement: "traffic stays flat", Critical: true}}}
	if err := appendDecisionEvent(context.Background(), j, agent.EventDecisionEvidenceSealed, decisionEvent{
		DecisionID: record.ID, Packet: &DecisionEvidencePacket{ID: "packet-1", Question: "Ship?"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := appendDecisionEvent(context.Background(), j, agent.EventDecisionFinalized, decisionEvent{
		DecisionID: record.ID, Record: record,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RecordAssumptionTransition(context.Background(), j, record.Assumptions, AssumptionTransition{
		DecisionID: record.ID, AssumptionID: "A1", From: AssumptionUnknown,
		To: AssumptionContradicted, Source: AssumptionSourceOperator, At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := appendDecisionEvent(context.Background(), j, agent.EventDecisionInvalidated, decisionEvent{
		DecisionID: record.ID, Reason: "critical assumption contradicted",
	}); err != nil {
		t.Fatal(err)
	}
	if err := index.RebuildFromJournal(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	entries, err := index.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].Stale || entries[0].Assumptions[0].EffectiveStatus() != AssumptionContradicted {
		t.Fatalf("rebuilt index = %#v, want stale contradicted decision", entries)
	}
}

func TestDirectResultChecksDeclaredAssumptionBeforeOtherClaims(t *testing.T) {
	c, journal := armedAssumptionCoordinator(t)
	tool := &submitResultTool{coordinator: c, todoID: "todo-1"}
	events := len(journal.events)
	for _, tc := range []struct {
		check string
		want  RepairFailureReason
	}{
		{`{"assumption_id":"A1","status":"supported"}`, RepairFailureInvalidSchema},
		{`{"assumption_id":"A1","status":"unknown"}`, RepairFailureInvalidSchema},
		{`{"assumption_id":"forged","status":"supported","unknown":true}`, RepairFailureSource},
		{`{"assumption_id":"forged","assumption_id":"A1","status":"unknown"}`, RepairFailureSource},
		{`{"assumption_id":"A1","status":"unknown","evidence":[false,"forged-digest"]}`, RepairFailureSource},
	} {
		raw := fmt.Sprintf(`{"status":"success","summary":"checked","unknown":true,"assumption_checks":[%s]}`, tc.check)
		resp, err := tool.Run(t.Context(), fantasy.ToolCall{Name: submitResultToolName, Input: raw})
		if err != nil || submissionFailureMetadata(resp.Metadata) != tc.want {
			t.Fatalf("assumption %s rejected as %s, %v, want %s", tc.check, resp.Metadata, err, tc.want)
		}
	}
	if len(journal.events) != events {
		t.Fatal("source preflight wrote an assumption transition")
	}
}

func TestNormalizerRepairsFormatOfDeclaredAssumption(t *testing.T) {
	var calls int
	c, _, item := newDirectNormalizationRun(t, func(ctx context.Context, draft []byte) ([]byte, error) {
		calls++
		return echoNormalization(ctx, draft)
	}, nil)
	armNormalizationAssumption(t, c, item, true)
	wire := func(status string) string {
		return fmt.Sprintf(`{"status":"success","summary":"checked","assumption_checks":[{"assumption_id":"A1","status":%q}],"structured_payload":%s}`, status, approvedNormalizationDraft)
	}
	tool := &submitResultTool{coordinator: c, todoID: item.ID}
	worker := &rejectedResultStreamModel{toolName: submitResultToolName, finish: fantasy.FinishReasonToolCalls, input: func(int) string { return wire("unknown") }}
	repair := &rejectedResultStreamModel{toolName: submitResultToolName, finish: fantasy.FinishReasonToolCalls, input: func(int) string { return wire("supported") }}
	c.workerAgentOverride = fantasy.NewAgent(worker, fantasy.WithTools(c.gatePolicyTools([]fantasy.AgentTool{tool})...), fantasy.WithMaxRetries(0))
	c.repairAgentOverride = fantasy.NewAgent(repair, fantasy.WithTools(tool), fantasy.WithMaxRetries(0))
	_, err := c.executeTask(withTestProtocolRepairInvocationContext(t.Context()), taskDefFromTodoItem(item), item.ID)
	if err != nil || worker.calls != 1 || repair.calls != 1 || calls != 1 {
		t.Fatalf("declared assumption format did not get one correction: work=%d repair=%d normalizer=%d error=%v", worker.calls, repair.calls, calls, err)
	}
}
