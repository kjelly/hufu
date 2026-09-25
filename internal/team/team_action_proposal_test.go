package team

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/utils"
)

type proposalFixture struct {
	c      *Coordinator
	events *EventStore
	items  map[string]*TodoItem
}

// newProposalFixture builds a catalog coordinator with a durable event store
// and one open task occurrence per proposer.
func newProposalFixture(t *testing.T) proposalFixture {
	t.Helper()
	session := loadActionCatalogTeam(t, actionCatalogTestManifest("", actionCatalogTestEntry))
	session.Workspace = t.TempDir()
	events, err := NewEventStore(session.Workspace, "run-proposals", "session-proposals")
	if err != nil {
		t.Fatal(err)
	}
	events.SetBranchID("main")
	t.Cleanup(func() { _ = events.Close() })
	c := &Coordinator{session: session, taskTracker: NewTaskTracker(), eventStore: events, executionRunID: "run-proposals"}
	c.SetEventJournal(eventStoreJournal{store: events})
	items := make(map[string]*TodoItem)
	for _, agentName := range []string{"runtime-engineer", "network-engineer"} {
		items[agentName] = c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: agentName, Desc: "diagnose"}})[0]
	}
	return proposalFixture{c: c, events: events, items: items}
}

func (f proposalFixture) propose(t *testing.T, agentName, input string) (string, bool) {
	t.Helper()
	item := f.items[agentName]
	ctx := occurrenceTestContext(f.c, item.ID, 1)
	ctx = context.WithValue(ctx, todoIDKey{}, item.ID)
	ctx = context.WithValue(ctx, tools.AgentNameKey, agentName)
	tool := &teamActionProposeTool{coordinator: f.c, todoID: item.ID, agent: agentName}
	response, err := tool.Run(ctx, fantasy.ToolCall{Name: teamActionProposeToolName, Input: input})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	return response.Content, response.IsError
}

func (f proposalFixture) proposalEvents(t *testing.T) []RunEvent {
	t.Helper()
	stored, err := f.events.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	var result []RunEvent
	for _, event := range stored {
		if event.Type == string(EventTeamActionProposed) {
			result = append(result, event)
		}
	}
	return result
}

const validProposal = `{"action":"collect-debug-bundle","arguments":{"service":"api"},"assessment":"recommended","rationale":"api latency spiked"}`

func TestTeamActionProposeRecordsDurableProposal(t *testing.T) {
	f := newProposalFixture(t)
	todosBefore := len(f.c.taskTracker.TodoList().Items())
	content, isError := f.propose(t, "runtime-engineer", validProposal)
	if got := len(f.c.taskTracker.TodoList().Items()); got != todosBefore {
		t.Fatalf("a proposal changed the task list: %d todos, want %d", got, todosBefore)
	}
	var first map[string]any
	if isError || json.Unmarshal([]byte(content), &first) != nil || first["status"] != "recorded" || first["duplicate"] != false ||
		!strings.HasPrefix(first["proposal_id"].(string), "tap_") {
		t.Fatalf("propose = %s (error %v)", content, isError)
	}
	events := f.proposalEvents(t)
	if len(events) != 1 || events[0].TaskID != f.items["runtime-engineer"].ID || events[0].Actor != "runtime-engineer" {
		t.Fatalf("proposal events = %#v", events)
	}
	var payload TeamActionProposedPayload
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	entry, _ := f.c.session.ActionCatalog.Lookup("collect-debug-bundle")
	if payload.Status != "proposed" || payload.EntryHash != entry.Hash || payload.CatalogHash != f.c.session.ActionCatalog.Hash ||
		string(payload.Arguments) != `{"service":"api"}` || payload.Attempt != 1 || payload.OccurrenceRevision < 1 {
		t.Fatalf("payload = %#v", payload)
	}

	content, isError = f.propose(t, "runtime-engineer", `{"arguments":{"service":"api"},"assessment":"recommended","rationale":"api latency spiked","action":"collect-debug-bundle"}`)
	var again map[string]any
	if isError || json.Unmarshal([]byte(content), &again) != nil || again["duplicate"] != true || again["proposal_id"] != first["proposal_id"] {
		t.Fatalf("repeated propose = %s", content)
	}
	if got := len(f.proposalEvents(t)); got != 1 {
		t.Fatalf("repeated propose appended %d events, want 1", got)
	}
	if content, isError = f.propose(t, "runtime-engineer", strings.Replace(validProposal, "api latency spiked", "a different reason", 1)); !isError || !strings.HasPrefix(content, teamActionProposalConflict) {
		t.Fatalf("conflicting propose = %s", content)
	}
}

