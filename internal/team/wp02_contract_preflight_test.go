package team

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// TestValidateExecutionContractFull_LintModes is the table-driven test for
// WP-02's transitional verifier-lint switch (§4.3). It verifies that the lint
// mode controls whether non-asserting verifier findings block dispatch.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, §11, WP-02
func TestValidateExecutionContractFull_LintModes(t *testing.T) {
	// A verifier with `|| echo FAIL` — structurally non-asserting (§4.3 row 1).
	nonAssertingVerify := "test -f artifact || echo FAIL"

	tests := []struct {
		name      string
		kind      ExecutionKind
		verify    string
		lintMode  string
		wantValid bool
		wantErrs  []string // error-severity finding codes expected
		wantWarns []string // warning-severity finding codes expected
	}{
		{
			name:      "error mode rejects non-asserting verifier (inline)",
			kind:      ExecutionKindInline,
			verify:    nonAssertingVerify,
			lintMode:  agent.VerifierLintError,
			wantValid: false,
			wantErrs:  []string{FindingVerifierNotAsserting},
		},
		{
			name:      "error mode rejects non-asserting verifier (interactive)",
			kind:      ExecutionKindInteractive,
			verify:    nonAssertingVerify,
			lintMode:  agent.VerifierLintError,
			wantValid: false,
			wantErrs:  []string{FindingVerifierNotAsserting},
		},
		{
			name:      "error mode rejects non-asserting verifier (external)",
			kind:      ExecutionKindExternal,
			verify:    nonAssertingVerify,
			lintMode:  agent.VerifierLintError,
			wantValid: false,
			wantErrs:  []string{FindingVerifierNotAsserting},
		},
		{
			name:      "error mode rejects non-asserting verifier (process)",
			kind:      ExecutionKindProcess,
			verify:    nonAssertingVerify,
			lintMode:  agent.VerifierLintError,
			wantValid: false,
			wantErrs:  []string{FindingVerifierNotAsserting},
		},
		{
			name:      "warn mode downgrades to warning — still dispatches",
			kind:      ExecutionKindInline,
			verify:    nonAssertingVerify,
			lintMode:  agent.VerifierLintWarn,
			wantValid: true,
			wantWarns: []string{FindingVerifierNotAsserting},
		},
		{
			name:      "off mode discards lint finding — dispatches",
			kind:      ExecutionKindInline,
			verify:    nonAssertingVerify,
			lintMode:  agent.VerifierLintOff,
			wantValid: true,
		},
		{
			name:      "default (empty) lintMode is error — rejects",
			kind:      ExecutionKindInline,
			verify:    nonAssertingVerify,
			lintMode:  "",
			wantValid: false,
			wantErrs:  []string{FindingVerifierNotAsserting},
		},
		{
			name:      "unknown lintMode normalizes to error — rejects",
			kind:      ExecutionKindInline,
			verify:    nonAssertingVerify,
			lintMode:  "bogus",
			wantValid: false,
			wantErrs:  []string{FindingVerifierNotAsserting},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := TaskDef{
				Agent:  "worker",
				Goal:   "do work",
				Verify: tt.verify,
				Execution: ExecutionContract{
					Kind: tt.kind,
				},
			}
			result := ValidateExecutionContractFull(task, tt.lintMode)
			if result.Valid != tt.wantValid {
				t.Errorf("Valid = %v, want %v; findings=%+v", result.Valid, tt.wantValid, result.Findings)
			}
			errCount, warnCount := 0, 0
			for _, f := range result.Findings {
				switch f.Severity {
				case FindingSeverityError:
					errCount++
					if !containsCode(tt.wantErrs, f.Code) {
						t.Errorf("unexpected error finding code %q: %+v", f.Code, f)
					}
				case FindingSeverityWarning:
					warnCount++
					if !containsCode(tt.wantWarns, f.Code) {
						t.Errorf("unexpected warning finding code %q: %+v", f.Code, f)
					}
				}
			}
			if errCount != len(tt.wantErrs) {
				t.Errorf("error-severity count = %d, want %d", errCount, len(tt.wantErrs))
			}
			if warnCount != len(tt.wantWarns) {
				t.Errorf("warning-severity count = %d, want %d", warnCount, len(tt.wantWarns))
			}
		})
	}
}

func containsCode(codes []string, code string) bool {
	for _, c := range codes {
		if c == code {
			return true
		}
	}
	return false
}

