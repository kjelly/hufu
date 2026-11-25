package team

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const verifierFixtureSecret = "sk-proj-" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const verifierFixtureEscapedPrefix = "fixture encoded credential\n"
const verifierFixtureEscapedSecret = verifierFixtureEscapedPrefix + "with \"quoted\" fixture unique tail"
const verifierFixtureMixedDecodedSecret = "verify-mixed-boundary"
const verifierFixtureMixedRawSecret = `verify-mixed-\u0062oundary fixture mixed tail`
const verifierFixturePrefixedSecret = "prefix&unique-suffix"
const verifierFixtureSecretFile = "verification-fixture-secret.txt"
const verifierFixtureSecretOutput = ` && cat verification-fixture-secret.txt && cat verification-fixture-secret.txt >&2`
const verificationProjectionFaultTrigger = `CREATE TRIGGER refuse_normalized_checkpoint BEFORE UPDATE ON runtime_documents WHEN NEW.name LIKE 'normalized-completion/%' AND json_extract(NEW.body, '$.objective.completed')=1`

func verificationFixtureSecretContent(t *testing.T) string {
	t.Helper()
	t.Setenv("FIXTURE_VERIFY_PREFIX_API_TOKEN", verifierFixtureEscapedPrefix)
	t.Setenv("FIXTURE_VERIFY_API_TOKEN", verifierFixtureEscapedSecret)
	t.Setenv("FIXTURE_VERIFY_MIXED_DECODED_TOKEN", verifierFixtureMixedDecodedSecret)
	t.Setenv("FIXTURE_VERIFY_MIXED_RAW_TOKEN", verifierFixtureMixedRawSecret)
	t.Setenv("FIXTURE_VERIFY_PREFIXED_TOKEN", verifierFixturePrefixedSecret)
	encoded, err := json.Marshal(verifierFixtureEscapedSecret)
	if err != nil {
		t.Fatal(err)
	}
	prefixed := `prefix " {"diagnostic":"prefix\u0026unique-suffix"}`
	return verifierFixtureSecret + "\n" + prefixed + "\n" + `{"value":` + string(encoded) + `,"mixed":"` + verifierFixtureMixedRawSecret + `"}`
}

func safeVerificationFixtureOutput(output string) bool {
	return !strings.Contains(output, verifierFixtureSecret) && !strings.Contains(output, "fixture encoded credential") &&
		!strings.Contains(output, "fixture unique tail") && !strings.Contains(output, "fixture mixed tail") &&
		!strings.Contains(output, "unique-suffix") && strings.Count(output, "[REDACTED]") >= 4
}

