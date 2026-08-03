package team

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
)

func TestPhase1DiagnosisPolicyDecisionTable(t *testing.T) {
	tests := []struct {
		name       string
		input      DiagnosisInput
		wantClass  TaskFailureClass
		wantAction RepairAction
		wantDisp   RetryDisposition
	}{
		{
			name: "execution retry",
			input: DiagnosisInput{TaskID: "t1", FailureClass: FailureExecution, RecoveryDecisionInput: RecoveryDecisionInput{
				FailureClass: FailureExecution, RecoveryPolicy: RecoveryRetry, Attempt: 1, MaxRetries: 3,
				EvidenceComplete: true, Replayable: true,
			}},
			wantClass: FailureExecution, wantAction: RepairActionRetry, wantDisp: RetryWorker,
		},
		{
			name: "external timeout reconciles",
			input: DiagnosisInput{TaskID: "t2", FailureClass: FailureTimeout, SideEffect: SideEffectExternalWrite, RecoveryDecisionInput: RecoveryDecisionInput{
				FailureClass: FailureTimeout, SideEffect: SideEffectExternalWrite, RecoveryPolicy: RecoveryRetry, Attempt: 1, MaxRetries: 3,
				EvidenceComplete: true, Replayable: true,
			}},
			wantClass: FailureTimeout, wantAction: RepairActionReconcile, wantDisp: ReconcileOnly,
		},
		{
			name: "permission blocks",
			input: DiagnosisInput{TaskID: "t3", Detail: "permission denied while writing", FailureClass: FailureExecution, RecoveryDecisionInput: RecoveryDecisionInput{
				FailureClass: FailureExecution, RecoveryPolicy: RecoveryRetry, Attempt: 1, MaxRetries: 3,
				EvidenceComplete: true, Replayable: true,
			}},
			wantClass: FailureExecution, wantAction: RepairActionBlock, wantDisp: NeedsHuman,
		},
		{
			name: "repeated failure replans",
			input: DiagnosisInput{TaskID: "t4", FailureClass: FailureExecution, RecoveryDecisionInput: RecoveryDecisionInput{
				FailureClass: FailureExecution, RecoveryPolicy: RecoveryRetry, Attempt: 1, MaxRetries: 3,
				EvidenceComplete: true, Replayable: true, FailureFingerprint: "same", PreviousFingerprint: "same",
			}},
			wantClass: FailureExecution, wantAction: RepairActionReplan, wantDisp: ReplanRequired,
		},
		{
			name: "protocol is reconcile only",
			input: DiagnosisInput{TaskID: "t5", FailureClass: FailureProtocol, RecoveryDecisionInput: RecoveryDecisionInput{
				FailureClass: FailureProtocol, RecoveryPolicy: RecoveryRetry, Attempt: 1, MaxRetries: 3,
				EvidenceComplete: true, Replayable: true,
			}},
			wantClass: FailureProtocol, wantAction: RepairActionReconcile, wantDisp: ReconcileOnly,
		},
		{
			name: "missing tool requires replan",
			input: DiagnosisInput{TaskID: "t7", FailureClass: FailureEnvironment, Detail: "executable not found: missing-tool", RecoveryDecisionInput: RecoveryDecisionInput{
				FailureClass: FailureEnvironment, RecoveryPolicy: RecoveryRetry, Attempt: 1, MaxRetries: 3,
				EvidenceComplete: true, Replayable: true,
			}},
			wantClass: FailureEnvironment, wantAction: RepairActionReplan, wantDisp: ReplanRequired,
		},
		{
			name: "external write crash reconciles",
			input: DiagnosisInput{TaskID: "t8", FailureClass: FailureExecution, SideEffect: SideEffectExternalWrite, RecoveryDecisionInput: RecoveryDecisionInput{
				FailureClass: FailureExecution, SideEffect: SideEffectExternalWrite, RecoveryPolicy: RecoveryRetry, Attempt: 1, MaxRetries: 3,
				EvidenceComplete: true, Replayable: false,
			}},
			wantClass: FailureExecution, wantAction: RepairActionReconcile, wantDisp: ReconcileOnly,
		},
		{
			name: "cancelled never retries",
			input: DiagnosisInput{TaskID: "t6", FailureClass: FailureCancelled, RecoveryDecisionInput: RecoveryDecisionInput{
				FailureClass: FailureCancelled, RecoveryPolicy: RecoveryRetry, Attempt: 1, MaxRetries: 3,
				EvidenceComplete: true, Replayable: true, ContextCancelled: true,
			}},
			wantClass: FailureCancelled, wantAction: RepairActionBlock, wantDisp: RetryNone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			packet, err := (DiagnosisPolicy{}).Diagnose(tt.input)
			if err != nil {
				t.Fatal(err)
			}
			if packet.FailureClass != tt.wantClass || packet.Disposition != tt.wantDisp {
				t.Fatalf("packet class/disposition = %s/%s, want %s/%s", packet.FailureClass, packet.Disposition, tt.wantClass, tt.wantDisp)
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

func TestPhase1DiagnosticPacketIsRedactedAndReplayable(t *testing.T) {
	packet, err := (DiagnosisPolicy{}).Diagnose(DiagnosisInput{
		TaskID: "task-1", RunID: "run-1", Attempt: 2,
		Detail:                "worker failed with api_key=supersecret-value",
		FailureClass:          FailureExecution,
		RecoveryDecisionInput: RecoveryDecisionInput{FailureClass: FailureExecution, RecoveryPolicy: RecoveryRetry, Attempt: 2, MaxRetries: 4, EvidenceComplete: true, Replayable: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "supersecret-value") {
		t.Fatal("diagnostic packet leaked secret material")
	}
	if got := packet.Attempt; got != 2 || packet.RunID != "run-1" || packet.TaskID != "task-1" {
		t.Fatalf("packet identity = %#v", packet)
	}
}

func TestPhase1NestedDiagnosticEvidenceIsRedactedAndBounded(t *testing.T) {
	secret := "Authorization: Bearer nested-secret-123"
	packet, err := (DiagnosisPolicy{}).Diagnose(DiagnosisInput{
		TaskID: "nested-task", FailureClass: FailureVerify, Detail: "verify failed",
		EvidenceRefs: []EvidenceRef{{Type: "tool", Description: secret, Value: secret}},
		VerifyResult: &VerificationResult{
			Command: secret, WorkDir: secret, Stdout: secret, Stderr: secret,
			WeakReason: secret, OverturnReason: secret,
			Spec: &VerificationSpec{Command: secret, Path: secret, Assertions: []JSONAssertion{{Path: secret, Equals: secret}}},
		},
		Capabilities: []CapabilityFinding{{Name: "capability", Scope: secret, Reason: secret, Evidence: secret}},
		LocalHints:   []string{secret}, SidecarReflection: secret,
		RecoveryDecisionInput: RecoveryDecisionInput{FailureClass: FailureVerify, RecoveryPolicy: RecoveryRetry, Attempt: 1, MaxRetries: 2, EvidenceComplete: true, Replayable: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "nested-secret-123") {
		t.Fatalf("nested diagnostic evidence leaked: %s", data)
	}
	if len(packet.Hypotheses) != 3 || packet.Hypotheses[1].Authoritative || packet.Hypotheses[2].Authoritative {
		t.Fatalf("candidate hypotheses were not labeled non-authoritative: %#v", packet.Hypotheses)
	}
}

func TestPhase1DiagnosticDispositionMergeIsMonotonic(t *testing.T) {
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

func TestPhase1PermissionDiagnosisControlsPersistedTaskStatus(t *testing.T) {
	c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{Name: "diagnostic-team"}}, sessionData: NewSession(), taskTracker: NewTaskTracker(), executionRunID: "run-1"}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "permission failure", MaxRetries: 2}})[0]
	c.PersistFailureWithClass(item.Agent, item.Desc, item.ID, "permission denied while writing", RetryWorker, FailureExecution)
	if item.Status != TaskBlocked {
		t.Fatalf("permission failure status = %s, want blocked", item.Status)
	}
	c.diagnosticPacketsMu.RLock()
	defer c.diagnosticPacketsMu.RUnlock()
	if len(c.diagnosticPackets) != 1 || c.diagnosticPackets[0].Disposition != NeedsHuman {
		t.Fatalf("permission diagnostic packet = %#v, want needs_human", c.diagnosticPackets)
	}
}

func TestPhase1StoredReflectionReachesDiagnosticPacket(t *testing.T) {
	c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{Name: "diagnostic-team"}}, sessionData: NewSession(), taskTracker: NewTaskTracker(), executionRunID: "run-1"}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "reflection failure", MaxRetries: 2}})[0]
	if err := c.taskTracker.TodoList().AppendDiagnosticHint(item.ID, "sidecar candidate: use the project-local executable"); err != nil {
		t.Fatal(err)
	}
	c.PersistFailureWithClass(item.Agent, item.Desc, item.ID, "execution failed", RetryWorker, FailureExecution)
	c.diagnosticPacketsMu.RLock()
	defer c.diagnosticPacketsMu.RUnlock()
	if len(c.diagnosticPackets) != 1 || len(c.diagnosticPackets[0].Hypotheses) < 2 {
		t.Fatalf("diagnostic packet did not include reflection candidate: %#v", c.diagnosticPackets)
	}
	candidate := c.diagnosticPackets[0].Hypotheses[1]
	if candidate.Authoritative || candidate.Source != "sidecar-reflection" || !strings.Contains(candidate.Cause, "project-local executable") {
		t.Fatalf("reflection candidate = %#v", candidate)
	}
}

