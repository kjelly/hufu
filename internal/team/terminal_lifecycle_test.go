package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestEventStoreAppendPersistedContextCancelsWhileBlocked(t *testing.T) {
	store, err := NewEventStore(t.TempDir(), "run-blocked", "session-blocked")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	store.lock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = store.AppendPersistedContext(ctx, RunEvent{Type: "run_started", Actor: "test", Payload: []byte(`{"ok":true}`)})
	store.release()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked append error = %v, want context deadline", err)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("blocked append wrote %d events", len(events))
	}
}

func TestTerminalLifecycleNormalEmergencyRacePublishesOneCanonicalEvent(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-race", "session-race")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c := &Coordinator{
		eventStore:             store,
		executionRunID:         "run-race",
		terminalLifecycleRunID: "run-race",
		session:                &TeamSession{Workspace: workspace},
		sessionData:            NewSession(),
	}
	c.terminalLifecycleDone = make(chan struct{})
	c.terminalLifecyclePrepareDone = make(chan struct{})
	normal := &RunResult{RunID: "run-race", Outcome: RunOutcomeFailed, StopReason: StopReasonRunFailed, ExitCode: 1}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		candidate, elected := c.electTerminalCandidate(normal)
		if elected {
			c.prepareTerminalResult(candidate)
			c.finishTerminalPreparation()
		}
		_, _ = c.commitTerminalLifecycle(context.Background(), candidate)
	}()
	go func() {
		defer wg.Done()
		_ = c.EmergencyFinalizeRun(context.Background())
	}()
	wg.Wait()

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
	if c.LastRunResult() != c.terminalCandidate() {
		t.Fatal("LastRunResult is not the elected business pointer")
	}
	if c.LastRunResult() == nil || c.LastRunResult().RunID != "run-race" {
		t.Fatalf("canonical result = %#v", c.LastRunResult())
	}
}

func TestActiveSetLastRunResultDoesNotElectOrPersistTerminalState(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		executionRunID:         "run-projection-only",
		terminalLifecycleRunID: "run-projection-only",
		terminalLifecycleState: terminalLifecycleOpen,
		session:                &TeamSession{Workspace: workspace},
		sessionData:            NewSession(),
	}
	result := &RunResult{RunID: "run-projection-only", Outcome: RunOutcomePartial}
	c.SetLastRunResult(result)

	c.terminalLifecycleMu.Lock()
	state := c.terminalLifecycleState
	candidate := c.terminalLifecycleCandidate
	c.terminalLifecycleMu.Unlock()
	if state != terminalLifecycleOpen || candidate != nil {
		t.Fatalf("SetLastRunResult changed active terminal state: state=%d candidate=%#v", state, candidate)
	}
	if c.LastRunResult() != result {
		t.Fatal("projection API did not retain the in-memory result")
	}
	if c.sessionData.RunResult != nil {
		t.Fatal("unconfirmed active result was persisted to session data")
	}
}

func TestTerminalAppendFailureLeavesDownstreamProjectionsUnchanged(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-append-failure", "session-append-failure")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.closed = true
	c := &Coordinator{
		eventStore:             store,
		executionRunID:         "run-append-failure",
		terminalLifecycleRunID: "run-append-failure",
		terminalLifecycleState: terminalLifecycleOpen,
		session:                &TeamSession{Workspace: workspace},
		sessionData:            NewSession(),
		taskTracker:            NewTaskTracker(),
	}
	result := c.FinalizeRun(context.Background(), &RunResult{RunID: "run-append-failure", Outcome: RunOutcomeFailed}, nil)
	if result == nil || c.TerminalLifecycleConfirmed() {
		t.Fatalf("append failure was treated as confirmed: result=%#v confirmed=%v", result, c.TerminalLifecycleConfirmed())
	}
	if c.sessionData.RunResult != nil {
		t.Fatal("session result advanced after run_finished append failure")
	}
	if _, err := os.Stat(filepath.Join(workspace, statusDir)); !os.IsNotExist(err) {
		t.Fatalf("status projection advanced after append failure: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "runtime", "current-workset.json")); !os.IsNotExist(err) {
		t.Fatalf("workset projection advanced after append failure: err=%v", err)
	}
}

func TestTerminalSnapshotUsesOneRunAndEvidenceIdentity(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-identity", "session-identity")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c := &Coordinator{
		eventStore:             store,
		executionRunID:         "run-identity",
		terminalLifecycleRunID: "run-identity",
		terminalLifecycleState: terminalLifecycleOpen,
		session:                &TeamSession{Workspace: workspace},
		sessionData:            NewSession(),
		taskTracker:            NewTaskTracker(),
	}
	result := c.FinalizeRun(context.Background(), &RunResult{RunID: "run-identity", Outcome: RunOutcomeFailed}, nil)
	if result == nil || !c.TerminalLifecycleConfirmed() || result.EvidenceManifest == nil {
		t.Fatalf("terminal snapshot incomplete: result=%#v confirmed=%v", result, c.TerminalLifecycleConfirmed())
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != "run_finished" {
			continue
		}
		var payload LifecycleEventPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.RunID != result.RunID || payload.EvidenceManifest == nil || payload.EvidenceManifest.ManifestHash != result.EvidenceManifest.ManifestHash {
			t.Fatalf("terminal identity mismatch: event=%#v result=%#v", payload, result)
		}
		return
	}
	t.Fatal("run_finished event missing")
}