// TestValidateExecutionContractFull_MalformedTypedVerifierAllKinds verifies
// that a malformed typed verifier is rejected at preflight for ALL
// ExecutionKinds, not only interactive/external (WP-02 spec: "inline kind 的
// malformed spec 也必須在派工前被攔下").
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §11, WP-02
func TestValidateExecutionContractFull_MalformedTypedVerifierAllKinds(t *testing.T) {
	kinds := []ExecutionKind{
		ExecutionKindInline,
		ExecutionKindProcess,
		ExecutionKindInteractive,
		ExecutionKindExternal,
	}
	for _, k := range kinds {
		t.Run(string(k), func(t *testing.T) {
			task := TaskDef{
				Agent: "worker",
				Goal:  "do work",
				Execution: ExecutionContract{
					Kind: k,
				},
				// file_exists requires a non-empty path — this is malformed.
				VerifySpec: &VerificationSpec{Type: VerifyFileExists},
			}
			result := ValidateExecutionContractFull(task, agent.VerifierLintError)
			if result.Valid {
				t.Fatalf("expected malformed typed verifier to be rejected for kind %q, got valid; findings=%+v", k, result.Findings)
			}
			if result.Error() == nil {
				t.Fatalf("expected error for malformed typed verifier, kind %q", k)
			}
			if !strings.Contains(result.Error().Error(), "verifier_invalid") {
				t.Errorf("expected verifier_invalid code in error, got: %v", result.Error())
			}
		})
	}
}

// TestValidateExecutionContractFull_LastStagePrinter verifies §4.3 row 2:
// a verifier whose last pipeline stage is a pure-output command (echo/cat)
// is rejected as non-asserting.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, §11, WP-02
func TestValidateExecutionContractFull_LastStagePrinter(t *testing.T) {
	tests := []struct {
		name   string
		verify string
	}{
		{name: "echo last stage", verify: "go test ./... | cat"},
		{name: "echo standalone", verify: "echo done"},
		{name: "printf last stage", verify: "grep -q foo file | printf found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := TaskDef{
				Agent:  "worker",
				Goal:   "do work",
				Verify: tt.verify,
				Execution: ExecutionContract{
					Kind: ExecutionKindInline,
				},
			}
			result := ValidateExecutionContractFull(task, agent.VerifierLintError)
			if result.Valid {
				t.Fatalf("expected non-asserting printer verifier to be rejected, got valid; findings=%+v", result.Findings)
			}
		})
	}
}