func TestPhase1SessionRestoreNormalizesSharedDataBeforeCheckpoint(t *testing.T) {
	dir := t.TempDir()
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
	c := &Coordinator{session: &TeamSession{Workspace: dir, Config: agent.TeamConfig{Name: "restore-team"}}, taskTracker: NewTaskTracker()}
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
	disk, err := os.ReadFile(filepath.Join(dir, sessionFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(disk), "legacy-session-secret") {
		t.Fatalf("checkpointed session retained secret: %s", disk)
	}
}

func TestPhase1FailureWritesDiagnosticPacketAndEventReplayRestoresIt(t *testing.T) {
	dir := t.TempDir()
	session := &TeamSession{Workspace: dir, Config: agent.TeamConfig{Name: "diagnostic-team"}}
	es, err := NewEventStore(dir, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	c := &Coordinator{session: session, sessionData: NewSession(), taskTracker: NewTaskTracker(), eventStore: es, executionRunID: "run-1"}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "will fail", MaxRetries: 2}})[0]
	c.PersistFailureWithClass(item.Agent, item.Desc, item.ID, "source=error | error=command failed", RetryWorker, FailureExecution)
	c.diagnosticPacketsMu.RLock()
	if len(c.diagnosticPackets) != 1 {
		c.diagnosticPacketsMu.RUnlock()
		t.Fatalf("diagnostic packet count = %d", len(c.diagnosticPackets))
	}
	original := c.diagnosticPackets[0]
	c.diagnosticPacketsMu.RUnlock()
	if original.TaskID != item.ID || original.Disposition != ReplanRequired || original.FailureClass != FailureExecution {
		t.Fatalf("original packet = %#v", original)
	}
	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, event := range events {
		if event.Type == "diagnostic_packet" {
			found = true
		}
	}
	if !found {
		t.Fatal("diagnostic_packet event was not written")
	}
	projected := ReduceToSessionData(events)
	if len(projected.DiagnosticPackets) != 1 || projected.DiagnosticPackets[0].ID != original.ID {
		t.Fatalf("replayed diagnostic packets = %#v", projected.DiagnosticPackets)
	}
}