func TestVerificationJSONAssertionReadsUnmaskedCommandOutput(t *testing.T) {
	dir := t.TempDir()
	content := verificationFixtureSecretContent(t)
	fixture := content[strings.LastIndexByte(content, '\n')+1:]
	if err := os.WriteFile(filepath.Join(dir, verifierFixtureSecretFile), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := executeJSONAssertVerification(t.Context(), "/bin/sh", dir, VerificationSpec{
		Command:    "cat " + verifierFixtureSecretFile,
		Assertions: []JSONAssertion{{Path: "value", Equals: verifierFixtureEscapedSecret}},
	})
	if err != nil || result.ExitCode != 0 || strings.Contains(result.Stdout+result.Stderr, "fixture encoded credential") {
		t.Fatalf("assertion failed to compare exact input privately: exit=%d err=%v", result.ExitCode, err)
	}
}

func TestVerificationEnvironmentFailureSurvivesRedactedReplay(t *testing.T) {
	t.Setenv("FIXTURE_VERIFY_API_TOKEN", "not found")
	for _, mode := range []string{"expected_failure", "observation"} {
		for _, kind := range []VerificationType{VerifyCommandExit, VerifyJSONAssert} {
			t.Run(mode+"/"+string(kind), func(t *testing.T) {
				spec := VerificationSpec{
					Type: kind, Mode: mode,
					Command:    `printf 'command %s\n' "$FIXTURE_VERIFY_API_TOKEN" >&2; exit 1`,
					Assertions: []JSONAssertion{{Path: "status", Equals: "passed"}},
				}
				if kind == VerifyCommandExit {
					spec.Assertions = nil
				}
				result, err := ExecuteVerificationSpec(t.Context(), "/bin/sh", t.TempDir(), spec)
				if err == nil || result == nil || result.EnvironmentFailure == nil || !*result.EnvironmentFailure || result.ExitCode != 1 {
					t.Fatalf("redaction changed the verification decision: result=%+v err=%v", result, err)
				}
				body, marshalErr := json.Marshal(result)
				if marshalErr != nil || strings.Contains(string(body)+err.Error(), "not found") || !strings.Contains(result.Stderr, "[REDACTED]") {
					t.Fatalf("saved diagnostics did not mask the environment value: %v", marshalErr)
				}
				var replayed VerificationResult
				if err := json.Unmarshal(body, &replayed); err != nil {
					t.Fatal(err)
				}
				if !sameCanonicalEvidence(toCanonicalVerifyResult(result), toCanonicalVerifyResult(&replayed)) || !verificationReportsEnvironmentFailure(&replayed) {
					t.Fatal("canonical verification projection lost the typed environment outcome")
				}
				if got := ClassifyTaskFailureStructured(FailureClassificationInput{
					Err: err, ExitCode: replayed.ExitCode, ExitCodeSource: ExitCodeSourceVerify,
					ResolveFindings: environmentFindingsFromVerifyResult(&replayed),
				}); got != FailureEnvironment {
					t.Fatalf("replayed verification class = %q, want environment", got)
				}
			})
		}
	}
}

func TestVerificationJSONAssertRetainsChildOutcome(t *testing.T) {
	for _, mode := range []string{"expected_failure", "observation"} {
		for _, tc := range []struct {
			name, command string
			exit          int
			cause         error
			startFailure  bool
			environment   bool
		}{
			{name: "deadline", command: "exec sleep 10", exit: -1, cause: context.DeadlineExceeded},
			{name: "canceled", command: "exit 0", exit: -1, cause: context.Canceled},
			{name: "signaled", command: "kill -TERM $$", exit: -1},
			{name: "could not start", command: "exit 0", exit: -1, startFailure: true, environment: true},
			{name: "command failed", command: "exit 7", exit: 7},
			{name: "malformed JSON", command: `printf '{'`, exit: -1},
			{name: "trailing JSON", command: `printf '{} {}'`, exit: -1},
			{name: "assertion mismatch", command: `printf '%s\n' '{"status":"command not found"}'`, exit: 1},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				ctx := t.Context()
				switch tc.cause {
				case context.DeadlineExceeded:
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 500*time.Millisecond)
					defer cancel()
				case context.Canceled:
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				shell := "/bin/sh"
				if tc.startFailure {
					shell = filepath.Join(dir, "absent-shell")
				}
				spec := VerificationSpec{
					Type: VerifyJSONAssert, Mode: mode, Command: tc.command,
					Assertions: []JSONAssertion{{Path: "status", Equals: "passed"}},
				}
				result, err := ExecuteVerificationSpec(ctx, shell, dir, spec)
				if result == nil || (err != nil) != (tc.exit < 0) || result.ExitCode != tc.exit ||
					result.TimedOut != (tc.cause == context.DeadlineExceeded) || (tc.cause != nil && !errors.Is(err, tc.cause)) {
					t.Fatalf("verification lost the child or assertion outcome: result=%+v err=%v", result, err)
				}
				body, marshalErr := json.Marshal(result)
				var replayed VerificationResult
				if marshalErr != nil || json.Unmarshal(body, &replayed) != nil || replayed.EnvironmentFailure == nil ||
					verificationReportsEnvironmentFailure(&replayed) != tc.environment ||
					!sameCanonicalEvidence(toCanonicalVerifyResult(result), toCanonicalVerifyResult(&replayed)) {
					t.Fatalf("replay lost the typed outcome: %v", marshalErr)
				}
				copy := cloneVerificationResult(&replayed)
				*copy.EnvironmentFailure = !tc.environment
				if verificationReportsEnvironmentFailure(&replayed) != tc.environment {
					t.Fatal("verification copy changed the original outcome")
				}
			})
		}
	}
	if !verificationReportsEnvironmentFailure(&VerificationResult{Stderr: "command not found"}) {
		t.Fatal("legacy verification lost its environment diagnostic")
	}
}

