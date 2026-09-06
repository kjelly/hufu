package team

import (
	"context"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestBranchScopedDecisionJournalIsolatesSiblingProjectionAndParentTail(t *testing.T) {
	workspace := t.TempDir()
	es, err := NewEventStore(workspace, "run-journal", "session-journal")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = es.Close() }()

	tree := NewSessionTree()
	tree.ActiveBranch = "main"
	mainJournal, err := newBranchScopedDecisionJournal(eventStoreJournal{store: es}, tree)
	if err != nil {
		t.Fatal(err)
	}
	if err := appendDecisionEvent(context.Background(), mainJournal, agent.EventDecisionStarted, decisionEvent{
		DecisionID: "decision-shared", Profile: "main", IdempotencyKey: "stage-start",
	}); err != nil {
		t.Fatal(err)
	}
	mainEvents, err := es.ReadEvents()
	if err != nil || len(mainEvents) != 1 {
		t.Fatalf("main event count = %d, err = %v", len(mainEvents), err)
	}
	forkEventID := mainEvents[0].ID

	left, err := tree.CreateBranch("left", forkEventID, es)
	if err != nil {
		t.Fatal(err)
	}
	right, err := tree.CreateBranch("right", forkEventID, es)
	if err != nil {
		t.Fatal(err)
	}

	tree.ActiveBranch = left.ID
	leftJournal, err := newBranchScopedDecisionJournal(eventStoreJournal{store: es}, tree)
	if err != nil {
		t.Fatal(err)
	}
	if err := appendDecisionEvent(context.Background(), leftJournal, agent.EventDecisionStarted, decisionEvent{
		DecisionID: "decision-shared", Profile: "left", IdempotencyKey: "stage-left",
	}); err != nil {
		t.Fatal(err)
	}

	// This parent event is after the fork and must not be visible in either
	// sibling's lineage, even though it is on the same global hash chain.
	if _, err := es.AppendPersisted(RunEvent{
		BranchID: "main", Type: agent.EventDecisionStarted, Actor: decisionActor,
		IdempotencyKey: "parent-after-fork", Payload: []byte(`{"decision_id":"decision-shared","profile":"main-after"}`),
	}); err != nil {
		t.Fatal(err)
	}

	tree.ActiveBranch = left.ID
	leftJournal, err = newBranchScopedDecisionJournal(eventStoreJournal{store: es}, tree)
	if err != nil {
		t.Fatal(err)
	}
	leftState, err := projectDecision(context.Background(), leftJournal, "decision-shared")
	if err != nil {
		t.Fatal(err)
	}
	if leftState.Profile != "left" {
		t.Fatalf("left projection profile = %q, want left", leftState.Profile)
	}

	tree.ActiveBranch = right.ID
	rightJournal, err := newBranchScopedDecisionJournal(eventStoreJournal{store: es}, tree)
	if err != nil {
		t.Fatal(err)
	}
	rightState, err := projectDecision(context.Background(), rightJournal, "decision-shared")
	if err != nil {
		t.Fatal(err)
	}
	if rightState.Profile != "main" {
		t.Fatalf("right projection profile = %q, want inherited main", rightState.Profile)
	}
	if events, err := rightJournal.ReadEvents(context.Background()); err != nil || len(events) != 1 {
		t.Fatalf("right lineage events = %d, err = %v; want fork parent only", len(events), err)
	}
}

func TestBranchScopedDecisionJournalNamespacesRetryDedupeByBranch(t *testing.T) {
	workspace := t.TempDir()
	es, err := NewEventStore(workspace, "run-journal", "session-journal")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = es.Close() }()

	tree := NewSessionTree()
	left, err := tree.CreateBranch("left", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	right, err := tree.CreateBranch("right", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	appendOn := func(branchID string) RunEvent {
		tree.ActiveBranch = branchID
		journal, journalErr := newBranchScopedDecisionJournal(eventStoreJournal{store: es}, tree)
		if journalErr != nil {
			t.Fatal(journalErr)
		}
		event, appendErr := journal.Append(context.Background(), RunEvent{
			Type: agent.EventDecisionStarted, Actor: decisionActor,
			IdempotencyKey: "same-logical-transition", Payload: []byte(`{"decision_id":"dedupe"}`),
		})
		if appendErr != nil {
			t.Fatal(appendErr)
		}
		return event
	}

	leftFirst := appendOn(left.ID)
	leftRetry := appendOn(left.ID)
	rightFirst := appendOn(right.ID)
	if leftFirst.ID != leftRetry.ID {
		t.Fatalf("left retry event ID = %q, want original %q", leftRetry.ID, leftFirst.ID)
	}
	if leftFirst.ID == rightFirst.ID {
		t.Fatalf("sibling branches shared dedupe identity %q", leftFirst.ID)
	}
	if leftFirst.BranchID != left.ID || rightFirst.BranchID != right.ID {
		t.Fatalf("stamped branches = %q/%q, want %q/%q", leftFirst.BranchID, rightFirst.BranchID, left.ID, right.ID)
	}
	if leftFirst.IdempotencyKey == "same-logical-transition" || leftFirst.IdempotencyKey == rightFirst.IdempotencyKey {
		t.Fatalf("branch idempotency keys were not namespaced: %q and %q", leftFirst.IdempotencyKey, rightFirst.IdempotencyKey)
	}
	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("global event count = %d, want one durable event per sibling branch", len(events))
	}
}

