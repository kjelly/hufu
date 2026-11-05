package team

import "testing"

func TestDecisionJournalRebindsToNextInvocation(t *testing.T) {
	c := newTestCoordinator(t)
	if err := SaveSessionTree(c.session.Workspace, NewSessionTree()); err != nil {
		t.Fatal(err)
	}
	c.initEventStore()
	first, err := c.pinDecisionControlPlane()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.eventStore.Close(); err != nil {
		t.Fatal(err)
	}
	c.eventStore = nil
	c.initEventStore()
	second, err := c.pinDecisionControlPlane()
	if err != nil || second == first || second.index != first.index || second.artifactStore != first.artifactStore {
		t.Fatal("invocation did not rebind its journal while preserving shared decision storage", err)
	}
	if _, err := second.journal.Append(t.Context(), RunEvent{Type: "journal_rebound", Actor: "coordinator"}); err != nil {
		t.Fatal("new invocation retained a closed event-store handle", err)
	}
	if _, err := first.journal.Append(t.Context(), RunEvent{Type: "stale_journal", Actor: "coordinator"}); err == nil {
		t.Fatal("an old invocation's pinned journal was silently redirected")
	}
}