func TestPhase1DiagnosticEventAppendFailureRetriesAtCheckpoint(t *testing.T) {
	dir := t.TempDir()
	session := &TeamSession{Workspace: dir, Config: agent.TeamConfig{Name: "diagnostic-team"}}
	c := &Coordinator{session: session, sessionData: NewSession(), taskTracker: NewTaskTracker(), executionRunID: "run-1"}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "will fail", MaxRetries: 2}})[0]
	c.PersistFailureWithClass(item.Agent, item.Desc, item.ID, "source=error | error=command failed", RetryWorker, FailureExecution)
	c.diagnosticPacketsMu.RLock()
	pending := len(c.pendingDiagnosticPackets)
	c.diagnosticPacketsMu.RUnlock()
	if pending != 1 {
		t.Fatalf("pending diagnostic packets = %d, want 1", pending)
	}
	es, err := NewEventStore(dir, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	c.eventStore = es
	defer es.Close()
	c.saveCheckpoint()
	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Type == "diagnostic_packet" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("diagnostic packet event count = %d, want 1", count)
	}
	c.diagnosticPacketsMu.RLock()
	pending = len(c.pendingDiagnosticPackets)
	c.diagnosticPacketsMu.RUnlock()
	if pending != 0 {
		t.Fatalf("pending diagnostic packets after retry = %d", pending)
	}
}