func TestFinalizeRunDoesNotSynthesizeNoProgressTerminalResult(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-no-progress", "session-no-progress")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c := &Coordinator{eventStore: store, executionRunID: "run-no-progress", terminalLifecycleRunID: "run-no-progress"}
	if got := c.FinalizeRun(context.Background(), nil, nil); got != nil {
		t.Fatalf("nil finalization returned %#v", got)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == "run_finished" {
			t.Fatal("nil/no-progress finalization synthesized run_finished")
		}
	}
}

func TestRuntimeWorksetManifestRequiresExplicitRoleAndStableIdentity(t *testing.T) {
	manifest := ArtifactRef{Path: "runtime/runs/r1/actions/a/workset/manifest.json"}
	falsePositive := ArtifactRef{Path: "workset.json", Kind: "output"}
	if isRuntimeWorksetManifest(falsePositive, manifest) {
		t.Fatal("path substring was accepted as a workset manifest")
	}
	declared := ArtifactRef{Kind: "workset_manifest", ID: "declared", SHA256: "sha"}
	produced := ArtifactRef{Kind: "workset_manifest", ID: "artifact", SHA256: "sha"}
	if !isRuntimeWorksetManifest(produced, declared) {
		t.Fatal("explicit workset_manifest kind was not accepted")
	}
	if produced.ID == "" || produced.SHA256 == "" {
		t.Fatal("workset manifest lacks stable identity")
	}
}

func TestLoadRuntimeWorksetProjectionRejectsWrongRunOrDigest(t *testing.T) {
	workspace := t.TempDir()
	runID := "run-current"
	manifestPath := filepath.Join(workspace, "runtime", "runs", runID, "actions", "action-1", "workset", "manifest.json")
	manifest := []byte(`{"schema_version":1,"items":[{"key":"one"}]}`)
	if err := AtomicWriteFile(manifestPath, manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(manifest)
	pointer := RuntimeWorksetPointer{SchemaVersion: 1, RunID: runID, ActionInvocationID: "action-1", ManifestArtifactID: "artifact-1", ManifestSHA256: hex.EncodeToString(digest[:]), ManifestPath: "runtime/runs/run-current/actions/action-1/workset/manifest.json"}
	projection := RuntimeWorksetProjection{SchemaVersion: 1, RunID: runID, Pointers: []RuntimeWorksetPointer{pointer}}
	encoded, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteFile(filepath.Join(workspace, "runtime", "current-workset.json"), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuntimeWorksetProjection(workspace, runID); err != nil {
		t.Fatalf("valid projection rejected: %v", err)
	}
	if _, err := LoadRuntimeWorksetProjection(workspace, "run-other"); err == nil {
		t.Fatal("projection from another run was accepted")
	}
	if err := os.WriteFile(manifestPath, []byte(`{"schema_version":1,"items":[{"key":"changed"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuntimeWorksetProjection(workspace, runID); err == nil {
		t.Fatal("digest-mismatched projection was accepted")
	}
}
