package team

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// The unified test matrix, spec §46 rows A–S (plan Stage 7.3).
//
// Each row below names the guarantee it covers and either exercises it here or
// points at the test that does. The matrix exists so a reader can answer "is
// row N covered, and by what" without grepping, and so a row that loses its
// coverage fails a named test rather than disappearing quietly.

// TestEveryDecisionStagePurposeIsRegistered is the guard for a defect this
// stage found: every decision stage dispatches under a purpose from a closed
// registry, and seven of the eight were never registered. Every decision
// therefore failed at its first judge, in production, with the subsystem's own
// unit tests all passing.
func TestEveryDecisionStagePurposeIsRegistered(t *testing.T) {
	for _, purpose := range decisionStagePurposes {
		policy, err := contextPurposePolicy(purpose)
		if err != nil {
			t.Fatalf("decision stage purpose %q is not registered: %v", purpose, err)
		}
		// A decision stage may never degrade: running with fewer judges, no
		// challenge, or no finalization is what spec §34 forbids.
		if policy.FallbackAllowed {
			t.Fatalf("decision stage purpose %q allows a silent fallback", purpose)
		}
	}
}

// decisionMatrixRow records one row of spec §46 and where it is proved.
type decisionMatrixRow struct {
	row      string
	guaranee string
	// coveredBy names the test that proves the row. A row proved inline names
	// this test.
	coveredBy []string
}

