package team

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// sessionFieldClass records how a branch switch derives each SessionData
// field: replayed from the target lineage, carried from the live session
// (carryWorkspaceSessionState), or set when session.json is saved.
var sessionFieldClass = map[string]string{
	"CreatedAt": "replay", "UpdatedAt": "save", "Rounds": "carry",
	"ExecutionPolicySnapshot": "replay", "RunInputSnapshots": "replay",
	"DecisionPrimitiveResults": "replay", "ActiveRunInputSnapshotID": "replay",
	"RequiredResourceLockSet":         "replay",
	"ConversationHistorySourceOffset": "carry", "ConversationHistorySourceCounts": "carry",
	"ConversationHistorySourceRanges": "carry", "ConversationHistoryNextSourceIndex": "carry",
	"Entries": "replay", "Tasks": "replay", "WorksetReceipts": "replay", "WorksetStates": "replay",
	"DelegationPhase": "replay", "RunResult": "replay", "DiagnosticPackets": "replay",
	"PlanRevisions": "replay", "PlanReviews": "replay", "AcceptanceContractRevisions": "replay",
	"ContinuationCheckpoint": "replay", "CriterionResults": "replay", "CriterionCheckpoints": "replay",
	"LastCriterionProgressAt": "replay", "WorkflowState": "replay", "PhaseResults": "replay",
	"RuntimeWorkspace": "carry", "RetryState": "replay", "LearningGaps": "carry",
	"RecoveryRequired": "replay", "RecoveryReason": "replay",
	// Kept by a rebuild of its own branch; blocks a switch to any other.
	"PendingTerminalCommit": "own-branch",
	"PendingWrapUp":         "replay", "CoordinatorContextManifests": "replay",
	// Disposable projections that replay leaves empty.
	"DecisionProjections": "replay", "RequestContractProjections": "replay",
	"CompactionReferences": "replay", "CompactionCheckpointReferences": "replay",
}

// TestSessionDataFieldsAreClassifiedForBranchSwitch fails when a SessionData
// field is added without deciding how a branch switch derives it.
func TestSessionDataFieldsAreClassifiedForBranchSwitch(t *testing.T) {
	fields := reflect.TypeFor[SessionData]()
	for i := range fields.NumField() {
		name := fields.Field(i).Name
		if _, ok := sessionFieldClass[name]; !ok {
			t.Errorf("SessionData.%s is not classified for branch switches; add it to sessionFieldClass and, if it is workspace state, to carryWorkspaceSessionState", name)
		}
	}
	// Carried fields keep the live value even when the target lineage is
	// empty; every other field starts from replay.
	live := NewSession()
	live.Rounds = 3
	live.RuntimeWorkspace = "/runtime"
	live.LearningGaps = []LearningGap{{EventType: "memory_retrieved"}}
	live.ConversationHistorySourceOffset = 2
	live.ConversationHistorySourceCounts = []int{1}
	live.ConversationHistorySourceRanges = [][]CompactionRange{{{StartIndex: 0, EndIndex: 1, MsgCount: 1}}}
	live.ConversationHistoryNextSourceIndex = 4
	next := NewSession()
	carryWorkspaceSessionState(next, live)
	liveValue, nextValue := reflect.ValueOf(*live), reflect.ValueOf(*next)
	for i := range fields.NumField() {
		name := fields.Field(i).Name
		if sessionFieldClass[name] != "carry" {
			continue
		}
		if !reflect.DeepEqual(liveValue.Field(i).Interface(), nextValue.Field(i).Interface()) {
			t.Errorf("carried field %s = %v, want %v", name, nextValue.Field(i).Interface(), liveValue.Field(i).Interface())
		}
	}
}

func appendBranchEvent(t *testing.T, store *EventStore, event RunEvent) {
	t.Helper()
	if event.Actor == "" {
		event.Actor = "coordinator"
	}
	if _, err := store.AppendPersisted(event); err != nil {
		t.Fatalf("append %s: %v", event.Type, err)
	}
}