// TestValidateExecutionContractFull_StructuralErrorsIgnoreLintMode verifies
// that structural (non-lint) errors — invalid kind, missing verifier — always
// block dispatch regardless of lintMode, since lintMode only governs the
// verifier assertiveness lint (§4.3).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, WP-02
func TestValidateExecutionContractFull_StructuralErrorsIgnoreLintMode(t *testing.T) {
	tests := []struct {
		name string
		task TaskDef
	}{
		{
			name: "invalid kind ignored by lintMode=off",
			task: TaskDef{
				Agent: "worker",
				Goal:  "do work",
				Execution: ExecutionContract{
					Kind: ExecutionKind("bogus"),
				},
			},
		},
		{
			name: "missing verifier (interactive + reqVerify) ignored by lintMode=off",
			task: TaskDef{
				Agent: "worker",
				Goal:  "do work",
				Execution: ExecutionContract{
					Kind:                 ExecutionKindInteractive,
					RequiresVerification: true,
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, mode := range []string{agent.VerifierLintOff, agent.VerifierLintWarn, agent.VerifierLintError} {
				t.Run("mode="+mode, func(t *testing.T) {
					result := ValidateExecutionContractFull(tt.task, mode)
					if result.Valid {
						t.Fatalf("expected structural error to block dispatch even with lintMode=%q, got valid", mode)
					}
				})
			}
		})
	}
}

// TestValidateExecutionContractFull_ObservationExempt verifies that
// observation-mode verifiers remain exempt from the assertiveness lint across
// all lint modes.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, WP-02
func TestValidateExecutionContractFull_ObservationExempt(t *testing.T) {
	for _, mode := range []string{agent.VerifierLintError, agent.VerifierLintWarn, agent.VerifierLintOff} {
		t.Run("mode="+mode, func(t *testing.T) {
			task := TaskDef{
				Agent:      "worker",
				Goal:       "observe",
				Verify:     "test -f artifact || echo FAIL",
				VerifyMode: "observation",
				Execution: ExecutionContract{
					Kind: ExecutionKindInline,
				},
			}
			result := ValidateExecutionContractFull(task, mode)
			if !result.Valid {
				t.Fatalf("observation mode verifier should be exempt from lint in mode %q, got invalid; findings=%+v", mode, result.Findings)
			}
		})
	}
}

// TestValidateExecutionContractLegacy_Wrapper verifies the legacy wrapper
// preserves the original API contract: error mode, returns error on
// non-asserting verifier.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, WP-02
func TestValidateExecutionContractLegacy_Wrapper(t *testing.T) {
	task := TaskDef{
		Agent:  "worker",
		Goal:   "do work",
		Verify: "test -f artifact || echo FAIL",
		Execution: ExecutionContract{
			Kind: ExecutionKindInline,
		},
	}
	err := ValidateExecutionContract(task)
	if err == nil {
		t.Fatal("expected legacy wrapper to reject non-asserting verifier")
	}
	if !strings.Contains(err.Error(), "verifier contract error") {
		t.Errorf("expected 'verifier contract error' prefix, got: %v", err)
	}

	// Clean verifier passes.
	cleanTask := TaskDef{
		Agent:  "worker",
		Goal:   "do work",
		Verify: "go test ./...",
		Execution: ExecutionContract{
			Kind: ExecutionKindInline,
		},
	}
	if err := ValidateExecutionContract(cleanTask); err != nil {
		t.Fatalf("expected clean verifier to pass, got: %v", err)
	}
}

// TestCoordinatorValidateAndReportContract_WarnEmitsEvent verifies that the
// execution-path contract check (validateContractStructural) emits a
// contract_warning event for warning-severity findings in warn mode.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, WP-02
func TestCoordinatorValidateAndReportContract_WarnEmitsEvent(t *testing.T) {
	var events []string
	c := &Coordinator{}
	c.SetStatusReporter(func(e StatusEvent) {
		if e.Type == "contract_warning" {
			events = append(events, e.Message)
		}
	})
	c.session = &TeamSession{
		Config: agent.TeamConfig{
			Reliability: agent.ReliabilityConfig{
				VerifierLintMode: agent.VerifierLintWarn,
			},
		},
	}

	task := TaskDef{
		Agent:  "worker",
		Goal:   "do work",
		Verify: "test -f artifact || echo FAIL",
		Execution: ExecutionContract{
			Kind: ExecutionKindInline,
		},
	}
	if err := c.validateContractStructural(task, "1"); err != nil {
		t.Fatalf("expected warn mode to not block dispatch, got error: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected contract_warning event to be emitted for warning finding")
	}
	if !strings.Contains(events[0], FindingVerifierNotAsserting) {
		t.Errorf("expected event to mention %s, got: %s", FindingVerifierNotAsserting, events[0])
	}
}

// TestCoordinatorValidateAndReportContract_ErrorBlocksDispatch verifies that
// when lintMode=error (default), error-severity lint findings block dispatch
// and record a contract-class failure.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, §5, WP-02
func TestCoordinatorValidateAndReportContract_ErrorBlocksDispatch(t *testing.T) {
	var events []string
	c := &Coordinator{}
	c.SetStatusReporter(func(e StatusEvent) {
		if e.Type == "contract_warning" {
			events = append(events, e.Message)
		}
	})
	c.session = &TeamSession{
		Config: agent.TeamConfig{
			Reliability: agent.ReliabilityConfig{
				VerifierLintMode: agent.VerifierLintError,
			},
		},
	}

	task := TaskDef{
		Agent:  "worker",
		Goal:   "do work",
		Verify: "test -f artifact || echo FAIL",
		Execution: ExecutionContract{
			Kind: ExecutionKindInline,
		},
	}
	err := c.validateAndReportContract(task, "")
	if err == nil {
		t.Fatal("expected error mode to block dispatch")
	}
	if !strings.Contains(err.Error(), "verifier contract error") {
		t.Errorf("expected 'verifier contract error' in message, got: %v", err)
	}
	// No warning events should be emitted when blocking.
	if len(events) != 0 {
		t.Errorf("expected no contract_warning events in error mode, got: %v", events)
	}
}

// TestCoordinatorValidateAndReportContract_OffMode verifies that lintMode=off
// discards lint findings and dispatches without events.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, WP-02
func TestCoordinatorValidateAndReportContract_OffMode(t *testing.T) {
	var events []string
	c := &Coordinator{}
	c.SetStatusReporter(func(e StatusEvent) {
		if e.Type == "contract_warning" {
			events = append(events, e.Message)
		}
	})
	c.session = &TeamSession{
		Config: agent.TeamConfig{
			Reliability: agent.ReliabilityConfig{
				VerifierLintMode: agent.VerifierLintOff,
			},
		},
	}

	task := TaskDef{
		Agent:  "worker",
		Goal:   "do work",
		Verify: "test -f artifact || echo FAIL",
		Execution: ExecutionContract{
			Kind: ExecutionKindInline,
		},
	}
	if err := c.validateAndReportContract(task, ""); err != nil {
		t.Fatalf("expected off mode to not block dispatch, got error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected no contract_warning events in off mode, got: %v", events)
	}
}

// TestCoordinatorExecuteTasks_RejectsNonAssertingVerifier_WarnMode verifies
// that ExecuteTasks honors the verifier-lint transitional switch: warn mode
// allows dispatch of non-asserting verifier tasks rather than rejecting them.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, WP-02
func TestCoordinatorExecuteTasks_RejectsNonAssertingVerifier_WarnMode(t *testing.T) {
	c := &Coordinator{}
	c.session = &TeamSession{
		Config: agent.TeamConfig{
			Reliability: agent.ReliabilityConfig{
				VerifierLintMode: agent.VerifierLintWarn,
			},
		},
	}
	tasks := []TaskDef{
		{
			Agent:  "worker",
			Goal:   "do work",
			Verify: "test -f artifact || echo FAIL",
			Execution: ExecutionContract{
				Kind: ExecutionKindInline,
			},
		},
	}
	// In warn mode the contract preflight should NOT reject the task.
	// ExecuteTasks will fail later on agent resolution, but not on the contract.
	_, err := c.ExecuteTasks(context.Background(), tasks)
	if err != nil && strings.Contains(err.Error(), "verifier contract error") {
		t.Fatalf("warn mode should not reject non-asserting verifier at contract preflight, got: %v", err)
	}
}

// TestNormalizeVerifierLintMode verifies the normalization function.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, WP-02
func TestNormalizeVerifierLintMode(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "", want: agent.VerifierLintError},
		{input: "error", want: agent.VerifierLintError},
		{input: "warn", want: agent.VerifierLintWarn},
		{input: "off", want: agent.VerifierLintOff},
		{input: "ERROR", want: agent.VerifierLintError}, // unknown → error
		{input: "bogus", want: agent.VerifierLintError},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := agent.NormalizeVerifierLintMode(tt.input)
			if got != tt.want {
				t.Errorf("NormalizeVerifierLintMode(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestParseReliabilityConfig_VerifierLintMode verifies the YAML parser
// threads the verifier-lint transitional switch through to the config.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, WP-02
func TestParseReliabilityConfig_VerifierLintMode(t *testing.T) {
	tests := []struct {
		name     string
		yaml     string
		wantMode string
	}{
		{
			name:     "default when unset",
			yaml:     "name: t\n",
			wantMode: agent.VerifierLintError,
		},
		{
			name:     "explicit warn",
			yaml:     "name: t\nreliability:\n  verifier-lint: warn\n",
			wantMode: agent.VerifierLintWarn,
		},
		{
			name:     "explicit off",
			yaml:     "name: t\nreliability:\n  verifier-lint: off\n",
			wantMode: agent.VerifierLintOff,
		},
		{
			name:     "explicit error",
			yaml:     "name: t\nreliability:\n  verifier-lint: error\n",
			wantMode: agent.VerifierLintError,
		},
		{
			name:     "unknown normalizes to error",
			yaml:     "name: t\nreliability:\n  verifier-lint: bogus\n",
			wantMode: agent.VerifierLintError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(tt.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := parseTeamYML(dir, nil)
			if err != nil {
				t.Fatalf("parseTeamYML failed: %v", err)
			}
			got := cfg.Reliability.VerifierLintMode
			if got != tt.wantMode {
				t.Errorf("VerifierLintMode = %q, want %q", got, tt.wantMode)
			}
		})
	}
}