// TestDecisionV1MatrixIsFullyAttributed asserts that every row of spec §46 has
// named coverage. It deliberately does not re-run the tests: its job is to
// make an unattributed row impossible to leave behind.
func TestDecisionV1MatrixIsFullyAttributed(t *testing.T) {
	rows := []decisionMatrixRow{
		{"A", "old config behaves identically", []string{
			"TestDecisionV1OffProfileIsInert",
			"TestParseTeamYMLWithoutDecisionBlock",
			"TestDisciplineHooksAreNoOpsWhenUnarmed",
		}},
		{"B", "a judge prompt contains no other judge's opinion", []string{
			"TestDecisionV1JudgePromptsAreIsolated",
			"TestChallengeRunsAfterAggregateAndIsAnonymized",
		}},
		{"C", "fixed opinions produce a fixed aggregate with zero LLM calls, order-independently", []string{
			"TestDecisionV1FormationIsDeterministic",
			"TestAggregateIsDeterministic",
			"TestStatisticsAreOrderIndependent",
		}},
		{"D", "out-of-range and NaN are rejected without clamping; weights normalize; tie-breaks are three-layered", []string{
			"TestValidateOpinionNeverClamps",
			"TestNormalizedWeights",
			"TestAggregateTieBreak",
		}},
		{"E", "a material change moves the sealed hash; a non-material one does not", []string{
			"TestMaterialChangesAlterTheHash",
			"TestNonMaterialChangesPreserveTheHash",
			"TestDecisionV1FormationIsDeterministic",
		}},
		{"F", "a required and missing no-go alternative blocks", []string{
			"TestDecisionV1HighStakesGatesBlockBeforeAnyJudge",
			"TestPreJudgeGatesBlockBeforeDispatch",
		}},
		{"G", "a required and missing outside view blocks", []string{
			"TestDecisionV1HighStakesGatesBlockBeforeAnyJudge",
			"TestOutsideViewSatisfiedProceeds",
		}},
		{"H", "a required and missing premortem blocks under high stakes", []string{
			"TestRequiredPremortemBlocks",
		}},
		{"I", "sources sharing a hash or domain group together; self-description does not regroup them", []string{
			"TestRecordCarriesIndependenceCounts",
			"TestIndependenceRequirementIsOptIn",
		}},
		{"J", "high dispersion triggers a challenge; low dispersion skips it and records the skip", []string{
			"TestChallengeSkipIsRecorded",
			"TestDecisionV1DispersionTriggersChallenge",
		}},
		{"K", "round 1 → challenge → one independent revision → finalize", []string{
			"TestRevisionIsBoundedAndIndependent",
			"TestUnchangedRevisionPreservesScores",
		}},
		{"L", "a side-effecting task missing a prerequisite is policy-blocked with zero tool starts", []string{
			"TestBlockedMutationStartsZeroToolProcesses",
			"TestCommitGateRequireRollbackUsesInvokedToolContract",
			"TestStructuredMutateStepUsesTheCommitGate",
		}},
		{"M", "a reached kill criterion stops execution", []string{
			"TestCheckpointStopsTaskAndRefusesFurtherCalls",
			"TestCheckpointSchedulerOutcomesProjectCanonicalTodoState",
		}},
		{"N", "a checkpoint is evaluated every N tool calls with zero LLM calls", []string{
			"TestCheckpointStopsTaskAndRefusesFurtherCalls",
			"TestCheckpointReadsLiveReconcileClassification",
			"TestCheckpointReadsMaterialEvidenceChangeFromDecisionJournal",
		}},
		{"O", "a contradicted critical assumption stales the decision and replans without editing the old record", []string{
			"TestCheckpointSchedulerOutcomesProjectCanonicalTodoState",
			"TestDecisionV1ContradictedAssumptionStalesAndReplans",
		}},
		{"P", "a resumed decision with 2 of 3 opinions dispatches only the missing one", []string{
			"TestDecisionEngineStage3FreshEngineResumeReusesTwoOpinions",
			"TestPhase2StagesResumeWithoutRerunning",
		}},
		{"Q", "a crash during a mutation reconciles before any retry", []string{
			"TestUnknownSideEffectStateNeverAutoReplays",
		}},
		{"R", "a run missing required evidence must not be judged successful", []string{
			"TestDecisionV1InvalidatingCheckpointBlocksSuccess",
		}},
		{"S", "forbidden degradation fails closed; explicit degradation follows a fixed order and leaves an event", []string{
			"TestEveryDecisionStagePurposeIsRegistered",
			"TestDecisionBudgetExplicitDegradationLadder",
			"TestDecisionBudgetDoesNotSilentlyReduceJudges",
			"TestDegradationNeverTouchesQualityGates",
		}},
	}

	if len(rows) != 19 {
		t.Fatalf("matrix has %d rows, want the 19 of spec §46 (A–S)", len(rows))
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if seen[row.row] {
			t.Fatalf("row %s appears twice", row.row)
		}
		seen[row.row] = true
		if strings.TrimSpace(row.guaranee) == "" {
			t.Fatalf("row %s states no guarantee", row.row)
		}
		if len(row.coveredBy) == 0 {
			t.Fatalf("row %s has no named coverage", row.row)
		}
		for _, name := range row.coveredBy {
			if !strings.HasPrefix(name, "Test") {
				t.Fatalf("row %s names %q, which is not a test", row.row, name)
			}
		}
	}
	for _, letter := range "ABCDEFGHIJKLMNOPQRS" {
		if !seen[string(letter)] {
			t.Fatalf("matrix row %s is missing", string(letter))
		}
	}

	// A named test that no longer exists is an unattributed row wearing a
	// name, which is the exact failure this matrix is meant to prevent.
	declared := declaredTestNames(t)
	for _, row := range rows {
		for _, name := range row.coveredBy {
			if !declared[name] {
				t.Fatalf("matrix row %s cites %s, which no longer exists", row.row, name)
			}
		}
	}
}

// declaredTestNames reads the package's own test declarations. Reflection
// cannot enumerate test functions, so the source is the only honest source.
func declaredTestNames(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("glob test files: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no test files found; the matrix cannot verify its own citations")
	}
	pattern := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	declared := map[string]bool{}
	for _, entry := range entries {
		source, err := os.ReadFile(entry)
		if err != nil {
			t.Fatalf("read %s: %v", entry, err)
		}
		for _, match := range pattern.FindAllStringSubmatch(string(source), -1) {
			declared[match[1]] = true
		}
	}
	return declared
}