func marshalPayload(t *testing.T, payload any) []byte {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestCheckoutRestoresTheTargetBranchSession checks out branch A after
// branch B diverged with different run inputs, a pending wrap-up and a
// finished workflow. session.json must become A's replayed state, and the
// next start must match it, restore A's workflow, and accept A's work.
func TestCheckoutRestoresTheTargetBranchSession(t *testing.T) {
	session := workflowTestSession(t)
	workspace := session.Workspace
	store, err := NewEventStore(workspace, "run-1", "session-ab")
	if err != nil {
		t.Fatal(err)
	}
	tree := NewSessionTree()
	definitions := []RunInputDefinition{{Name: "count", Schema: RunInputSchema{Type: "integer"}, Required: true}}

	// Branch A (main), run 1: inputs, a task, a workflow in AUDIT, a
	// contract revision and a continuation checkpoint.
	inputsA, err := ResolveRunInputSnapshot(definitions, []RunInputAssignment{{Name: "count", RawValue: []byte("1"), Source: RunInputSourceCLI}}, "run-1", "invocation-1", session.Config.Name)
	if err != nil {
		t.Fatal(err)
	}
	appendBranchEvent(t, store, RunEvent{Type: string(EventRunStarted), RunID: "run-1", Payload: marshalPayload(t, LifecycleEventPayload{Team: session.Config.Name})})
	appendBranchEvent(t, store, RunEvent{Type: string(EventRunInputsResolved), RunID: "run-1", Payload: marshalPayload(t, inputsA)})
	appendBranchEvent(t, store, RunEvent{Type: string(EventTaskCreated), RunID: "run-1", TaskID: "1", Actor: "preparer", Payload: []byte(`{"id":"1","status":"done","agent":"preparer","contract_id":"prepare","phase":"PREPARE","desc":"prepare"}`)})
	appendBranchEvent(t, store, RunEvent{Type: string(EventWorkflowStateChanged), RunID: "run-1", Payload: marshalPayload(t, WorkflowStateChangedPayload{
		State: PhaseAudit, PhaseResults: map[Phase]PhaseResult{PhasePrepare: {Status: PhaseStatusSuccess, Summary: "prepared"}},
	})})
	appendBranchEvent(t, store, RunEvent{Type: "acceptance_contract_modified", RunID: "run-1", Payload: []byte(`{"revision":1,"new_spec":{"commands":["test -f a"]},"reason":"a"}`)})
	appendBranchEvent(t, store, RunEvent{Type: "coordinator_continuation_checkpoint", RunID: "run-1", Payload: []byte(`{"turn_count":2,"max_turns":10,"reason":"step_limit","status":"pending"}`)})

	// Branch B forks A's head and diverges in run 2.
	branchB, err := tree.CreateBranch("b", "", store)
	if err != nil {
		t.Fatal(err)
	}
	tree.ActiveBranch = branchB.ID
	store.SetBranchID(branchB.ID)
	inputsB, err := ResolveRunInputSnapshot(definitions, []RunInputAssignment{{Name: "count", RawValue: []byte("2"), Source: RunInputSourceCLI}}, "run-2", "invocation-2", session.Config.Name)
	if err != nil {
		t.Fatal(err)
	}
	appendBranchEvent(t, store, RunEvent{Type: string(EventRunStarted), RunID: "run-2", Payload: marshalPayload(t, LifecycleEventPayload{Team: session.Config.Name})})
	appendBranchEvent(t, store, RunEvent{Type: string(EventRunInputsResolved), RunID: "run-2", Payload: marshalPayload(t, inputsB)})
	appendBranchEvent(t, store, RunEvent{Type: string(EventWorkflowStateChanged), RunID: "run-2", Payload: marshalPayload(t, WorkflowStateChangedPayload{
		State: PhaseDone, PhaseResults: map[Phase]PhaseResult{
			PhasePrepare: {Status: PhaseStatusSuccess}, PhaseAudit: {Status: PhaseStatusSuccess},
			PhaseExecute: {Status: PhaseStatusSuccess}, PhaseVerify: {Status: PhaseStatusSuccess},
		},
	})})
	appendBranchEvent(t, store, RunEvent{Type: string(EventWrapUpPhase), RunID: "run-2", Payload: marshalPayload(t, PendingWrapUp{RunID: "run-2", BranchID: branchB.ID, RequestedAt: "2026-10-13T00:00:00Z", Reason: "operator"})})

	// session.json is B's live checkpoint.
	events := mustReadEvents(t, store)
	liveB := ReduceToSessionData(FilterEventsForBranch(events, tree, branchB.ID))
	if liveB.PendingWrapUp == nil || liveB.WorkflowState != PhaseDone || liveB.ActiveRunInputSnapshotID != inputsB.ID {
		t.Fatalf("branch B fixture did not replay its divergence: %+v", liveB)
	}
	liveB.Rounds = 5
	liveB.RuntimeWorkspace = filepath.Join(workspace, "runtime")
	if err := SaveSession(workspace, liveB); err != nil {
		t.Fatal(err)
	}

	// Check out A.
	if _, err := tree.CheckoutBranch("main", store); err != nil {
		t.Fatal(err)
	}
	if err := RebuildSessionForBranch(workspace, tree, store, "main"); err != nil {
		t.Fatalf("RebuildSessionForBranch: %v", err)
	}
	got := LoadSession(workspace)
	if got.PendingWrapUp != nil || got.WorkflowState != PhaseAudit || got.ActiveRunInputSnapshotID != inputsA.ID || len(got.RunInputSnapshots) != 1 {
		t.Fatalf("checked-out session kept branch B state: wrapUp=%+v workflow=%s inputs=%s (%d)", got.PendingWrapUp, got.WorkflowState, got.ActiveRunInputSnapshotID, len(got.RunInputSnapshots))
	}
	if len(got.AcceptanceContractRevisions) != 1 || got.ContinuationCheckpoint == nil || got.ContinuationCheckpoint.Status != "pending" || len(got.Tasks) != 1 {
		t.Fatalf("checked-out session lost branch A state: %+v", got)
	}
	if got.Rounds != 5 || got.RuntimeWorkspace != liveB.RuntimeWorkspace {
		t.Fatalf("workspace state was not carried: rounds=%d runtime=%q", got.Rounds, got.RuntimeWorkspace)
	}

	// The next start on A matches its replay and restores its workflow.
	store.SetBranchID("main")
	if err := SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	workflow, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: session, taskTracker: NewTaskTracker(), phaseWorkflow: workflow}
	c.SetSessionData(LoadSession(workspace))
	c.initEventStore()
	t.Cleanup(func() { _ = c.eventStore.Close() })
	if c.sessionData.RecoveryRequired {
		t.Fatalf("next start requires recovery: %s", c.sessionData.RecoveryReason)
	}
	var shadow []string
	for _, event := range mustReadEvents(t, c.eventStore) {
		if event.Type == "projection_mismatch" {
			t.Fatalf("next start reported a projection mismatch: %s", event.Payload)
		}
		if event.Type == "projection_rebuilt" {
			shadow = append(shadow, string(event.Payload))
		}
	}
	if len(shadow) != 1 || !strings.Contains(shadow[0], `"status":"matched"`) {
		t.Fatalf("projection shadow results = %v, want one match", shadow)
	}
	if got := workflow.State(); got != PhaseAudit {
		t.Fatalf("restored workflow state = %s, want AUDIT", got)
	}
	if err := workflow.validateTasks([]TaskDef{{Agent: "auditor", ContractID: "audit", Phase: PhaseAudit}}); err != nil {
		t.Fatalf("branch A's AUDIT work was rejected after checkout: %v", err)
	}
}