func TestTeamActionProposeRejectsInvalidProposals(t *testing.T) {
	longEvidence := make([]string, maxProposalEvidenceRefs+1)
	for i := range longEvidence {
		longEvidence[i] = fmt.Sprintf("art-%d", i)
	}
	encodedEvidence, _ := json.Marshal(longEvidence)
	tests := []struct {
		name  string
		agent string
		input string
		want  string
	}{
		{name: "not a proposer", agent: "critic", input: validProposal, want: teamActionProposalForbidden},
		{name: "unknown action", agent: "runtime-engineer", input: strings.Replace(validProposal, "collect-debug-bundle", "missing", 1), want: teamActionProposalForbidden},
		{name: "arguments violate schema", agent: "runtime-engineer", input: strings.Replace(validProposal, `{"service":"api"}`, `{"service":7}`, 1), want: teamActionArgumentsInvalid},
		{name: "redaction-unstable arguments", agent: "runtime-engineer", input: strings.Replace(validProposal, `"api"`, `"api_key=sk-abcdefghijklmnopqrstuvwxyz0123456789"`, 1), want: teamActionArgumentsNotStable},
		{name: "unknown evidence", agent: "runtime-engineer", input: strings.Replace(validProposal, `"rationale"`, `"evidence_refs":["art-missing"],"rationale"`, 1), want: teamActionEvidenceInvalid},
		{name: "path evidence", agent: "runtime-engineer", input: strings.Replace(validProposal, `"rationale"`, `"evidence_refs":["reports/out.md"],"rationale"`, 1), want: teamActionEvidenceInvalid},
		{name: "too much evidence", agent: "runtime-engineer", input: strings.Replace(validProposal, `"rationale"`, `"evidence_refs":`+string(encodedEvidence)+`,"rationale"`, 1), want: teamActionEvidenceInvalid},
		{name: "empty rationale", agent: "runtime-engineer", input: strings.Replace(validProposal, "api latency spiked", "  ", 1), want: teamActionInvalidToolRequest},
		{name: "bad assessment", agent: "runtime-engineer", input: strings.Replace(validProposal, "recommended", "urgent", 1), want: teamActionInvalidToolRequest},
		{name: "unknown field", agent: "runtime-engineer", input: strings.Replace(validProposal, `"rationale"`, `"run_now":true,"rationale"`, 1), want: teamActionInvalidToolRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newProposalFixture(t)
			if tt.agent == "critic" {
				f.items["critic"] = f.c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "critic", Desc: "review"}})[0]
			}
			content, isError := f.propose(t, tt.agent, tt.input)
			if !isError || !strings.HasPrefix(content, tt.want) {
				t.Fatalf("propose = %q, want %s", content, tt.want)
			}
			if events := f.proposalEvents(t); len(events) != 0 {
				t.Fatalf("rejected proposal appended %d events", len(events))
			}
		})
	}
}

func TestTeamActionProposeEnforcesTheSessionLimit(t *testing.T) {
	f := newProposalFixture(t)
	f.c.actionProposals.mu.Lock()
	for i := range maxProposalsPerSession {
		f.c.actionProposals.addLocked(TeamActionProposal{IdempotencyKey: fmt.Sprintf("key-%d", i)})
	}
	f.c.actionProposals.mu.Unlock()
	if content, isError := f.propose(t, "runtime-engineer", validProposal); !isError || !strings.HasPrefix(content, teamActionProposalLimitExceeded) {
		t.Fatalf("propose over the limit = %q", content)
	}
}