// Row J: dispersion above the profile's threshold triggers a challenge; the
// same fixture with agreeing judges skips it.
func TestDecisionV1DispersionTriggersChallenge(t *testing.T) {
	agreeing := newDecisionE2E(t)
	agreeing.formDecision(t, fixtureProfileStandard)
	if got := agreeing.judge.count(stageChallenge); got != 0 {
		t.Fatalf("agreeing judges triggered %d challenges, want none below the threshold", got)
	}

	dispersed := newDecisionE2E(t)
	dispersed.judge.dispersion = 2.0
	dispersed.formDecision(t, fixtureProfileStandard)
	if got := dispersed.judge.count(stageChallenge); got == 0 {
		t.Fatal("dispersed judges did not trigger the profile's challenge")
	}
	// A challenge that ran must be followed by the profile's bounded revision.
	if got := dispersed.judge.count(stageRevision); got == 0 {
		t.Fatal("a challenge ran but no revision followed it")
	}
}

// Row O: a contradicted critical assumption stales the governing decision and
// drives a replan, and the superseded record is never edited.
func TestDecisionV1ContradictedAssumptionStalesAndReplans(t *testing.T) {
	e := newDecisionE2E(t)
	entry := e.formDecision(t, fixtureProfileStandard)
	before, found, err := mustDecisionEntry(e, entry.DecisionID)
	if err != nil || !found {
		t.Fatalf("decision entry: found=%t err=%v", found, err)
	}

	index, err := e.coordinator.decisionIndex()
	if err != nil {
		t.Fatalf("decision index: %v", err)
	}
	if _, _, err := index.CheckAssumption(entry.DecisionID, "service-accepts",
		string(AssumptionContradicted), "the service rejected the change"); err != nil {
		t.Fatalf("CheckAssumption: %v", err)
	}

	after, found, err := mustDecisionEntry(e, entry.DecisionID)
	if err != nil || !found {
		t.Fatalf("decision entry after contradiction: found=%t err=%v", found, err)
	}
	// What the decision selected is history and must not be rewritten.
	if after.FinalOption != before.FinalOption || after.EvidenceHash != before.EvidenceHash {
		t.Fatalf("contradiction rewrote the decision: before=%q/%q after=%q/%q",
			before.FinalOption, before.EvidenceHash, after.FinalOption, after.EvidenceHash)
	}
	contradicted := false
	for _, assumption := range after.Assumptions {
		if assumption.ID == "service-accepts" && assumption.Status == AssumptionContradicted {
			contradicted = true
		}
	}
	if !contradicted {
		t.Fatalf("assumption status was not recorded: %#v", after.Assumptions)
	}
	// The contradiction is what a checkpoint reads to stop or replan.
	if got := CriticalContradiction(after.Assumptions); got != "service-accepts" {
		t.Fatalf("CriticalContradiction = %q, want the contradicted assumption", got)
	}
}

func mustDecisionEntry(e *decisionE2E, decisionID string) (DecisionIndexEntry, bool, error) {
	index, err := e.coordinator.decisionIndex()
	if err != nil {
		return DecisionIndexEntry{}, false, err
	}
	return index.Get(decisionID)
}