func TestVerificationMalformedJSONStaysPrivateAfterReopen(t *testing.T) {
	t.Setenv("FIXTURE_VERIFY_PASSWORD", "q")
	const diagnostic = "json_assert invalid JSON output at byte 1"
	c, journal, item := newDirectNormalizationRun(t, echoNormalization, func(spec *TodoSpec) {
		spec.OutputMode = TaskOutputModeVerbatim
		spec.VerifySpec = &VerificationSpec{
			Type: VerifyJSONAssert, Mode: "expected_failure",
			Command:    `printf '%s' "$FIXTURE_VERIFY_PASSWORD"`,
			Assertions: []JSONAssertion{{Path: "status", Equals: "passed"}},
		}
	})
	c.workerAgentOverride, _ = normalizationStreamedSubmission(c, item.ID, normalizationWire(approvedNormalizationDraft), nil)
	stopBeforeObjectiveVerification(t, c, item)
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reader, _ := restartDirectNormalizationRun(t, c, normalizationResumeProjection(t, c), item, nil)
	durable, err := reader.loadNormalizedCompletion(item.ID, todoByID(t, reader, item.ID).TypedResult, "")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := scriptWorker(failTurn(errors.New("unexpected model call")))
	reader.workerAgentOverride, reader.repairAgentOverride = forbidden, forbidden
	count, err := reader.ResumeInterruptedTasks(WithSessionResume(t.Context()))
	live := todoByID(t, reader, item.ID)
	if count != 1 || err == nil || live.Status != TaskBlocked || live.VerifyResult == nil ||
		live.VerifyResult.ExitCode != -1 || live.VerifyResult.Stderr != diagnostic || forbidden.Calls() != 0 ||
		strings.Contains(err.Error(), "'q'") {
		t.Fatalf("malformed input was accepted or echoed: count=%d status=%s verification=%+v error=%v", count, live.Status, live.VerifyResult, err)
	}
	docs, artifactRoot := workspaceDocs(reader.session.Workspace), reader.artifactStoreRootPath()
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	outcome, found, err := readNormalizedCompletionCheckpoint(docs, artifactRoot, durable.key)
	if err != nil || !found || !outcome.Objective.Completed || outcome.Objective.Error == "" ||
		strings.Contains(outcome.Objective.Error, "'q'") || outcome.Objective.Result == nil ||
		outcome.Objective.Result.Stderr != diagnostic || outcome.Objective.Result.ExitCode != -1 {
		t.Fatalf("reopened objective did not preserve the safe failure: found=%t outcome=%+v error=%v", found, outcome.Objective, err)
	}
}