func TestBranchSwitchRefusesAnUnconfirmedTerminalCommit(t *testing.T) {
	workspace := t.TempDir()
	tree := NewSessionTree()
	branch, err := tree.CreateBranch("other", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	live := NewSession()
	live.AddEntry("user", "current branch")
	live.PendingTerminalCommit = &PendingTerminalCommit{RunID: "run-1", BranchID: "main", IdempotencyKey: "terminal:run-1"}
	if err := SaveSession(workspace, live); err != nil {
		t.Fatal(err)
	}

	err = RebuildSessionForBranch(workspace, tree, nil, branch.ID)
	if err == nil || !strings.Contains(err.Error(), "hufu resume") {
		t.Fatalf("rebuild error = %v, want a refusal naming hufu resume", err)
	}
	got := LoadSession(workspace)
	if len(got.Entries) != 1 || got.PendingTerminalCommit == nil {
		t.Fatalf("refused switch changed session.json: %+v", got)
	}

	// Rebuilding the branch that owns the commit keeps it for the next run
	// on that branch to confirm.
	if err := RebuildSessionForBranch(workspace, tree, nil, "main"); err != nil {
		t.Fatalf("rebuilding the commit's own branch: %v", err)
	}
	if got := LoadSession(workspace); got.PendingTerminalCommit == nil || got.PendingTerminalCommit.RunID != "run-1" {
		t.Fatalf("rebuild of the commit's branch dropped it: %+v", got.PendingTerminalCommit)
	}
}

// TestProjectionRepairReplacesStaleRunInputs pins that the shadow check's
// repair replaces the run inputs it compares: a repaired start must leave a
// projection the next start matches, not one it repairs again forever.
func TestProjectionRepairReplacesStaleRunInputs(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-1", "session-inputs")
	if err != nil {
		t.Fatal(err)
	}
	definitions := []RunInputDefinition{{Name: "count", Schema: RunInputSchema{Type: "integer"}, Required: true}}
	inputs, err := ResolveRunInputSnapshot(definitions, []RunInputAssignment{{Name: "count", RawValue: []byte("1"), Source: RunInputSourceCLI}}, "run-1", "invocation-1", "inputs-team")
	if err != nil {
		t.Fatal(err)
	}
	appendBranchEvent(t, store, RunEvent{Type: string(EventRunStarted), RunID: "run-1", Payload: marshalPayload(t, LifecycleEventPayload{Team: "inputs-team"})})
	appendBranchEvent(t, store, RunEvent{Type: string(EventRunInputsResolved), RunID: "run-1", Payload: marshalPayload(t, inputs)})
	appendBranchEvent(t, store, RunEvent{Type: string(EventUserMessageAdded), RunID: "run-1", Payload: marshalPayload(t, SessionMessageEventPayload{Role: "user", Content: "hello"})})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// session.json agrees on everything but the run inputs it never saved.
	stale := NewSession()
	stale.AddEntry("user", "hello")
	if err := SaveSession(workspace, stale); err != nil {
		t.Fatal(err)
	}

	start := func() string {
		t.Helper()
		c := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "inputs-team"}}, taskTracker: NewTaskTracker()}
		c.SetSessionData(LoadSession(workspace))
		c.initEventStore()
		defer func() { _ = c.eventStore.Close() }()
		if c.sessionData.RecoveryRequired {
			t.Fatalf("start requires recovery: %s", c.sessionData.RecoveryReason)
		}
		status := ""
		for _, event := range mustReadEvents(t, c.eventStore) {
			if event.Type == "projection_rebuilt" {
				var payload map[string]string
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				status = payload["status"]
			}
		}
		return status
	}
	if got := start(); got != "repaired" {
		t.Fatalf("first start projection status = %q, want repaired", got)
	}
	if got := start(); got != "matched" {
		t.Fatalf("second start projection status = %q, want matched after the repair", got)
	}
	if got := LoadSession(workspace); got.ActiveRunInputSnapshotID != inputs.ID || len(got.RunInputSnapshots) != 1 {
		t.Fatalf("repaired run inputs = %q (%d), want %q", got.ActiveRunInputSnapshotID, len(got.RunInputSnapshots), inputs.ID)
	}
}
