package team

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/runtimestore"
)

// A diagnostic packet is produced for every persisted failure: it carries
// the authoritative disposition plus non-authoritative repair candidates,
// is redacted before it reaches the session, and replays from the event
// store.

func diagnosticCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	c := newTestCoordinator(t, withTeamConfig(func(cfg *agent.TeamConfig) { cfg.Name = "diagnostic-team" }))
	c.executionRunID = "run-1"
	c.sessionData = NewSession()
	return c
}

func recordedDiagnosticPackets(c *Coordinator) []DiagnosticPacket {
	c.diagnosticPacketsMu.RLock()
	defer c.diagnosticPacketsMu.RUnlock()
	return append([]DiagnosticPacket(nil), c.diagnosticPackets...)
}

func pendingDiagnosticPackets(c *Coordinator) int {
	c.diagnosticPacketsMu.RLock()
	defer c.diagnosticPacketsMu.RUnlock()
	return len(c.pendingDiagnosticPackets)
}

func TestDiagnosisPolicyDecisionTable(t *testing.T) {
	retry := func(class TaskFailureClass, edit func(*RecoveryDecisionInput)) RecoveryDecisionInput {
		in := RecoveryDecisionInput{FailureClass: class, RecoveryPolicy: RecoveryRetry, Attempt: 1, MaxRetries: 3, EvidenceComplete: true, Replayable: true}
		if edit != nil {
			edit(&in)
		}
		return in
	}
	tests := []struct {
		name       string
		input      DiagnosisInput
		wantAction RepairAction
		wantDisp   RetryDisposition
	}{
		{
			name:       "execution retry",
			input:      DiagnosisInput{TaskID: "t1", FailureClass: FailureExecution, RecoveryDecisionInput: retry(FailureExecution, nil)},
			wantAction: RepairActionRetry, wantDisp: RetryWorker,
		},
		{
			name: "external timeout reconciles",
			input: DiagnosisInput{TaskID: "t2", FailureClass: FailureTimeout, SideEffect: SideEffectExternalWrite,
				RecoveryDecisionInput: retry(FailureTimeout, func(in *RecoveryDecisionInput) { in.SideEffect = SideEffectExternalWrite })},
			wantAction: RepairActionReconcile, wantDisp: ReconcileOnly,
		},
		{
			name:       "permission blocks",
			input:      DiagnosisInput{TaskID: "t3", Detail: "permission denied while writing", FailureClass: FailureExecution, RecoveryDecisionInput: retry(FailureExecution, nil)},
			wantAction: RepairActionBlock, wantDisp: NeedsHuman,
		},
		{
			name: "repeated failure replans",
			input: DiagnosisInput{TaskID: "t4", FailureClass: FailureExecution,
				RecoveryDecisionInput: retry(FailureExecution, func(in *RecoveryDecisionInput) { in.FailureFingerprint, in.PreviousFingerprint = "same", "same" })},
			wantAction: RepairActionReplan, wantDisp: ReplanRequired,
		},
		{
			name:       "protocol is reconcile only",
			input:      DiagnosisInput{TaskID: "t5", FailureClass: FailureProtocol, RecoveryDecisionInput: retry(FailureProtocol, nil)},
			wantAction: RepairActionReconcile, wantDisp: ReconcileOnly,
		},
		{
			name:       "missing tool requires replan",
			input:      DiagnosisInput{TaskID: "t7", FailureClass: FailureEnvironment, Detail: "executable not found: missing-tool", RecoveryDecisionInput: retry(FailureEnvironment, nil)},
			wantAction: RepairActionReplan, wantDisp: ReplanRequired,
		},
		{
			name: "external write crash reconciles",
			input: DiagnosisInput{TaskID: "t8", FailureClass: FailureExecution, SideEffect: SideEffectExternalWrite,
				RecoveryDecisionInput: retry(FailureExecution, func(in *RecoveryDecisionInput) { in.SideEffect, in.Replayable = SideEffectExternalWrite, false })},
			wantAction: RepairActionReconcile, wantDisp: ReconcileOnly,
		},
		{
			name: "cancelled never retries",
			input: DiagnosisInput{TaskID: "t6", FailureClass: FailureCancelled,
				RecoveryDecisionInput: retry(FailureCancelled, func(in *RecoveryDecisionInput) { in.ContextCancelled = true })},
			wantAction: RepairActionBlock, wantDisp: RetryNone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			packet, err := (DiagnosisPolicy{}).Diagnose(tt.input)
			if err != nil {
				t.Fatal(err)
			}
			if packet.FailureClass != tt.input.FailureClass || packet.Disposition != tt.wantDisp {
				t.Fatalf("packet class/disposition = %s/%s, want %s/%s", packet.FailureClass, packet.Disposition, tt.input.FailureClass, tt.wantDisp)
			}
			if len(packet.Hypotheses) != 1 || packet.Hypotheses[0].ProposedAction != tt.wantAction {
				t.Fatalf("hypotheses = %#v, want action %s", packet.Hypotheses, tt.wantAction)
			}
			if packet.Confidence <= 0 || packet.ID == "" || packet.CreatedAt.IsZero() {
				t.Fatalf("incomplete diagnostic packet: %#v", packet)
			}
		})
	}
}