func TestTeamActionProposeRequiresJournalAndOpenOccurrence(t *testing.T) {
	f := newProposalFixture(t)
	f.c.eventStore = nil
	f.c.SetEventJournal(nil)
	if content, isError := f.propose(t, "runtime-engineer", validProposal); !isError || !strings.HasPrefix(content, teamActionJournalRequired) {
		t.Fatalf("propose without a journal = %q", content)
	}

	f = newProposalFixture(t)
	item := f.items["runtime-engineer"]
	ctx := context.WithValue(context.WithValue(context.Background(), todoIDKey{}, item.ID), tools.AgentNameKey, "runtime-engineer")
	tool := &teamActionProposeTool{coordinator: f.c, todoID: item.ID, agent: "runtime-engineer"}
	response, err := tool.Run(ctx, fantasy.ToolCall{Name: teamActionProposeToolName, Input: validProposal})
	if err != nil || !response.IsError || !strings.HasPrefix(response.Content, teamActionCallerInvalid) {
		t.Fatalf("propose without an occurrence identity = %#v, err %v", response, err)
	}
}

func TestCoordinatorSeesProposalsNewestFirst(t *testing.T) {
	f := newProposalFixture(t)
	if _, isError := f.propose(t, "runtime-engineer", validProposal); isError {
		t.Fatal("first proposal failed")
	}
	if _, isError := f.propose(t, "network-engineer", strings.Replace(validProposal, "recommended", "defer", 1)); isError {
		t.Fatal("second proposal failed")
	}
	views := f.c.teamActionProposalViews("collect-debug-bundle")
	if len(views) != 2 || views[0].Agent != "network-engineer" || views[0].Assessment != "defer" || views[1].Agent != "runtime-engineer" {
		t.Fatalf("proposal views = %#v, want newest first", views)
	}
	counts := f.c.teamActionProposalCounts("collect-debug-bundle")
	if counts != (teamActionProposalCounts{Recommended: 1, Defer: 1}) {
		t.Fatalf("proposal counts = %#v", counts)
	}
	entry, _ := f.c.session.ActionCatalog.Lookup("collect-debug-bundle")
	if matched := f.c.matchingProposals(entry.ID, entry.Hash, runInputHash([]byte(`{"service":"api"}`))); len(matched) != 2 {
		t.Fatalf("matching proposals = %d, want 2", len(matched))
	}
}

func TestTeamActionProposalIndexRebuildsPerBranch(t *testing.T) {
	session := loadActionCatalogTeam(t, actionCatalogTestManifest("", actionCatalogTestEntry))
	session.Workspace = t.TempDir()
	start := func(fresh bool) *Coordinator {
		c := &Coordinator{session: session, taskTracker: NewTaskTracker(), executionRunID: "run-rebuild", sessionData: NewSession()}
		c.SetFreshSession(fresh)
		c.initEventStore()
		if c.eventStore == nil {
			t.Fatal("initEventStore did not open an event store")
		}
		return c
	}
	first := start(false)
	item := first.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "runtime-engineer", Desc: "diagnose"}})[0]
	f := proposalFixture{c: first, events: first.eventStore, items: map[string]*TodoItem{"runtime-engineer": item}}
	if _, isError := f.propose(t, "runtime-engineer", validProposal); isError {
		t.Fatal("proposal failed")
	}
	want := first.proposalsForAction("collect-debug-bundle")
	_ = first.eventStore.Close()

	resumed := start(false)
	if got := resumed.proposalsForAction("collect-debug-bundle"); !reflect.DeepEqual(got, want) {
		t.Fatalf("resumed index = %#v, want %#v", got, want)
	}
	_ = resumed.eventStore.Close()

	fresh := start(true)
	defer fresh.eventStore.Close()
	if got := fresh.proposalCount(); got != 0 {
		t.Fatalf("--new session sees %d proposals, want 0", got)
	}
}