// Row R: a submit_result that invalidates its own decision must not leave a
// success projection behind.
func TestDecisionV1InvalidatingCheckpointBlocksSuccess(t *testing.T) {
	c := boundaryCoordinator(t, deployerAgent())
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent: "deployer", Desc: "mutate", Goal: "mutate",
	}})[0]
	task := mutatingTask()
	task.Agent = "deployer"
	policy := disciplinePolicy(CommitGatePolicy{}, StopPolicy{CheckpointEvery: 1}, ReplanPolicy{})
	if err := c.armDiscipline(context.Background(), item.ID, task, policy, &DecisionRecord{ID: "dec-1"}); err != nil {
		t.Fatalf("arm: %v", err)
	}

	// An unaccountable side effect stops the task at its next checkpoint.
	c.taskTracker.TodoList().SetRecoveryState(item.ID, RecoveryStateUnknown)
	if decision := c.recordToolCall(context.Background(), item.ID, false); decision.Action != CheckpointStop {
		t.Fatalf("checkpoint = %#v, want a stop", decision)
	}

	// A stopped task refuses further tool calls, and cannot be terminalized as
	// a success.
	if denial := c.checkpointDenial(item.ID); denial == "" {
		t.Fatal("a stopped task still admitted tool calls")
	}
	// Two independent boundaries refuse the success: the checkpoint already
	// projected the task terminal-blocked, and the armed discipline refuses a
	// successful terminalization outright. Either is sufficient; assert the
	// outcome rather than which one fired first.
	if err := c.CommitTaskTransition(context.Background(), item.ID, item.Status, TaskDone,
		"worker claims success", "", nil); err == nil {
		t.Fatal("a stopped decision task was terminalized as successful")
	}
	if got := todoItemByID(c.taskTracker.TodoList().Items(), item.ID); got == nil || got.Status == TaskDone {
		t.Fatalf("task projection = %#v, want anything but done", got)
	}
	// The discipline's own refusal is the boundary that does not depend on the
	// projected status, so prove it directly.
	discipline := c.disciplineFor(item.ID)
	if discipline == nil {
		t.Fatal("the stopped task disarmed its discipline")
	}
	discipline.mu.Lock()
	stopped := discipline.stopped
	discipline.mu.Unlock()
	if !stopped {
		t.Fatal("the discipline did not record the stop that forbids success")
	}
}

// Every lifecycle event the specification declares must actually be emitted by
// some production path. A constant with no caller is a lifecycle stage that
// cannot be observed, which is indistinguishable from one that never runs.
func TestDeclaredDecisionEventsAreEmittedBySomeProductionPath(t *testing.T) {
	sources := map[string]string{}
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob sources: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry, "_test.go") {
			continue
		}
		content, err := os.ReadFile(entry)
		if err != nil {
			t.Fatalf("read %s: %v", entry, err)
		}
		sources[entry] = string(content)
	}

	// Names reserved for behavior V1 does not implement. Each is listed with
	// why, so an unexplained gap cannot hide behind this map. Emitting one of
	// these would mean building the feature it records — in the case below,
	// an operator bypass of a safety gate, which is not something to add in
	// order to satisfy an event name.
	reservedUnimplemented := map[string]string{
		agent.EventDecisionAlternativesOverride: "spec §19 reserves it for an operator emergency override of the " +
			"alternatives gate; V1 implements no such bypass, so nothing can legitimately emit it",
	}

	for _, event := range decisionLifecycleEvents() {
		constName := decisionEventConstantName(event)
		if constName == "" {
			t.Fatalf("event %q has no constant name mapping in this test", event)
		}
		found := false
		for name, content := range sources {
			if name == "decision_reason.go" {
				continue
			}
			if strings.Contains(content, constName) {
				found = true
				break
			}
		}
		if !found {
			if why, ok := reservedUnimplemented[event]; ok {
				t.Logf("event %s is reserved and not emitted: %s", event, why)
				continue
			}
			t.Fatalf("event %s (%s) is declared but no production file references it", event, constName)
		}
		if why, ok := reservedUnimplemented[event]; ok {
			t.Fatalf("event %s is listed as reserved (%s) but production code now references it; "+
				"remove it from the reserved list", event, why)
		}
	}
}

