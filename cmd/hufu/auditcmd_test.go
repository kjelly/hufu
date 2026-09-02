package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/auditverify"
	"github.com/kjelly/hufu/internal/team"
)

// resetAuditCLIFlags clears every audit subcommand's package-level flag
// variable. auditRunID et al. are bound once at init() via pflag's
// StringVar/BoolVar, so a flag omitted from a later SetArgs call keeps
// whatever value an earlier test (or an earlier case in the same test) left
// in place -- pflag only assigns a flag's variable when that flag is present
// in argv. Call this at the start of every audit CLI test (and again before
// each case in a table test) so tests are order-independent.
func resetAuditCLIFlags() {
	auditWorkspace, auditRunID, auditJSON, auditRecheck, auditBundle = "", "", false, false, ""
	auditExplainRunID, auditExplainJSON = "", false
	auditExportRunID, auditExportOutput, auditExportArtifactMode = "", "", ""
}

// buildAuditCLIFixture writes a minimal, legitimately-failed canonical run
// directly through team's exported append-only primitives -- the same
// approach internal/auditverify's own fixtures use -- so this test exercises
// real CLI wiring (flag parsing, exit codes, JSON framing) without needing a
// live coordinator run.
func buildAuditCLIFixture(t *testing.T) (workspace, runID string) {
	t.Helper()
	workspace = t.TempDir()
	runID = "run-cli-fixture"
	store, err := team.NewEventStore(workspace, runID, "session-cli-fixture")
	if err != nil {
		t.Fatalf("new event store: %v", err)
	}
	defer func() { _ = store.Close() }()

	result := team.RunResult{RunID: runID, Outcome: team.RunOutcomeFailed, GoalSatisfied: false, StopReason: team.StopReasonRunFailed, ExitCode: 1}
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal run result: %v", err)
	}
	if _, err := store.AppendPersisted(team.RunEvent{Type: "run_finished", Actor: "coordinator", RunID: runID, Payload: payload}); err != nil {
		t.Fatalf("append run_finished: %v", err)
	}
	return workspace, runID
}

func TestCLIAuditVerifyMissingRunFlagIsUsageError(t *testing.T) {
	resetAuditCLIFlags()
	root := newRootCommand()
	root.SetArgs([]string{"audit", "verify", "--workspace", t.TempDir()})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected an error when --run is omitted")
	}
	withCode, ok := err.(interface{ ProcessExitCode() int })
	if !ok || withCode.ProcessExitCode() != 2 {
		t.Fatalf("err = %v, want ProcessExitCode() == 2", err)
	}
}

func TestCLIAuditVerifyUnknownRunIsUsageError(t *testing.T) {
	resetAuditCLIFlags()
	workspace, _ := buildAuditCLIFixture(t)
	root := newRootCommand()
	root.SetArgs([]string{"audit", "verify", "--run", "run-does-not-exist", "--workspace", workspace})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected an error for an unknown run id")
	}
	withCode, ok := err.(interface{ ProcessExitCode() int })
	if !ok || withCode.ProcessExitCode() != 2 {
		t.Fatalf("err = %v, want ProcessExitCode() == 2", err)
	}
}

func TestCLIAuditVerifyPassOnLegitimateFailure(t *testing.T) {
	resetAuditCLIFlags()
	workspace, runID := buildAuditCLIFixture(t)
	root := newRootCommand()
	root.SetArgs([]string{"audit", "verify", "--run", runID, "--workspace", workspace})

	stdout := captureStdout(t, func() {
		if err := root.Execute(); err != nil {
			t.Fatalf("audit verify: %v", err)
		}
	})
	if !strings.Contains(stdout, "AUDIT PASS") {
		t.Fatalf("stdout = %q, want it to contain AUDIT PASS", stdout)
	}
}

func TestCLIAuditVerifyJSONIsSingleObjectOnStdout(t *testing.T) {
	resetAuditCLIFlags()
	workspace, runID := buildAuditCLIFixture(t)
	root := newRootCommand()
	root.SetArgs([]string{"audit", "verify", "--run", runID, "--workspace", workspace, "--json"})

	stdout := captureStdout(t, func() {
		if err := root.Execute(); err != nil {
			t.Fatalf("audit verify --json: %v", err)
		}
	})
	var result auditverify.AuditVerificationResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &result); err != nil {
		t.Fatalf("stdout is not a single JSON object: %v\nstdout=%q", err, stdout)
	}
	if result.Verdict != auditverify.AuditVerdictPass || result.RunID != runID {
		t.Fatalf("decoded result = %#v, want pass/%s", result, runID)
	}
}