func TestBranchScopedDecisionJournalRejectsReturnedBranchMismatch(t *testing.T) {
	tree := NewSessionTree()
	journal, err := newBranchScopedDecisionJournal(wrongBranchJournal{}, tree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), RunEvent{Type: agent.EventDecisionStarted, IdempotencyKey: "key"}); err == nil {
		t.Fatal("accepted an append returned from the wrong branch")
	}
}

func TestProjectEventsForBranchValidatesStrictMultigenerationLineage(t *testing.T) {
	tree := NewSessionTree()
	left, err := tree.CreateBranch("left", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	left.ForkEventID = "main-cut"
	grandchild, err := tree.CreateBranch("grandchild", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	grandchild.ParentID = left.ID
	grandchild.ForkEventID = "left-cut"
	events := []RunEvent{
		{ID: "main-before", BranchID: "main"},
		{ID: "main-cut", BranchID: "main"},
		{ID: "main-after", BranchID: "main"},
		{ID: "left-cut", BranchID: left.ID},
		{ID: "left-after", BranchID: left.ID},
		{ID: "grandchild-event", BranchID: grandchild.ID},
	}
	lineage, err := projectEventsForBranch(events, tree, grandchild.ID)
	if err != nil {
		t.Fatalf("projectEventsForBranch = %v", err)
	}
	got := make([]string, 0, len(lineage))
	for _, event := range lineage {
		got = append(got, event.ID)
	}
	want := []string{"main-before", "main-cut", "left-cut", "grandchild-event"}
	if len(got) != len(want) {
		t.Fatalf("lineage IDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lineage IDs = %v, want %v", got, want)
		}
	}
}

func TestProjectEventsForBranchRejectsInvalidForkReferences(t *testing.T) {
	tests := []struct {
		name      string
		forkEvent string
	}{
		{name: "missing", forkEvent: "does-not-exist"},
		{name: "sibling", forkEvent: "right-event"},
		{name: "descendant", forkEvent: "left-descendant-event"},
		{name: "beyond-parent-cutoff", forkEvent: "main-after-cut"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tree := NewSessionTree()
			left, err := tree.CreateBranch("left", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			left.ForkEventID = "main-cut"
			right, err := tree.CreateBranch("right", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			right.ForkEventID = "main-cut"
			child, err := tree.CreateBranch("child", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			child.ParentID = left.ID
			child.ForkEventID = tc.forkEvent
			descendant, err := tree.CreateBranch("descendant", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			descendant.ParentID = left.ID
			descendant.ForkEventID = "main-cut"
			events := []RunEvent{
				{ID: "main-cut", BranchID: "main"},
				{ID: "main-after-cut", BranchID: "main"},
				{ID: "left-event", BranchID: left.ID},
				{ID: "left-descendant-event", BranchID: descendant.ID},
				{ID: "right-event", BranchID: right.ID},
			}
			if tc.name == "beyond-parent-cutoff" {
				child.ForkEventID = "main-after-cut"
			}
			if _, err := projectEventsForBranch(events, tree, child.ID); err == nil {
				t.Fatalf("accepted invalid %s fork %q", tc.name, child.ForkEventID)
			}
			if filtered := FilterEventsForBranch(events, tree, child.ID); filtered != nil {
				t.Fatalf("compatibility filter exposed invalid %s lineage: %#v", tc.name, filtered)
			}
		})
	}
}

func TestProjectEventsForBranchEmptyForkDoesNotInheritParentHistory(t *testing.T) {
	tree := NewSessionTree()
	child, err := tree.CreateBranch("empty-fork", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	events := []RunEvent{
		{ID: "main-event", BranchID: "main"},
		{ID: "child-event", BranchID: child.ID},
	}
	lineage, err := projectEventsForBranch(events, tree, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(lineage) != 1 || lineage[0].ID != "child-event" {
		t.Fatalf("empty-fork lineage = %#v, want child event only", lineage)
	}
}

type wrongBranchJournal struct{}

func (wrongBranchJournal) Append(context.Context, RunEvent) (RunEvent, error) {
	return RunEvent{BranchID: "wrong"}, nil
}

func (wrongBranchJournal) ReadEvents(context.Context) ([]RunEvent, error) { return nil, nil }

func (wrongBranchJournal) VerifyHashChain(context.Context) error { return nil }
