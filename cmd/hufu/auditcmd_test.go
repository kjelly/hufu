package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/auditverify"
	"github.com/kjelly/hufu/internal/team"
)

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