func TestVerificationCommandBoundsRetainedEvidence(t *testing.T) {
	t.Setenv("FIXTURE_VERIFY_PASSWORD", "q")
	result, err := ExecuteVerificationSpec(t.Context(), "/bin/sh", t.TempDir(), VerificationSpec{
		Type: VerifyCommandExit, Mode: "expected_failure",
		Command: `head -c 33554432 /dev/zero | tr '\000' "$FIXTURE_VERIFY_PASSWORD"; printf '\nCOMMAND NOT FOUND\n'; exit 1`,
	})
	if err == nil || result == nil || result.ExitCode != 1 || !verificationReportsEnvironmentFailure(result) ||
		len(result.Stdout) > verificationEvidenceBytes+2 || result.rawStdout != "" ||
		!strings.HasPrefix(result.Stdout, "[REDACTED][REDACTED]") || strings.Contains(result.Stdout, "q") {
		t.Fatalf("large output changed the retained evidence or hid a late executable error: verification=%+v error=%v", result, err)
	}

	const keep = verificationRedactionPrefix + 24
	capture := verificationOutput{limit: keep}
	_, _ = capture.Write([]byte(strings.Repeat("x", 2*keep) + "EXECUTABLE FI"))
	_, _ = capture.Write([]byte("LE NOT FOUND"))
	if capture.prefix.Len() != keep || !capture.truncated || !capture.environmentFailure {
		t.Fatalf("captured %d bytes, truncated=%t, detected=%t", capture.prefix.Len(), capture.truncated, capture.environmentFailure)
	}

	warning, err := ExecuteVerificationSpec(t.Context(), "/bin/sh", t.TempDir(), VerificationSpec{
		Type:    VerifyCommandExit,
		Command: `head -c 131072 /dev/zero | tr '\000' ' '; printf '\nerror: append-only diagnostic %s\n' "$FIXTURE_VERIFY_PASSWORD"`,
	})
	if err != nil || warning == nil || !warning.WeakWarning || warning.WeakReason != "command exited 0 but output contains failure indicators" {
		t.Fatalf("late warning was lost or published private output: verification=%+v error=%v", warning, err)
	}

	weak := verificationOutput{limit: keep}
	_, _ = weak.Write([]byte(strings.Repeat("x", 2*keep) + strings.Repeat(" ", 2*keep) + "err"))
	_, _ = weak.Write([]byte("or: mid-line\n    FAIL"))
	if weak.weakWarning {
		t.Fatal("mid-line error label was interpreted as a line prefix")
	}
	_, _ = weak.Write([]byte("ED: test did not run"))
	if !weak.weakWarning {
		t.Fatal("split failure label was lost")
	}

	_, err = ExecuteVerificationSpec(t.Context(), "/bin/sh", t.TempDir(), VerificationSpec{
		Type:    VerifyCommandExit,
		Command: `head -c 131072 /dev/zero | tr '\000' ' '; grep -c found /dev/null; result=$?; head -c 131072 /dev/zero | tr '\000' ' '; exit "$result"`,
	})
	if !errors.Is(err, errWrongVerificationPolarity) {
		t.Fatalf("polarity check lost the full stdout after capture truncation: %v", err)
	}
}

func assertDirectVerificationWorkspaceResume(t *testing.T, reader *Coordinator, item *TodoItem, changed bool, forbidden *scriptedWorker, skepticCalls *atomic.Int32, fixture *isolatedTeamFixture, path, original, edited string) {
	t.Helper()
	checkpoint, err := reader.loadNormalizedCompletion(item.ID, item.TypedResult, "")
	if err != nil {
		t.Fatal(err)
	}
	world := reader.attemptWorldDirFor(item.ExecutionReceipt.ExecutionWorldID)
	assertPendingVerificationWorld(t, reader, world, checkpoint.key)
	if changed {
		writeUnverifiedApplyFixture(t, filepath.Join(world, attemptWorldRootDir, afterPreparedApplyPath))
	}
	_, err = reader.ResumeInterruptedTasks(WithSessionResume(t.Context()))
	got := todoByID(t, reader, item.ID)
	assertVerificationWorkspaceResume(t, reader, got, world, checkpoint.key, err, changed, skepticCalls.Load())
	want := edited
	if changed {
		want = original
	}
	if fixture.read(t, path) != want || forbidden.Calls() != 0 {
		t.Fatal("verified continuation did not preserve the expected edit and original worker")
	}
}

func assertPendingVerificationWorld(t *testing.T, c *Coordinator, world, key string) {
	t.Helper()
	if _, err := os.Stat(preparedApplyRecordDir(world)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected a fault before preparing any apply intent: %v", err)
	}
	sealed, published, err := readRuntimeEffectCompletion(c.artifactStoreRootPath(), key+"/"+objectiveVerification+"/outcome", normalizedVerificationRecordMaxBytes)
	if err != nil || published || !safeVerificationFixtureOutput(string(sealed)) {
		t.Fatalf("expected pending redacted verification evidence: published=%t error=%v", published, err)
	}
}