// decisionEventConstantName maps an event's wire name to the Go constant that
// production code would reference.
func decisionEventConstantName(event string) string {
	switch event {
	case agent.EventDecisionStarted:
		return "EventDecisionStarted"
	case agent.EventDecisionOptionsProposed:
		return "EventDecisionOptionsProposed"
	case agent.EventDecisionEvidenceSealed:
		return "EventDecisionEvidenceSealed"
	case agent.EventDecisionEvidenceChanged:
		return "EventDecisionEvidenceChanged"
	case agent.EventDecisionReferenceStarted:
		return "EventDecisionReferenceStarted"
	case agent.EventDecisionReferenceCompleted:
		return "EventDecisionReferenceCompleted"
	case agent.EventDecisionReferenceFailed:
		return "EventDecisionReferenceFailed"
	case agent.EventDecisionOpinionSubmitted:
		return "EventDecisionOpinionSubmitted"
	case agent.EventDecisionOpinionRejected:
		return "EventDecisionOpinionRejected"
	case agent.EventDecisionJudgeOverallIgnored:
		return "EventDecisionJudgeOverallIgnored"
	case agent.EventDecisionAggregateComputed:
		return "EventDecisionAggregateComputed"
	case agent.EventDecisionChallengeSubmitted:
		return "EventDecisionChallengeSubmitted"
	case agent.EventDecisionChallengeSkipped:
		return "EventDecisionChallengeSkipped"
	case agent.EventDecisionPremortemSubmitted:
		return "EventDecisionPremortemSubmitted"
	case agent.EventDecisionRevisionSubmitted:
		return "EventDecisionRevisionSubmitted"
	case agent.EventDecisionFinalizationResult:
		return "EventDecisionFinalizationResult"
	case agent.EventDecisionFinalizationOverride:
		return "EventDecisionFinalizationOverride"
	case agent.EventDecisionFinalized:
		return "EventDecisionFinalized"
	case agent.EventDecisionAlternativesOverride:
		return "EventDecisionAlternativesOverride"
	case agent.EventDecisionBudgetDegraded:
		return "EventDecisionBudgetDegraded"
	case agent.EventDecisionEvidenceSharedOrigin:
		return "EventDecisionEvidenceSharedOrigin"
	case agent.EventDecisionInvalidated:
		return "EventDecisionInvalidated"
	case agent.EventDecisionRunEnvelopeAnchored:
		return "EventDecisionRunEnvelopeAnchored"
	case agent.EventRequestContractCommitted:
		return "EventRequestContractCommitted"
	case agent.EventAssumptionDeclared:
		return "EventAssumptionDeclared"
	case agent.EventAssumptionSupported:
		return "EventAssumptionSupported"
	case agent.EventAssumptionContradicted:
		return "EventAssumptionContradicted"
	case agent.EventAssumptionStale:
		return "EventAssumptionStale"
	case agent.EventReplanRequested:
		return "EventReplanRequested"
	case agent.EventReplanCompleted:
		return "EventReplanCompleted"
	default:
		return ""
	}
}

// An assumption is declared when the evidence it belongs to is sealed, so
// every later status change is a transition from a recorded beginning.
func TestDecisionV1SealingDeclaresItsAssumptions(t *testing.T) {
	e := newDecisionE2E(t)
	e.formDecision(t, fixtureProfileStandard)

	if got := e.journal.count(agent.EventAssumptionDeclared); got != 1 {
		t.Fatalf("assumption_declared events = %d, want one per declared assumption: %v",
			got, e.journal.typesOf())
	}
	// The declaration must precede any status change for the same assumption.
	types := e.journal.typesOf()
	declaredAt, sealedAt := -1, -1
	for i, event := range types {
		if event == agent.EventDecisionEvidenceSealed && sealedAt < 0 {
			sealedAt = i
		}
		if event == agent.EventAssumptionDeclared && declaredAt < 0 {
			declaredAt = i
		}
	}
	if sealedAt < 0 || declaredAt < 0 || declaredAt < sealedAt {
		t.Fatalf("assumption was declared at %d, sealing at %d; declaration must follow the seal it belongs to: %v",
			declaredAt, sealedAt, types)
	}
}

