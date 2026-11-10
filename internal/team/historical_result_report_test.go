package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"
)

func TestHistoricalResultGrantReportRequiresCanonicalDispatch(t *testing.T) {
	c, item, input := historicalContextFixture(t)
	_, manifest := historicalContextManifest(t, c, item, input)
	assertState := func(want string) {
		t.Helper()
		reports, err := c.HistoricalResultGrantReport(t.Context())
		if err != nil || len(reports) != 1 || reports[0].State != want {
			t.Fatalf("grant report = %+v, %v; want %s", reports, err, want)
		}
		encoded, err := json.Marshal(reports)
		if err != nil || strings.Contains(string(encoded), "complete deliverable") {
			t.Fatal("grant report disclosed source bodies")
		}
	}
	assertState("not_dispatched")
	if err := c.persistContextManifest(&manifest); err != nil {
		t.Fatal(err)
	}
	assertState("prepared")
	forged := *cloneContextInjectionManifest(&manifest)
	forged.ModelCalled, forged.Outcome = true, "model_call"
	forged.Fingerprint = historicalResultManifestFingerprint(forged)
	if err := c.taskTracker.TodoList().SetContextManifest(item.ID, &forged); err != nil {
		t.Fatal(err)
	}
	assertState("prepared")
	if _, err := markDependencyViewModelCall(c.bindDependencyViewCall(t.Context(), &manifest)); err != nil {
		t.Fatal(err)
	}
	assertState("dispatched")
	if err := c.EventStore().Close(); err != nil {
		t.Fatal(err)
	}
	c.eventStore = nil
	c.SetEventJournal(eventStoreJournal{})
	assertState("dispatched")
}

type historicalHeadRaceJournal struct {
	eventStoreJournal
	other *EventStore
}

func (j historicalHeadRaceJournal) AppendBatchAtHead(ctx context.Context, events []RunEvent, head string) ([]RunEvent, error) {
	if _, err := j.other.AppendPersistedContext(ctx, RunEvent{Type: string(EventUserMessageAdded), Actor: "user", Payload: []byte(`{"role":"user","content":"concurrent change"}`)}); err != nil {
		return nil, err
	}
	return j.eventStoreJournal.AppendBatchAtHead(ctx, events, head)
}

func TestHistoricalResultRequestFencesConcurrentHeadChange(t *testing.T) {
	c, rows := historicalRequestFixture(t)
	ctx := claimHistoricalRequestFixture(t, c, rows)
	other, err := OpenEventStore(c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	c.SetEventJournal(historicalHeadRaceJournal{eventStoreJournal: eventStoreJournal{store: c.EventStore()}, other: other})
	if _, err := c.beginFollowUpWorkflow(ctx, "new request"); !errors.Is(err, ErrEventHeadChanged) {
		t.Fatalf("stale source read was not fenced: %v", err)
	}
	if err := c.EventStore().VerifyHashChain(); err != nil {
		t.Fatal(err)
	}
	for _, event := range mustReadEvents(t, c.EventStore()) {
		if event.Type == string(EventWorkflowCycleStarted) || event.Type == string(EventHistoricalResultsGranted) {
			t.Fatal("head conflict retained a partial cycle admission")
		}
	}
}

func TestHistoricalResultResumeSourcesPrecedeSemanticStartup(t *testing.T) {
	c, rows := historicalRequestFixture(t)
	ctx := claimHistoricalRequestFixture(t, c, rows)
	if _, err := c.beginFollowUpWorkflow(ctx, "new request"); err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateHistoricalResultCycleSources(t.Context()); err != nil {
		t.Fatal(err)
	}
	(*rows)[0].Validity = "invalidated"
	if err := c.ValidateHistoricalResultStartupSources(t.Context()); err == nil {
		t.Fatal("restored invalid source passed startup admission")
	}
	if err := c.prepareInvocationInputs(WithSessionResume(t.Context()), "new request"); err == nil {
		t.Fatal("restored invalid source reached input or model preparation")
	}
}