func TestCLIAuditExplainMissingRunFlagIsUsageError(t *testing.T) {
	resetAuditCLIFlags()
	root := newRootCommand()
	root.SetArgs([]string{"audit", "explain", "--workspace", t.TempDir()})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected an error when --run is omitted")
	}
	withCode, ok := err.(interface{ ProcessExitCode() int })
	if !ok || withCode.ProcessExitCode() != 2 {
		t.Fatalf("err = %v, want ProcessExitCode() == 2", err)
	}
}

func TestCLIAuditExplainPassOnLegitimateFailure(t *testing.T) {
	resetAuditCLIFlags()
	workspace, runID := buildAuditCLIFixture(t)
	root := newRootCommand()
	root.SetArgs([]string{"audit", "explain", "--run", runID, "--workspace", workspace})

	stdout := captureStdout(t, func() {
		if err := root.Execute(); err != nil {
			t.Fatalf("audit explain: %v", err)
		}
	})
	if !strings.Contains(stdout, "was certified FAILED") {
		t.Fatalf("stdout = %q, want it to mention the certified outcome", stdout)
	}
}

func TestCLIAuditExplainJSONIsSingleObjectOnStdout(t *testing.T) {
	resetAuditCLIFlags()
	workspace, runID := buildAuditCLIFixture(t)
	root := newRootCommand()
	root.SetArgs([]string{"audit", "explain", "--run", runID, "--workspace", workspace, "--json"})

	stdout := captureStdout(t, func() {
		if err := root.Execute(); err != nil {
			t.Fatalf("audit explain --json: %v", err)
		}
	})
	var result auditverify.ExplainResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &result); err != nil {
		t.Fatalf("stdout is not a single JSON object: %v\nstdout=%q", err, stdout)
	}
	if result.Verification == nil || result.Verification.RunID != runID {
		t.Fatalf("decoded result = %#v, want verification for %s", result, runID)
	}
	if result.Witness == nil || result.Witness.Outcome != team.RunOutcomeFailed {
		t.Fatalf("decoded witness = %#v, want outcome failed", result.Witness)
	}
}

func TestCLIAuditExportMissingFlagsAreUsageErrors(t *testing.T) {
	resetAuditCLIFlags()
	cases := [][]string{
		{"audit", "export", "--output", filepath.Join(t.TempDir(), "b.tar"), "--workspace", t.TempDir()},
		{"audit", "export", "--run", "run-1", "--workspace", t.TempDir()},
	}
	for _, args := range cases {
		resetAuditCLIFlags()
		root := newRootCommand()
		root.SetArgs(args)
		err := root.Execute()
		if err == nil {
			t.Fatalf("args %v: expected a usage error", args)
		}
		withCode, ok := err.(interface{ ProcessExitCode() int })
		if !ok || withCode.ProcessExitCode() != 2 {
			t.Fatalf("args %v: err = %v, want ProcessExitCode() == 2", args, err)
		}
	}
}

func TestCLIAuditExportThenVerifyBundle(t *testing.T) {
	resetAuditCLIFlags()
	workspace, runID := buildAuditCLIFixture(t)
	bundlePath := filepath.Join(t.TempDir(), "run-audit.tar")

	exportRoot := newRootCommand()
	exportRoot.SetArgs([]string{"audit", "export", "--run", runID, "--workspace", workspace, "--output", bundlePath})
	if err := exportRoot.Execute(); err != nil {
		t.Fatalf("audit export: %v", err)
	}
	if _, err := os.Stat(bundlePath); err != nil {
		t.Fatalf("exported bundle missing: %v", err)
	}

	verifyRoot := newRootCommand()
	verifyRoot.SetArgs([]string{"audit", "verify", "--bundle", bundlePath})
	stdout := captureStdout(t, func() {
		if err := verifyRoot.Execute(); err != nil {
			t.Fatalf("audit verify --bundle: %v", err)
		}
	})
	if !strings.Contains(stdout, "AUDIT PASS") {
		t.Fatalf("stdout = %q, want it to contain AUDIT PASS", stdout)
	}
}

func TestCLIAuditVerifyRunAndBundleAreMutuallyExclusive(t *testing.T) {
	resetAuditCLIFlags()
	root := newRootCommand()
	root.SetArgs([]string{"audit", "verify", "--run", "run-1", "--bundle", "b.tar", "--workspace", t.TempDir()})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected an error when both --run and --bundle are set")
	}
	withCode, ok := err.(interface{ ProcessExitCode() int })
	if !ok || withCode.ProcessExitCode() != 2 {
		t.Fatalf("err = %v, want ProcessExitCode() == 2", err)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	original := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = original }()

	done := make(chan string, 1)
	go func() {
		buf := make([]byte, 0, 4096)
		chunk := make([]byte, 4096)
		for {
			n, readErr := r.Read(chunk)
			if n > 0 {
				buf = append(buf, chunk[:n]...)
			}
			if readErr != nil {
				break
			}
		}
		done <- string(buf)
	}()

	fn()
	_ = w.Close()
	return <-done
}