// A finalizer that departs from the aggregate records the departure as its own
// event, so an auditor does not have to recompute the aggregate to find one.
func TestFinalizationOverrideIsItsOwnEvent(t *testing.T) {
	for _, tc := range []struct {
		name         string
		optionID     string
		wantOverride int
	}{
		{name: "agreeing finalization", optionID: "execute", wantOverride: 0},
		{name: "override", optionID: "reduce", wantOverride: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDecisionE2E(t)
			// The finalizer answers with a different option than the judges
			// scored highest, which is what makes the outcome an override.
			e.judge.finalizeAs = tc.optionID
			e.formDecision(t, fixtureProfileCoordinator)

			if got := e.journal.count(agent.EventDecisionFinalizationOverride); got != tc.wantOverride {
				t.Fatalf("finalization_override events = %d, want %d: %v",
					got, tc.wantOverride, e.journal.typesOf())
			}
			// The result event is always written, override or not.
			if got := e.journal.count(agent.EventDecisionFinalizationResult); got == 0 {
				t.Fatalf("no finalization result event: %v", e.journal.typesOf())
			}
		})
	}
}

// A replan that produced its replacement attempt records the completion, not
// only the request. Recording only the request leaves a reader unable to tell
// an abandoned plan from one that got its replacement.
func TestReplanRecordsRequestAndCompletion(t *testing.T) {
	c := boundaryCoordinator(t, deployerAgent())
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent: "deployer", Desc: "mutate", Goal: "mutate",
	}})[0]
	journal := c.eventJournal.(*memoryJournal)

	task := mutatingTask()
	task.Agent = "deployer"
	policy := disciplinePolicy(CommitGatePolicy{},
		StopPolicy{CheckpointEvery: 1},
		ReplanPolicy{OnCriticalAssumptionContradicted: CheckpointReplan})
	record := &DecisionRecord{ID: "dec-1", EvidenceHash: "hash-1", Assumptions: []DecisionAssumption{{
		ID: "a1", Statement: "the service accepts the change", Critical: true, Status: AssumptionContradicted,
	}}}
	if err := c.armDiscipline(context.Background(), item.ID, task, policy, record); err != nil {
		t.Fatalf("arm: %v", err)
	}

	decision := c.recordToolCall(context.Background(), item.ID, false)
	if decision.Action != CheckpointReplan {
		t.Fatalf("checkpoint = %#v, want a replan", decision)
	}
	if got := journal.count(agent.EventReplanRequested); got != 1 {
		t.Fatalf("replan_requested = %d, want 1: %v", got, journal.typesOf())
	}
	if got := journal.count(agent.EventReplanCompleted); got != 1 {
		t.Fatalf("replan_completed = %d, want 1: %v", got, journal.typesOf())
	}
}

// A discipline that could not persist its armed contract must not stay armed.
// The caller receives an error and no cleanup function, so a published
// discipline would gate the tools of whatever ran under that todo next.
func TestArmDisciplineLeavesNothingArmedWhenPersistenceFails(t *testing.T) {
	c := boundaryCoordinator(t, deployerAgent())
	c.SetEventJournal(failingJournal{err: errors.New("journal unavailable")})

	task := mutatingTask()
	task.Agent = "deployer"
	policy := disciplinePolicy(CommitGatePolicy{}, StopPolicy{CheckpointEvery: 1}, ReplanPolicy{})

	err := c.armDiscipline(context.Background(), "todo-1", task, policy, &DecisionRecord{ID: "dec-1"})
	if err == nil {
		t.Fatal("arming succeeded with a journal that cannot append")
	}
	if c.disciplineFor("todo-1") != nil {
		t.Fatal("a failed arm left an armed discipline behind")
	}
	// The hooks it would have activated stay no-ops.
	if denial := c.commitGateDenial(context.Background(), "todo-1", "bash", `{"command":"touch out.txt"}`); denial != "" {
		t.Fatalf("a failed arm still gated a tool: %q", denial)
	}
	if denial := c.checkpointDenial("todo-1"); denial != "" {
		t.Fatalf("a failed arm still denied a tool: %q", denial)
	}
}
