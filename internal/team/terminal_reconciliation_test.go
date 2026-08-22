package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPendingTerminalCommitReconcilesDelayedAppendExactlyOnce(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-delayed", "session-delayed")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	entered := make(chan struct{})
	release := make(chan struct{})
	store.syncFile = func() error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return nil
	}
	c := &Coordinator{
		eventStore:             store,
		executionRunID:         "run-delayed",
		terminalLifecycleRunID: "run-delayed",
		terminalLifecycleState: terminalLifecycleOpen,
		session:                &TeamSession{Workspace: workspace},
		sessionData:            NewSession(),
		taskTracker:            NewTaskTracker(),
	}
	candidate := &RunResult{RunID: "run-delayed", Outcome: RunOutcomeFailed}
	candidate, _ = c.electTerminalCandidate(candidate)
	c.prepareTerminalResult(candidate)
	c.finishTerminalPreparation()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_, err = c.commitTerminalLifecycle(ctx, candidate)
	cancel()
	if err == nil || !strings.Contains(err.Error(), "durability unknown") {
		t.Fatalf("commit error = %v, want bounded durability error", err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("append did not reach the delayed sync")
	}
	saved := LoadSession(workspace)
	if saved == nil || !saved.RecoveryRequired || saved.PendingTerminalCommit == nil {
		t.Fatalf("pending recovery checkpoint = %#v", saved)
	}
	if saved.PendingTerminalCommit.RunID != "run-delayed" || saved.PendingTerminalCommit.IdempotencyKey != terminalFinishedIdempotencyKey("run-delayed") || saved.PendingTerminalCommit.BranchID != "main" {
		t.Fatalf("pending identity = %#v", saved.PendingTerminalCommit)
	}

	close(release)
	if _, err := store.ReadEvents(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restarted := &Coordinator{
		session:     &TeamSession{Workspace: workspace},
		sessionData: saved,
		taskTracker: NewTaskTracker(),
	}
	restarted.SetSessionData(saved)
	restarted.initEventStore()
	if restarted.sessionData.RecoveryRequired || restarted.sessionData.PendingTerminalCommit != nil {
		t.Fatalf("reconciled session = %#v", restarted.sessionData)
	}
	if restarted.sessionData.RunResult == nil || restarted.sessionData.RunResult.RunID != "run-delayed" {
		t.Fatalf("reconciled run result = %#v", restarted.sessionData.RunResult)
	}
	if err := restarted.checkRunAdmission(); err != nil {
		t.Fatalf("reconciled session remained inadmissible: %v", err)
	}
	events, err := restarted.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	finished := 0
	for _, event := range events {
		if event.Type == "run_finished" {
			finished++
		}
	}
	if finished != 1 {
		t.Fatalf("run_finished count = %d, want exactly one", finished)
	}
}

func TestEmergencyWaiterTimeoutDoesNotLeaveRecoveryAfterNormalCommit(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-emergency-waiter", "session-emergency-waiter")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	entered := make(chan struct{})
	release := make(chan struct{})
	store.syncFile = func() error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return nil
	}
	c := &Coordinator{
		eventStore:             store,
		executionRunID:         "run-emergency-waiter",
		terminalLifecycleRunID: "run-emergency-waiter",
		terminalLifecycleState: terminalLifecycleOpen,
		session:                &TeamSession{Workspace: workspace},
		sessionData:            NewSession(),
		taskTracker:            NewTaskTracker(),
	}
	candidate := &RunResult{RunID: "run-emergency-waiter", Outcome: RunOutcomeFailed}
	candidate, elected := c.electTerminalCandidate(candidate)
	if !elected {
		t.Fatal("normal finalizer did not elect the terminal candidate")
	}
	c.prepareTerminalResult(candidate)
	c.finishTerminalPreparation()

	normalDone := make(chan error, 1)
	go func() {
		_, normalErr := c.commitTerminalLifecycle(context.Background(), candidate)
		normalDone <- normalErr
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("normal append did not reach delayed sync")
	}

	emergencyCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	emergencyErr := c.EmergencyFinalizeRun(emergencyCtx)
	cancel()
	if emergencyErr == nil || !strings.Contains(emergencyErr.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("emergency waiter error = %v, want context deadline", emergencyErr)
	}
	saved := LoadSession(workspace)
	if saved == nil || !saved.RecoveryRequired || saved.PendingTerminalCommit == nil {
		t.Fatalf("emergency timeout checkpoint = %#v", saved)
	}

	close(release)
	select {
	case normalErr := <-normalDone:
		if normalErr != nil {
			t.Fatalf("normal canonical commit failed: %v", normalErr)
		}
	case <-time.After(time.Second):
		t.Fatal("normal canonical commit did not complete after release")
	}

	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	finished := 0
	for _, event := range events {
		if event.Type == "run_finished" {
			finished++
		}
	}
	if finished != 1 {
		t.Fatalf("run_finished count = %d, want exactly one", finished)
	}
	saved = LoadSession(workspace)
	if saved == nil || saved.RecoveryRequired || saved.PendingTerminalCommit != nil {
		t.Fatalf("post-commit recovery state = %#v", saved)
	}
	if err := c.checkRunAdmission(); err != nil {
		t.Fatalf("next admission remained blocked: %v", err)
	}
}