func TestDiagnosticPacketRedactsEveryEvidenceField(t *testing.T) {
	const secret = "Authorization: Bearer nested-secret-123"
	// keyValueSecret only matches the key=value redactor, so the detail line
	// exercises that path on its own as well as the bearer-token path.
	const keyValueSecret = "supersecret-value"
	packet, err := (DiagnosisPolicy{}).Diagnose(DiagnosisInput{
		TaskID: "nested-task", RunID: "run-1", Attempt: 2, FailureClass: FailureVerify,
		Detail:       "verify failed with api_key=" + keyValueSecret + " " + secret,
		EvidenceRefs: []EvidenceRef{{Type: "tool", Description: secret, Value: secret}},
		VerifyResult: &VerificationResult{
			Command: secret, WorkDir: secret, Stdout: secret, Stderr: secret,
			WeakReason: secret, OverturnReason: secret,
			Spec: &VerificationSpec{Command: secret, Path: secret, Assertions: []JSONAssertion{{Path: secret, Equals: secret}}},
		},
		Capabilities: []CapabilityFinding{{Name: "capability", Scope: secret, Reason: secret, Evidence: secret}},
		LocalHints:   []string{secret}, SidecarReflection: secret,
		RecoveryDecisionInput: RecoveryDecisionInput{FailureClass: FailureVerify, RecoveryPolicy: RecoveryRetry, Attempt: 2, MaxRetries: 4, EvidenceComplete: true, Replayable: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "nested-secret-123") || strings.Contains(string(data), keyValueSecret) {
		t.Fatalf("diagnostic packet leaked evidence: %s", data)
	}
	if packet.Attempt != 2 || packet.RunID != "run-1" || packet.TaskID != "nested-task" {
		t.Fatalf("packet identity = %#v", packet)
	}
	if len(packet.Hypotheses) != 3 || packet.Hypotheses[1].Authoritative || packet.Hypotheses[2].Authoritative {
		t.Fatalf("candidate hypotheses were not labeled non-authoritative: %#v", packet.Hypotheses)
	}
}

func TestDiagnosticDispositionMergeIsMonotonic(t *testing.T) {
	ordered := []RetryDisposition{RetryWorker, RetryNone, ReplanRequired, ReconcileOnly, NeedsHuman}
	for _, applied := range ordered {
		for _, diagnosed := range ordered {
			got := mergeDiagnosticDisposition(applied, diagnosed)
			want := applied
			if dispositionSafetyRank(diagnosed) >= dispositionSafetyRank(applied) {
				want = diagnosed
			}
			if got != want {
				t.Fatalf("merge(%s, %s) = %s, want %s", applied, diagnosed, got, want)
			}
		}
	}
}

func TestPersistedFailureRecordsDiagnosticPacket(t *testing.T) {
	tests := []struct {
		name   string
		detail string
		hint   string
		check  func(t *testing.T, item *TodoItem, packet DiagnosticPacket)
	}{
		{
			name:   "permission diagnosis blocks the persisted task",
			detail: "permission denied while writing",
			check: func(t *testing.T, item *TodoItem, packet DiagnosticPacket) {
				if item.Status != TaskBlocked || packet.Disposition != NeedsHuman {
					t.Fatalf("status = %s, disposition = %s, want blocked/needs_human", item.Status, packet.Disposition)
				}
			},
		},
		{
			name:   "stored reflection becomes a non-authoritative candidate",
			detail: "execution failed",
			hint:   "sidecar candidate: use the project-local executable",
			check: func(t *testing.T, _ *TodoItem, packet DiagnosticPacket) {
				if len(packet.Hypotheses) < 2 {
					t.Fatalf("packet did not include the reflection candidate: %#v", packet)
				}
				candidate := packet.Hypotheses[1]
				if candidate.Authoritative || candidate.Source != "sidecar-reflection" || !strings.Contains(candidate.Cause, "project-local executable") {
					t.Fatalf("reflection candidate = %#v", candidate)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := diagnosticCoordinator(t)
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: tt.name, MaxRetries: 2}})[0]
			if tt.hint != "" {
				if err := c.taskTracker.TodoList().AppendDiagnosticHint(item.ID, tt.hint); err != nil {
					t.Fatal(err)
				}
			}
			c.PersistFailureWithClass(item.Agent, item.Desc, item.ID, tt.detail, RetryWorker, FailureExecution)
			packets := recordedDiagnosticPackets(c)
			if len(packets) != 1 {
				t.Fatalf("diagnostic packets = %#v, want one", packets)
			}
			tt.check(t, item, packets[0])
		})
	}
}

