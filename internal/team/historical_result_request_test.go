package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func historicalRequestFixture(t *testing.T) (*Coordinator, *[]HistoricalTaskResult) {
	t.Helper()
	c, rows, _ := historicalGrantPreparationFixture(t)
	c.session.Config.Workflow = agent.WorkflowConfig{Phases: []string{"prepare", "audit", "execute", "verify"}}
	c.session.Config.ResultTransport = agent.ResultTransportTool
	var err error
	c.phaseWorkflow, err = newRuntimeWorkflow(c.session)
	if err != nil {
		t.Fatal(err)
	}
	c.executionPolicy, err = newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveSessionTree(c.session.Workspace, NewSessionTree()); err != nil {
		t.Fatal(err)
	}
	store, err := NewEventStore(c.session.Workspace, "prior-run", "request-session")
	if err != nil {
		t.Fatal(err)
	}
	c.eventStore = store
	c.SetEventJournal(eventStoreJournal{store: store})
	store.SetBranchID("main")
	policy, err := json.Marshal(c.ExecutionPolicySnapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []RunEvent{
		{Type: string(EventExecutionPolicySnapshot), Actor: "coordinator", Payload: policy},
		{Type: string(EventRunStarted), Actor: "coordinator", Payload: []byte(`{"team":"test"}`)},
		{Type: string(EventWorkflowStateChanged), Actor: "runtime", Payload: []byte(`{"state":"DONE","phase_results":{"VERIFY":{"status":"SUCCESS"}}}`)},
	} {
		if _, err := store.AppendPersisted(event); err != nil {
			t.Fatal(err)
		}
	}
	c.SetSessionData(ReduceToSessionData(mustReadEvents(t, store)))
	c.executionRunID = "new-cycle"
	return c, rows
}

func claimHistoricalRequestFixture(t *testing.T, c *Coordinator, rows *[]HistoricalTaskResult) context.Context {
	t.Helper()
	request, err := c.PrepareHistoricalResultRequest(t.Context(), "new request", []string{"worker=" + (*rows)[0].ResultID})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithHistoricalResultRequest(t.Context(), request)
	if _, err := c.claimHistoricalResultRequest(ctx, "new request"); err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestHistoricalResultRequestCommitsCycleAndGrantTogether(t *testing.T) {
	c, rows := historicalRequestFixture(t)
	ctx := claimHistoricalRequestFixture(t, c, rows)
	if started, err := c.beginFollowUpWorkflow(ctx, "new request"); err != nil || !started {
		t.Fatalf("historical follow-up = %v, %v", started, err)
	}
	events := mustReadEvents(t, c.EventStore())
	var boundary, grant int
	for i, event := range events {
		if event.Type == string(EventWorkflowCycleStarted) {
			boundary = i
		}
		if event.Type == string(EventHistoricalResultsGranted) {
			grant = i
		}
	}
	if boundary < 1 || grant != boundary+1 || c.SessionData().HistoricalResultGrants == nil {
		t.Fatal("cycle and grant were not adjacent durable canonical events")
	}
	if err := CompareCanonicalProjection(c.SessionData(), events); err != nil {
		t.Fatal(err)
	}
	reports, err := c.HistoricalResultGrantReport(t.Context())
	if err != nil || len(reports) != 1 || reports[0].State != "not_dispatched" || len(c.taskTracker.TodoList().Items()) != 0 {
		t.Fatalf("unused recipient report = %+v, %v", reports, err)
	}
	routing, err := c.historicalResultRoutingMetadata(t.Context())
	if err != nil || !strings.Contains(routing, (*rows)[0].ResultID) || strings.Contains(routing, "complete deliverable") {
		t.Fatalf("coordinator routing exposed historical content: %v", err)
	}
	if _, err := c.PrepareHistoricalResultRequest(t.Context(), "another request", []string{"worker=" + (*rows)[0].ResultID}); err == nil {
		t.Fatal("unfinished cycle accepted additional grants")
	}
}

func TestHistoricalResultRequestAtomicFailure(t *testing.T) {
	for _, failure := range []string{"index", "sync", "source_drift"} {
		t.Run(failure, func(t *testing.T) {
			c, rows := historicalRequestFixture(t)
			ctx := claimHistoricalRequestFixture(t, c, rows)
			before := mustReadEvents(t, c.EventStore())
			switch failure {
			case "index":
				if _, err := c.EventStore().store.DB().ExecContext(t.Context(), `CREATE TRIGGER refuse_grant_cycle BEFORE INSERT ON historical_grant_cycles BEGIN SELECT RAISE(ABORT, 'fixture rejection'); END`); err != nil {
					t.Fatal(err)
				}
			case "sync":
				c.SetEventJournal(historicalSyncFailureJournal{eventStoreJournal{store: c.EventStore()}})
			case "source_drift":
				(*rows)[0].Validity = "invalidated"
			}
			if _, err := c.beginFollowUpWorkflow(ctx, "new request"); err == nil {
				t.Fatal("failed admission returned success")
			}
			store, err := OpenEventStoreReadOnly(c.session.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			after := mustReadEvents(t, store)
			if failure != "sync" {
				if !reflect.DeepEqual(before, after) {
					t.Fatal("failed intake left a partial cycle or grant")
				}
				return
			}
			if len(after) != len(before)+2 {
				t.Fatal("uncertain commit retained only part of its cycle")
			}
			if _, err := store.validateHistoricalResultGrantProjection(t.Context(), after); err != nil {
				t.Fatalf("restart lost the original grant index: %v", err)
			}
		})
	}
}

type historicalSyncFailureJournal struct{ eventStoreJournal }

func (j historicalSyncFailureJournal) AppendBatchAtHead(ctx context.Context, events []RunEvent, head string) ([]RunEvent, error) {
	j.store.commitHook = func() error { return errors.New("fixture sync failure") }
	return j.eventStoreJournal.AppendBatchAtHead(ctx, events, head)
}

func TestHistoricalResultRequestRejectsUnsupportedEntriesBeforeModels(t *testing.T) {
	for _, entry := range []string{"resume", "direct", "continue", "dry_run", "other_owner", "prompt_changed", "source_changed", "inactive_branch"} {
		t.Run(entry, func(t *testing.T) {
			c, rows := historicalRequestFixture(t)
			request, err := c.PrepareHistoricalResultRequest(t.Context(), "new request", []string{"worker=" + (*rows)[0].ResultID})
			if err != nil {
				t.Fatal(err)
			}
			ctx := WithHistoricalResultRequest(t.Context(), request)
			before := mustReadEvents(t, c.EventStore())
			switch entry {
			case "resume":
				_, err = c.Run(WithSessionResume(ctx), "new request")
			case "direct":
				_, err = c.RunDirectAgent(ctx, "worker", "new request")
			case "continue":
				_, err = c.ContinueWithPrompt(ctx, "new request")
			case "dry_run":
				_, err = c.DryRun(ctx, "new request")
			case "other_owner":
				_, err = newTestCoordinator(t).Run(ctx, "new request")
			case "prompt_changed":
				_, err = c.Run(ctx, "different request")
			case "source_changed":
				(*rows)[0].Validity = "invalidated"
				_, err = c.Run(ctx, "new request")
			case "inactive_branch":
				tree, loadErr := LoadSessionTree(c.session.Workspace)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				tree.Branches["other"] = &SessionBranch{ID: "other", Name: "other", ParentID: "main", ForkEventID: before[len(before)-1].ID, CreatedAt: tree.Branches["main"].CreatedAt}
				if saveErr := SaveSessionTree(c.session.Workspace, tree); saveErr != nil {
					t.Fatal(saveErr)
				}
				c.recoveryBranch = "other"
				_, err = c.Run(ctx, "new request")
			}
			if err == nil || !reflect.DeepEqual(before, mustReadEvents(t, c.EventStore())) {
				t.Fatalf("unsupported intake crossed the invocation boundary: %v", err)
			}
		})
	}
}
