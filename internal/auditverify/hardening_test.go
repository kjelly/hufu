package auditverify

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

// --- Tamper Case E (spec.md §52): splice a different run's events.jsonl in,
// with its own bundle.json file hash correctly recomputed, but bundle.json's
// declared run_finished identity left pointing at the original run. Every
// per-file hash checks out individually; only the cross-linked canonical
// proof (declared terminal event actually present in the log) breaks. ---

func TestVerifyBundleRejectsSplicedEventLogWithConsistentFileHash(t *testing.T) {
	bundleA := exportFixtureBundle(t)
	bundleB := exportFixtureBundle(t) // a second, independent fixture/run

	eventsB := readTarEntry(t, bundleB, "events.jsonl")
	tamperTarEntryAndRehash(t, bundleA, "events.jsonl", func([]byte) []byte { return eventsB })

	result, err := VerifyBundle(context.Background(), bundleA, VerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if result.Verdict != AuditVerdictFail {
		t.Fatalf("spliced event log verdict = %#v, want fail", result)
	}
}

func readTarEntry(t *testing.T, bundlePath, entryPath string) []byte {
	t.Helper()
	extractDir := t.TempDir()
	f, err := os.Open(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := extractTarArchive(f, extractDir); err != nil {
		t.Fatalf("extract: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(extractDir, entryPath))
	if err != nil {
		t.Fatalf("read %s: %v", entryPath, err)
	}
	return data
}

// --- Crash/atomicity (spec.md §54): a failed or successful export must
// never leave a half-written bundle at outputPath, and must never leave a
// stray ".tmp.*" file behind. ---

func TestExportRunLeavesNoTempFilesOnSuccess(t *testing.T) {
	fx := buildCompletedRunFixture(t)
	outDir := t.TempDir()
	bundlePath := filepath.Join(outDir, "run-audit.tar")
	if err := ExportRun(context.Background(), fx.workspace, fx.runID, bundlePath, ExportOptions{}); err != nil {
		t.Fatalf("ExportRun: %v", err)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "run-audit.tar" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("output dir contains %v, want exactly [run-audit.tar]", names)
	}
}

func TestExportRunOverwritesExistingBundleAtomically(t *testing.T) {
	fx := buildCompletedRunFixture(t)
	bundlePath := filepath.Join(t.TempDir(), "run-audit.tar")
	if err := os.WriteFile(bundlePath, []byte("stale placeholder content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ExportRun(context.Background(), fx.workspace, fx.runID, bundlePath, ExportOptions{}); err != nil {
		t.Fatalf("ExportRun: %v", err)
	}
	result, err := VerifyBundle(context.Background(), bundlePath, VerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if result.Verdict != AuditVerdictPass {
		t.Fatalf("overwritten bundle verdict = %#v, want pass", result)
	}
}

func TestExportRunFailsClosedWhenOutputIsUnwritableDirectory(t *testing.T) {
	fx := buildCompletedRunFixture(t)
	// outputPath's parent cannot be created because a file already occupies
	// that path component.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundlePath := filepath.Join(blocker, "run-audit.tar")
	if err := ExportRun(context.Background(), fx.workspace, fx.runID, bundlePath, ExportOptions{}); err == nil {
		t.Fatal("expected an error when the output directory cannot be created")
	}
}

// --- Secret redaction (spec.md §44): no known secret survives into
// bundle.json, decision-witness.json, run-result.json, or receipts/*.json.
// This is a regression test on auditverify's own export path, not on
// team's redaction boundary itself (already exercised there): every file
// auditverify writes is built from data that already passed through
// team.EventStore's AppendPersisted, which redacts JSON payloads before they
// become durable. ---

func TestExportRunRedactsSecretsCarriedInRunResultReason(t *testing.T) {
	workspace := t.TempDir()
	runID := "run-secret-fixture"
	const secretValue = "sk-verysecretvalue1234567890abcdef"

	store, err := team.NewEventStore(workspace, runID, "session-secret-fixture")
	if err != nil {
		t.Fatalf("new event store: %v", err)
	}
	defer func() { _ = store.Close() }()

	runResult := team.RunResult{
		RunID: runID, Outcome: team.RunOutcomeFailed, GoalSatisfied: false,
		StopReason: team.StopReasonRunFailed,
		Reason:     "task failed because api_key=" + secretValue + " was rejected by the provider",
	}
	payload, err := json.Marshal(runResult)
	if err != nil {
		t.Fatal(err)
	}
	// AppendPersisted (not the legacy Append) is the redacting boundary that
	// every production run_finished event goes through.
	if _, err := store.AppendPersisted(team.RunEvent{Type: "run_finished", Actor: "coordinator", RunID: runID, Payload: payload}); err != nil {
		t.Fatalf("append run_finished: %v", err)
	}

	bundlePath := filepath.Join(t.TempDir(), "run-audit.tar")
	if err := ExportRun(context.Background(), workspace, runID, bundlePath, ExportOptions{}); err != nil {
		t.Fatalf("ExportRun: %v", err)
	}

	extractDir := t.TempDir()
	f, err := os.Open(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := extractTarArchive(f, extractDir); err != nil {
		t.Fatalf("extract: %v", err)
	}

	for _, name := range []string{"bundle.json", "decision-witness.json", "run-result.json", "events.jsonl"} {
		data, readErr := os.ReadFile(filepath.Join(extractDir, name))
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		if strings.Contains(string(data), secretValue) {
			t.Fatalf("%s contains the raw secret value:\n%s", name, data)
		}
	}
}

// --- Legacy/backward compatibility (spec.md §40): missing optional data
// (no evidence manifest, no acceptance) on an honestly non-completed run
// must not fabricate a FAIL; the run must still audit PASS. ---

func TestVerifyWorkspaceRunLegacyRunWithNoOptionalDataIsPass(t *testing.T) {
	workspace := t.TempDir()
	runID := "run-legacy-minimal"
	store, err := team.NewEventStore(workspace, runID, "session-legacy-minimal")
	if err != nil {
		t.Fatalf("new event store: %v", err)
	}
	defer func() { _ = store.Close() }()

	// The oldest possible shape: no acceptance, no evidence manifest, no
	// stats/metrics -- just a bare cancelled outcome.
	runResult := team.RunResult{RunID: runID, Outcome: team.RunOutcomeCancelled, ExitCode: 130}
	payload, err := json.Marshal(runResult)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(team.RunEvent{Type: "run_finished", Actor: "coordinator", RunID: runID, Payload: payload}); err != nil {
		t.Fatalf("append run_finished: %v", err)
	}

	result, err := VerifyWorkspaceRun(context.Background(), workspace, runID, VerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyWorkspaceRun: %v", err)
	}
	if result.Verdict != AuditVerdictPass {
		t.Fatalf("legacy minimal run result = %#v, want pass", result)
	}
	if result.Evidence.Status != AuditDimensionSkipped || result.Acceptance.Status != AuditDimensionSkipped {
		t.Fatalf("legacy minimal run should skip (not fail) undecided dimensions: evidence=%#v acceptance=%#v", result.Evidence, result.Acceptance)
	}
}

// A legacy run that nonetheless claims completed without any evidence must
// still fail closed -- absence of optional data never excuses a "completed"
// claim that has no proof behind it (spec.md §40's own carve-out).
func TestVerifyWorkspaceRunLegacyCompletedClaimWithoutEvidenceStillFails(t *testing.T) {
	workspace := t.TempDir()
	runID := "run-legacy-completed-unproven"
	store, err := team.NewEventStore(workspace, runID, "session-legacy-completed-unproven")
	if err != nil {
		t.Fatalf("new event store: %v", err)
	}
	defer func() { _ = store.Close() }()

	runResult := team.RunResult{RunID: runID, Outcome: team.RunOutcomeCompleted, GoalSatisfied: true}
	payload, err := json.Marshal(runResult)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(team.RunEvent{Type: "run_finished", Actor: "coordinator", RunID: runID, Payload: payload}); err != nil {
		t.Fatalf("append run_finished: %v", err)
	}

	result, err := VerifyWorkspaceRun(context.Background(), workspace, runID, VerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyWorkspaceRun: %v", err)
	}
	if result.Verdict != AuditVerdictFail {
		t.Fatalf("unproven legacy completed claim = %#v, want fail", result)
	}
}

// --- Remaining spec.md §53 security cases: invalid JSON, a malformed hash
// value, and an unsupported future schema version must all be rejected
// cleanly (an error or a FAIL verdict), never a panic or a fabricated pass.

func TestVerifyBundleRejectsInvalidBundleJSON(t *testing.T) {
	bundlePath := exportFixtureBundle(t)
	tamperTarEntry(t, bundlePath, "bundle.json", func([]byte) []byte {
		return []byte("{not valid json")
	})
	if _, err := VerifyBundle(context.Background(), bundlePath, VerifyOptions{}); err == nil {
		t.Fatal("expected an error for invalid bundle.json")
	}
}

func TestVerifyBundleRejectsMalformedHashValue(t *testing.T) {
	bundlePath := exportFixtureBundle(t)
	rewriteTarArchive(t, bundlePath, func(entries []bundleFileEntry) []bundleFileEntry {
		for i := range entries {
			if entries[i].path != "bundle.json" {
				continue
			}
			var manifest AuditBundleManifest
			if err := json.Unmarshal(entries[i].data, &manifest); err != nil {
				t.Fatalf("decode bundle.json: %v", err)
			}
			for j := range manifest.Files {
				if manifest.Files[j].Path == "run-result.json" {
					manifest.Files[j].SHA256 = "not-a-hex-digest"
				}
			}
			// Reseal so BundleHash itself is internally consistent with the
			// malformed value: this isolates the per-file comparison
			// (file.SHA256 vs the actual recomputed digest) as the thing
			// under test, rather than merely re-triggering VerifyHash's own
			// (already separately tested) mismatch detection.
			if err := manifest.Seal(); err != nil {
				t.Fatalf("reseal: %v", err)
			}
			data, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			entries[i].data = data
		}
		return entries
	})
	result, err := VerifyBundle(context.Background(), bundlePath, VerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if result.Verdict != AuditVerdictFail {
		t.Fatalf("malformed hash verdict = %#v, want fail", result)
	}
}

func TestVerifyBundleRejectsUnsupportedFutureSchemaVersion(t *testing.T) {
	bundlePath := exportFixtureBundle(t)
	tamperTarEntry(t, bundlePath, "bundle.json", func(data []byte) []byte {
		var manifest AuditBundleManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatalf("decode bundle.json: %v", err)
		}
		// Set this after Seal() (called during export), not before: Seal()
		// unconditionally stamps the current SchemaVersion, so setting it
		// first would just be overwritten. The version-gate check runs
		// before hash verification, so the now-inconsistent BundleHash is
		// never reached.
		manifest.SchemaVersion = BundleSchemaVersion + 1
		out, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		return out
	})
	if _, err := VerifyBundle(context.Background(), bundlePath, VerifyOptions{}); err == nil {
		t.Fatal("expected an explicit error for an unsupported future bundle schema version")
	}
}

// --- Provenance (spec.md §26-27, AC-7): binding/receipt mismatches must
// FAIL, never guess a winner. buildProvenanceFixture builds a single task
// with the given receipts and evidence binding so each case only has to vary
// the one thing it's testing. ---

func buildProvenanceFixture(t *testing.T, receipts []team.ExecutionReceipt, binding team.EvidenceBinding) fixture {
	t.Helper()
	workspace := t.TempDir()
	runID := "run-provenance-fixture"
	taskID := "t1"

	store, err := team.NewEventStore(workspace, runID, "session-provenance-fixture")
	if err != nil {
		t.Fatalf("new event store: %v", err)
	}
	defer func() { _ = store.Close() }()
	mustAppend := func(event team.RunEvent) {
		if _, err := store.AppendPersisted(event); err != nil {
			t.Fatalf("append %s: %v", event.Type, err)
		}
	}

	mustAppend(team.RunEvent{Type: "task_created", Actor: "coordinator", RunID: runID, TaskID: taskID,
		Payload: mustJSON(t, taskEventPayload{ID: taskID, Status: "pending", Desc: "do the thing", Agent: "worker"})})
	mustAppend(team.RunEvent{Type: "task_completed", Actor: "worker", RunID: runID, TaskID: taskID,
		Payload: mustJSON(t, taskEventPayload{ID: taskID, Status: "done", Desc: "do the thing", Agent: "worker", ExecutionReceipts: receipts})})

	artifactStore, err := team.NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	var artifactRefs []team.ArtifactRef
	for _, id := range binding.ArtifactIDs {
		putResult, err := artifactStore.Put(context.Background(), team.PutArtifactRequest{
			Kind: "task_transcript", Path: id + ".txt", Content: []byte("transcript for " + id),
			RunID: runID, TaskID: taskID, Attempt: binding.Attempt, Agent: "worker",
		})
		if err != nil {
			t.Fatalf("put artifact: %v", err)
		}
		artifactRefs = append(artifactRefs, putResult.ArtifactRef)
	}

	manifest := &team.EvidenceManifest{
		RunID: runID, Status: "accepted", ArtifactRefs: artifactRefs,
		EvidenceResults: []team.EvidenceResult{
			{RequirementID: "task:" + taskID, Status: "passed", ArtifactRefs: artifactRefs, Binding: &binding},
			{RequirementID: "run:acceptance", Status: "passed"},
		},
	}
	if err := manifest.Seal(); err != nil {
		t.Fatalf("seal manifest: %v", err)
	}

	runResult := team.RunResult{
		RunID: runID, Outcome: team.RunOutcomeCompleted, GoalSatisfied: true,
		StopReason: team.StopReasonCompleted, Acceptance: &team.AcceptanceResult{State: team.AcceptancePassed, Passed: true},
		EvidenceManifest: manifest,
	}
	mustAppend(team.RunEvent{Type: "run_finished", Actor: "coordinator", RunID: runID, Payload: mustJSON(t, runResult)})

	return fixture{workspace: workspace, runID: runID, taskID: taskID}
}

func TestVerifyWorkspaceRunAmbiguousReceiptsFailProvenance(t *testing.T) {
	fx := buildProvenanceFixture(t,
		[]team.ExecutionReceipt{
			{RunID: "run-provenance-fixture", TaskID: "t1", Attempt: 1, ModelExecutionID: "exec-a", ProducerID: "worker-a", ExitCode: intPtr(0), TranscriptRef: "sha256-a"},
			{RunID: "run-provenance-fixture", TaskID: "t1", Attempt: 1, ModelExecutionID: "exec-b", ProducerID: "worker-b", ExitCode: intPtr(0), TranscriptRef: "sha256-b"},
		},
		team.EvidenceBinding{RunID: "run-provenance-fixture", TaskID: "t1", Attempt: 1, ModelExecutionID: "exec-a", ProducerID: "worker-a", TranscriptRef: "sha256-a", ArtifactIDs: []string{"sha256-a"}},
	)
	result, err := VerifyWorkspaceRun(context.Background(), fx.workspace, fx.runID, VerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyWorkspaceRun: %v", err)
	}
	if result.Provenance.Status != AuditDimensionFail {
		t.Fatalf("ambiguous receipts provenance = %#v, want fail", result.Provenance)
	}
	if result.Verdict != AuditVerdictFail {
		t.Fatalf("ambiguous receipts verdict = %s, want fail", result.Verdict)
	}
}

func TestVerifyWorkspaceRunBindingAttemptMismatchFailsProvenance(t *testing.T) {
	fx := buildProvenanceFixture(t,
		[]team.ExecutionReceipt{
			{RunID: "run-provenance-fixture", TaskID: "t1", Attempt: 2, ModelExecutionID: "exec-a", ProducerID: "worker-a", ExitCode: intPtr(0), TranscriptRef: "sha256-a"},
		},
		// Binding claims attempt 1, but the only successful receipt is attempt 2.
		team.EvidenceBinding{RunID: "run-provenance-fixture", TaskID: "t1", Attempt: 1, ModelExecutionID: "exec-a", ProducerID: "worker-a", TranscriptRef: "sha256-a", ArtifactIDs: []string{"sha256-a"}},
	)
	result, err := VerifyWorkspaceRun(context.Background(), fx.workspace, fx.runID, VerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyWorkspaceRun: %v", err)
	}
	if result.Provenance.Status != AuditDimensionFail {
		t.Fatalf("binding attempt mismatch provenance = %#v, want fail", result.Provenance)
	}
}

func TestVerifyWorkspaceRunBindingProducerMismatchFailsProvenance(t *testing.T) {
	fx := buildProvenanceFixture(t,
		[]team.ExecutionReceipt{
			{RunID: "run-provenance-fixture", TaskID: "t1", Attempt: 1, ModelExecutionID: "exec-a", ProducerID: "worker-a", ExitCode: intPtr(0), TranscriptRef: "sha256-a"},
		},
		// Binding claims a different producer than the actual receipt's.
		team.EvidenceBinding{RunID: "run-provenance-fixture", TaskID: "t1", Attempt: 1, ModelExecutionID: "exec-a", ProducerID: "worker-imposter", TranscriptRef: "sha256-a", ArtifactIDs: []string{"sha256-a"}},
	)
	result, err := VerifyWorkspaceRun(context.Background(), fx.workspace, fx.runID, VerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyWorkspaceRun: %v", err)
	}
	if result.Provenance.Status != AuditDimensionFail {
		t.Fatalf("binding producer mismatch provenance = %#v, want fail", result.Provenance)
	}
}

func TestVerifyWorkspaceRunMissingReceiptForBindingFailsProvenance(t *testing.T) {
	fx := buildProvenanceFixture(t,
		nil, // no execution receipts at all
		team.EvidenceBinding{RunID: "run-provenance-fixture", TaskID: "t1", Attempt: 1, ModelExecutionID: "exec-a", ProducerID: "worker-a", TranscriptRef: "sha256-a", ArtifactIDs: []string{"sha256-a"}},
	)
	result, err := VerifyWorkspaceRun(context.Background(), fx.workspace, fx.runID, VerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyWorkspaceRun: %v", err)
	}
	if result.Provenance.Status != AuditDimensionFail {
		t.Fatalf("missing receipt provenance = %#v, want fail", result.Provenance)
	}
}

// Legacy VerificationResult entries persisted before the Fingerprint field
// existed must not block recheck outright; they are simply unsupported for
// recheck (Incomplete), never a fabricated pass or an unrelated crash.
func TestRecheckToleratesLegacyVerificationResultWithoutFingerprint(t *testing.T) {
	legacy := &team.VerificationResult{
		Command: "true", ExitCode: 0, EvaluatedAt: time.Now(),
		Spec: &team.VerificationSpec{Type: team.VerifyCommandExit, Command: "true"},
		// Fingerprint intentionally left empty, as an old persisted record would.
	}
	outcome := recheckVerification(context.Background(), legacy)
	if outcome.attempted {
		t.Fatalf("command_exit must not be attempted for recheck without a recheck_safe marking: %#v", outcome)
	}
}