func TestSessionRestoreRedactsDiagnosticPacketsBeforeCheckpoint(t *testing.T) {
	secret := "Authorization: Bearer legacy-session-secret"
	legacy := DiagnosticPacket{
		ID: "legacy-diagnostic", TaskID: "task-legacy", RunID: "run-legacy", FailureClass: FailureVerify,
		EvidenceRefs:      []EvidenceRef{{Type: "verify", Description: secret, Value: secret}},
		VerifyResult:      &VerificationResult{Command: secret, Stdout: secret, Stderr: secret, Spec: &VerificationSpec{Command: secret, Path: secret}},
		CapabilityFinding: []CapabilityFinding{{Name: "capability", Reason: secret, Evidence: secret}},
		Hypotheses:        []RepairHypothesis{{ID: "h1", Cause: secret, ExpectedSignal: secret}},
	}
	sd := NewSession()
	sd.DiagnosticPackets = []DiagnosticPacket{legacy}
	c := newTestCoordinator(t, withTeamConfig(func(cfg *agent.TeamConfig) { cfg.Name = "restore-team" }))
	c.SetSessionData(sd)
	if len(sd.DiagnosticPackets) != 1 {
		t.Fatalf("shared session packet count = %d", len(sd.DiagnosticPackets))
	}
	shared, err := json.Marshal(sd.DiagnosticPackets[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(shared), "legacy-session-secret") {
		t.Fatalf("shared SessionData retained secret: %s", shared)
	}
	c.saveCheckpoint()
	disk, _ := getRuntimeDocument(t, c.session.Workspace, runtimestore.DocumentSession)
	if strings.Contains(string(disk), "legacy-session-secret") {
		t.Fatalf("checkpointed session retained secret: %s", disk)
	}
}

func TestDiagnosticPacketEventReplaysAndRetriesAtCheckpoint(t *testing.T) {
	persist := func(c *Coordinator) *TodoItem {
		item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "will fail", MaxRetries: 2}})[0]
		c.PersistFailureWithClass(item.Agent, item.Desc, item.ID, "source=error | error=command failed", RetryWorker, FailureExecution)
		return item
	}
	diagnosticEvents := func(t *testing.T, store *EventStore) ([]RunEvent, int) {
		t.Helper()
		events, err := store.ReadEvents()
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, event := range events {
			if event.Type == "diagnostic_packet" {
				count++
			}
		}
		return events, count
	}

	t.Run("written with the failure and restored by replay", func(t *testing.T) {
		c := diagnosticCoordinator(t)
		store, err := NewEventStore(c.session.Workspace, "run-1", "session-1")
		if err != nil {
			t.Fatal(err)
		}
		c.eventStore = store // closed by the coordinator's cleanup
		item := persist(c)
		packets := recordedDiagnosticPackets(c)
		if len(packets) != 1 || packets[0].TaskID != item.ID || packets[0].Disposition != ReplanRequired || packets[0].FailureClass != FailureExecution {
			t.Fatalf("recorded packets = %#v", packets)
		}
		events, count := diagnosticEvents(t, store)
		if count != 1 {
			t.Fatalf("diagnostic_packet events = %d, want 1", count)
		}
		projected := ReduceToSessionData(events)
		if len(projected.DiagnosticPackets) != 1 || projected.DiagnosticPackets[0].ID != packets[0].ID {
			t.Fatalf("replayed diagnostic packets = %#v", projected.DiagnosticPackets)
		}
	})

	t.Run("append without a store is retried at the next checkpoint", func(t *testing.T) {
		c := diagnosticCoordinator(t)
		persist(c)
		if got := pendingDiagnosticPackets(c); got != 1 {
			t.Fatalf("pending diagnostic packets = %d, want 1", got)
		}
		store, err := NewEventStore(c.session.Workspace, "run-1", "session-1")
		if err != nil {
			t.Fatal(err)
		}
		c.eventStore = store // closed by the coordinator's cleanup
		c.saveCheckpoint()
		if _, count := diagnosticEvents(t, store); count != 1 {
			t.Fatalf("diagnostic_packet events after checkpoint = %d, want 1", count)
		}
		if got := pendingDiagnosticPackets(c); got != 0 {
			t.Fatalf("pending diagnostic packets after retry = %d, want 0", got)
		}
	})
}