func assertVerificationWorkspaceResume(t *testing.T, c *Coordinator, result *TodoItem, world, key string, resumeErr error, changed bool, skepticCalls int32) {
	t.Helper()
	root := c.projectDir
	if changed {
		root = filepath.Join(world, attemptWorldRootDir)
		if localResultRejectionReason(resumeErr) != RepairFailureSource || result.Status != TaskBlocked || skepticCalls != 0 {
			t.Fatalf("changed world authorized a follow-up check or apply: status=%s skeptic=%d error=%v", result.Status, skepticCalls, resumeErr)
		}
		if _, err := os.Stat(filepath.Join(c.projectDir, "verifier.log")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("blocked recovery changed the canonical project: %v", err)
		}
	} else {
		if resumeErr != nil || result.Status != TaskDone || skepticCalls != 1 || result.VerifyResult == nil {
			t.Fatalf("original world did not settle: status=%s skeptic=%d error=%v", result.Status, skepticCalls, resumeErr)
		}
		for _, output := range []string{result.VerifyResult.Stdout, result.VerifyResult.Stderr} {
			if !safeVerificationFixtureOutput(output) {
				t.Fatal("resumed verification output was not redacted")
			}
		}
		body, found, err := workspaceDocs(c.session.Workspace).getDocument(key)
		if err != nil || !found || !safeVerificationFixtureOutput(string(body)) {
			t.Fatalf("verification projection lost its redaction: found=%t error=%v", found, err)
		}
	}
	if log, err := os.ReadFile(filepath.Join(root, "verifier.log")); err != nil || string(log) != "verified\n" {
		t.Fatalf("objective verification did not run exactly once: log=%q error=%v", log, err)
	}
	if _, err := os.Stat(filepath.Join(c.projectDir, afterPreparedApplyPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unverified extra file reached the canonical project: %v", err)
	}
}

func assertDirectApplyRequiresOriginalIntent(t *testing.T, reader *Coordinator, journal *EventStore, item *TodoItem, forbidden *scriptedWorker, skepticCalls *atomic.Int32, fixture *isolatedTeamFixture, path, original string) {
	t.Helper()
	checkpoint, err := reader.loadNormalizedCompletion(item.ID, item.TypedResult, "")
	if err != nil || checkpoint.DeltaSHA256 == "" {
		t.Fatalf("expected a prepared apply after real verification: %v", err)
	}
	world := reader.attemptWorldDirFor(item.ExecutionReceipt.ExecutionWorldID)
	execJournal(t, reader.session.Workspace, `UPDATE runtime_documents SET body=CAST(json_remove(body, '$.delta_sha256', '$.delta_manifest_sha256', '$.apply_sha256') AS BLOB) WHERE name=?`, checkpoint.key)
	writeUnverifiedApplyFixture(t, filepath.Join(world, attemptWorldRootDir, path))
	reader, _ = restartNormalizedIsolatedReader(t, reader, journal, item, forbidden)
	installCompletionSkeptic(reader, skepticCalls)
	_, err = reader.ResumeInterruptedTasks(WithSessionResume(t.Context()))
	got := todoByID(t, reader, item.ID)
	if localResultRejectionReason(err) != RepairFailureSource || got.Status != TaskBlocked ||
		forbidden.Calls() != 0 || skepticCalls.Load() != 1 || fixture.read(t, path) != original {
		t.Fatalf("recovery did not retain the original verified intent: status=%s error=%v model calls=%d skeptic=%d",
			got.Status, err, forbidden.Calls(), skepticCalls.Load())
	}
	log, err := os.ReadFile(filepath.Join(world, attemptWorldRootDir, "verifier.log"))
	if err != nil || string(log) != "verified\n" {
		t.Fatalf("recovery discarded the world or repeated the verifier: %q, %v", log, err)
	}
}

func removeCollectedApplyProjection(t *testing.T, c *Coordinator, cp *ResultCollectionCheckpoint, world *isolatedAttempt, path string) {
	t.Helper()
	execJournal(t, c.session.Workspace, `DELETE FROM runtime_documents WHERE name=?`, collectedApplyKey(cp))
	writeUnverifiedApplyFixture(t, filepath.Join(world.prepared.Root, path))
}

func writeUnverifiedApplyFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("changed after verification"), 0o600); err != nil {
		t.Fatal(err)
	}
}

const afterPreparedApplyPath = "unverified-extra.txt"

