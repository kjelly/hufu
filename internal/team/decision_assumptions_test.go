package team

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

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
	c, _ := armedAssumptionCoordinator(t)
	_, err := c.ApplyAssumptionChecks(context.Background(), "todo-1", []AssumptionCheck{
		{AssumptionID: "A9", Status: AssumptionSupported},
	}, AssumptionSourceTaskResult)
	if err == nil || !strings.Contains(err.Error(), "not declared on this decision") {
		t.Fatalf("ApplyAssumptionChecks = %v, want an undeclared-assumption rejection", err)
	}
}

func TestApplyAssumptionChecksRejectsUnreportableStatus(t *testing.T) {
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
	c := disciplineCoordinator(t)
	applied, err := c.ApplyAssumptionChecks(context.Background(), "todo-unarmed", []AssumptionCheck{
		{AssumptionID: "A1", Status: AssumptionSupported},
	}, AssumptionSourceTaskResult)
	if err != nil || applied != 0 {
		t.Fatalf("ApplyAssumptionChecks = %d, %v, want a silent no-op", applied, err)
	}
}

func TestApplyAssumptionChecksFailsClosedWithoutJournal(t *testing.T) {
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
}

func TestProjectDecisionReplaysAssumptionTransitionAndInvalidation(t *testing.T) {
	j := &memoryJournal{}
	record := &DecisionRecord{ID: "dec-replay", Assumptions: []DecisionAssumption{{ID: "A1", Critical: true}}}
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
	index := newIndex(t)
	j := &memoryJournal{}
	record := &DecisionRecord{ID: "dec-rebuild", RunID: "run-1", TaskID: "task-1",
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