func TestPendingTerminalCommitWrongIdentityRemainsRecoveryRequired(t *testing.T) {
	for _, test := range []struct {
		name        string
		eventKey    string
		eventBranch string
	}{
		{name: "wrong key", eventKey: "run_finished:other", eventBranch: "main"},
		{name: "wrong branch", eventKey: terminalFinishedIdempotencyKey("run-invalid"), eventBranch: "feature"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			store, err := NewEventStore(workspace, "run-invalid", "session-invalid")
			if err != nil {
				t.Fatal(err)
			}
			payload, _ := json.Marshal(map[string]any{"run_id": "run-invalid", "outcome": RunOutcomeFailed})
			if _, err := store.AppendPersisted(RunEvent{
				Type: "run_finished", RunID: "run-invalid", BranchID: test.eventBranch,
				Actor: "coordinator", IdempotencyKey: test.eventKey, Payload: payload,
			}); err != nil {
				t.Fatal(err)
			}
			_ = store.Close()
			session := NewSession()
			session.RecoveryRequired = true
			session.RecoveryReason = "terminal persistence unconfirmed"
			session.PendingTerminalCommit = &PendingTerminalCommit{RunID: "run-invalid", IdempotencyKey: terminalFinishedIdempotencyKey("run-invalid"), BranchID: "main"}
			if err := SaveSession(workspace, session); err != nil {
				t.Fatal(err)
			}
			restarted := &Coordinator{session: &TeamSession{Workspace: workspace}, sessionData: LoadSession(workspace), taskTracker: NewTaskTracker()}
			restarted.SetSessionData(restarted.sessionData)
			restarted.initEventStore()
			if !restarted.sessionData.RecoveryRequired || restarted.sessionData.PendingTerminalCommit == nil {
				t.Fatalf("invalid pending identity was cleared: %#v", restarted.sessionData)
			}
			if err := restarted.checkRunAdmission(); err == nil {
				t.Fatal("invalid pending identity was admitted")
			}
		})
	}
}

func TestRuntimeWorksetPointerRequiresCanonicalEvidenceMembership(t *testing.T) {
	workspace := t.TempDir()
	content := []byte(`{"schema_version":1,"items":[]}`)
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	path := filepath.Join(workspace, "runtime", "runs", "run-workset", "actions", "action-1", "workset", "manifest.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	ref := ArtifactRef{ID: "manifest-member", SHA256: sha}
	manifest := &EvidenceManifest{RunID: "run-workset", Status: "accepted", ArtifactRefs: []ArtifactRef{ref}}
	if err := manifest.Seal(); err != nil {
		t.Fatal(err)
	}
	result := &RunResult{RunID: "run-workset", EvidenceManifest: manifest}
	projection := RuntimeWorksetProjection{RunID: "run-workset", Pointers: []RuntimeWorksetPointer{{
		RunID: "run-workset", ManifestArtifactID: "outsider", ManifestSHA256: sha,
		ManifestPath: "runtime/runs/run-workset/actions/action-1/workset/manifest.json",
	}}}
	data, _ := json.Marshal(projection)
	if err := os.MkdirAll(filepath.Join(workspace, "runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "runtime", "current-workset.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuntimeWorksetProjection(workspace, result); err == nil || !strings.Contains(err.Error(), "outside canonical evidence manifest") {
		t.Fatalf("outsider pointer error = %v", err)
	}

	projection.Pointers[0].ManifestArtifactID = ref.ID
	data, _ = json.Marshal(projection)
	if err := os.WriteFile(filepath.Join(workspace, "runtime", "current-workset.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuntimeWorksetProjection(workspace, result); err != nil {
		t.Fatalf("exact evidence member rejected: %v", err)
	}
}

func TestCanonicalRunSnapshotUsesActiveBranchLineage(t *testing.T) {
	workspace := t.TempDir()
	tree := NewSessionTree()
	feature, err := tree.CreateRootBranch("feature")
	if err != nil {
		t.Fatal(err)
	}
	tree.ActiveBranch = feature.ID
	if err := SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	store, err := NewEventStore(workspace, "run-main", "session-branch")
	if err != nil {
		t.Fatal(err)
	}
	mainPayload, _ := json.Marshal(map[string]any{"run_id": "run-main", "outcome": RunOutcomeFailed})
	if _, err := store.AppendPersisted(RunEvent{Type: "run_finished", RunID: "run-main", BranchID: "main", Actor: "coordinator", Payload: mainPayload}); err != nil {
		t.Fatal(err)
	}
	featurePayload, _ := json.Marshal(map[string]any{"run_id": "run-feature", "outcome": RunOutcomePartial})
	if _, err := store.AppendPersisted(RunEvent{Type: "run_finished", RunID: "run-feature", BranchID: feature.ID, Actor: "coordinator", Payload: featurePayload}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := LoadCanonicalRunFinishedSnapshot(workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.RunID != "run-feature" {
		t.Fatalf("canonical branch result = %#v, want feature run", result)
	}
}

func TestCanonicalRunSnapshotUsesEnvelopeRunIDForHistoricalPayload(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-historical", "session-historical")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"outcome": RunOutcomeFailed})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(RunEvent{
		Type: "run_finished", RunID: "run-historical", BranchID: "main", Actor: "coordinator", Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := LoadCanonicalRunFinishedSnapshot(workspace, "run-historical")
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.RunID != "run-historical" {
		t.Fatalf("canonical historical result = %#v, want envelope run ID", result)
	}
}