func removePendingApplyManifest(t *testing.T, worldDir string, wholeRecord bool) {
	t.Helper()
	path := preparedApplyRecordDir(worldDir)
	remove := os.RemoveAll
	if !wholeRecord {
		path = filepath.Join(path, runtimeEffectBodyFile)
		remove = os.Remove
	}
	if err := remove(path); err != nil {
		t.Fatal(err)
	}
	writeUnverifiedApplyFixture(t, filepath.Join(worldDir, attemptWorldRootDir, afterPreparedApplyPath))
}

func substitutePreparedFinalDigest(t *testing.T, worldDir string) {
	t.Helper()
	record, err := readPreparedApplyRecord(worldDir)
	if err != nil || record == nil {
		t.Fatalf("expected the verified original delta: %v", err)
	}
	// Preserve the legacy changes digest and the work binding.
	record.FinalDigest = hashContentKey("other final workspace")
	body, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(preparedApplyRecordDir(worldDir), runtimeEffectBodyFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertMissingPendingDirectApply(t *testing.T, reader *Coordinator, journal *EventStore, item *TodoItem, forbidden *scriptedWorker, skepticCalls *atomic.Int32, f *isolatedTeamFixture, path, original string) {
	t.Helper()
	world := reader.attemptWorldDirFor(item.ExecutionReceipt.ExecutionWorldID)
	removePendingApplyManifest(t, world, false)
	execJournal(t, reader.session.Workspace, "DROP TRIGGER refuse_normalized_checkpoint")
	reader, _ = restartNormalizedIsolatedReader(t, reader, journal, item, forbidden)
	installCompletionSkeptic(reader, skepticCalls)
	_, err := reader.ResumeInterruptedTasks(WithSessionResume(t.Context()))
	_, extraErr := os.Stat(filepath.Join(reader.projectDir, afterPreparedApplyPath))
	log, logErr := os.ReadFile(filepath.Join(world, attemptWorldRootDir, "verifier.log"))
	if localResultRejectionReason(err) != RepairFailureSource || todoByID(t, reader, item.ID).Status != TaskBlocked ||
		forbidden.Calls() != 0 || skepticCalls.Load() != 1 || f.read(t, path) != original || !errors.Is(extraErr, os.ErrNotExist) ||
		logErr != nil || string(log) != "verified\n" {
		t.Fatalf("lost pending manifest was rebuilt or checks repeated: error=%v, extra=%v, verifier=%q, read=%v", err, extraErr, log, logErr)
	}
}

func appendDivergentNormalizedApply(t *testing.T, c *Coordinator, item *TodoItem, failure string) {
	t.Helper()
	receipt := item.ExecutionReceipt
	checkpoint, err := c.loadNormalizedCompletion(item.ID, item.TypedResult, "")
	if err != nil || checkpoint.DeltaSHA256 == "" {
		t.Fatalf("expected a genuinely settled delta: %v", err)
	}
	result := cloneTaskResult(item.TypedResult)
	if failure == "forged_delta_manifest" {
		dir := c.attemptWorldDirFor(receipt.ExecutionWorldID)
		substitutePreparedFinalDigest(t, dir)
	} else if failure == "missing_apply_artifact" {
		if len(result.Artifacts) != 1 {
			t.Fatalf("expected a published result artifact: %+v", result.Artifacts)
		}
		faultNormalizedEvidencePath(t, normalizedArtifactData(c, result.Artifacts[0].ID), false)
	} else {
		result.Summary = "replaced apply result with matching structured hashes"
	}
	appendEvent := func(kind EventType, payload map[string]any) {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.EventJournal().Append(t.Context(), RunEvent{
			Type: string(kind), TaskID: item.ID, Attempt: receipt.Attempt, Actor: attemptWorldEventActor,
			IdempotencyKey: "fixture-alternate-" + string(kind), Payload: data,
		}); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(EventAttemptWorkspaceApplyStarted, map[string]any{
		"world_id": receipt.ExecutionWorldID, "delta_digest": checkpoint.DeltaSHA256, "verify_result": checkpoint.Objective.Result,
		"execution_receipt": receipt, "typed_result": result, "coordinator_output": item.Output,
	})
	if failure == "forged_already_applied" {
		appendEvent(EventAttemptWorkspaceApplied, map[string]any{"world_id": receipt.ExecutionWorldID, "delta_digest": checkpoint.DeltaSHA256})
	}
}