func TestTeamActionProposedPayloadValidation(t *testing.T) {
	payload := TeamActionProposedPayload{
		SchemaVersion: 1, Status: "proposed", ProposalID: "tap_1", ActionID: "collect", Agent: "worker",
		Arguments: json.RawMessage(`{}`), Assessment: "recommended", Rationale: "why",
	}
	valid, _ := json.Marshal(payload)
	tests := []struct {
		name    string
		taskID  string
		payload string
		wantErr bool
	}{
		{name: "valid", taskID: "1", payload: string(valid)},
		{name: "missing task", payload: string(valid), wantErr: true},
		{name: "unknown field", taskID: "1", payload: strings.Replace(string(valid), `"status"`, `"extra":1,"status"`, 1), wantErr: true},
		{name: "trailing value", taskID: "1", payload: string(valid) + ` {}`, wantErr: true},
		{name: "bad assessment", taskID: "1", payload: strings.Replace(string(valid), "recommended", "urgent", 1), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTeamActionProposedPayload(RunEvent{Type: string(EventTeamActionProposed), TaskID: tt.taskID, Payload: json.RawMessage(tt.payload)})
			if (err != nil) != tt.wantErr {
				t.Fatalf("validate error = %v, want error %v", err, tt.wantErr)
			}
		})
	}
	if !IsKnownEventType(string(EventTeamActionProposed)) {
		t.Fatal("team_action_proposed is not a known event type")
	}
}

func TestTeamActionProposalFieldNamesAreNotRedacted(t *testing.T) {
	for _, value := range []any{TeamActionProposedPayload{}, TeamActionEvidenceRef{}} {
		typ := reflect.TypeOf(value)
		for i := range typ.NumField() {
			name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			if utils.IsRedactedJSONKey(name) {
				t.Errorf("%s.%s json name %q would be redacted", typ.Name(), typ.Field(i).Name, name)
			}
		}
	}
}

func TestTeamActionProposeIsNotAMutationOrCommitGated(t *testing.T) {
	if readOnlyToolMutation(teamActionProposeToolName, validProposal) {
		t.Fatal("team_action_propose is treated as a mutation")
	}
	if isReadOnlyToolCall(teamActionProposeToolName, validProposal) {
		t.Fatal("team_action_propose must not be a read-only call for retry and fallback")
	}
	c := disciplineCoordinator(t)
	task := TaskDef{ID: "t1", SideEffect: SideEffectInfraMutation, Recovery: RecoveryRetry}
	policy := disciplinePolicy(CommitGatePolicy{RequireReconcile: true}, StopPolicy{}, ReplanPolicy{})
	if err := c.armDiscipline(context.Background(), "todo-1", task, policy, &DecisionRecord{ID: "dec-1", EvidenceHash: "hash-1"}); err != nil {
		t.Fatal(err)
	}
	if denial := c.commitGateDenial(context.Background(), "todo-1", "bash", `{"command":"touch out.txt"}`); denial == "" {
		t.Fatal("fixture commit gate did not block a mutation")
	}
	if denial := c.commitGateDenial(context.Background(), "todo-1", teamActionProposeToolName, validProposal); denial != "" {
		t.Fatalf("commit gate blocked team_action_propose: %s", denial)
	}
}

func TestStaticExposureAddsProposeForProposers(t *testing.T) {
	session := loadActionCatalogTeam(t, actionCatalogTestManifest("", actionCatalogTestEntry))
	for agentName, want := range map[string]bool{"runtime-engineer": true, "critic": false} {
		names := staticTeamActionToolNames(session, session.Agents[agentName])
		if got := len(names) == 3 && names[2] == teamActionProposeToolName; got != want {
			t.Fatalf("%s catalog tools = %v, want propose %v", agentName, names, want)
		}
	}
}
