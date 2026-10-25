package team

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// These tests cover the pure contract preflight: ValidateExecutionContractFull
// and the verifier-lint switch that decides whether a non-asserting verifier
// blocks dispatch. The coordinator's enforcement of these findings is covered
// by contract_enforcement_test.go.

func TestValidateExecutionContractFullLintModes(t *testing.T) {
	// `|| echo FAIL` is structurally non-asserting: the fallback always exits 0.
	const nonAsserting = "test -f artifact || echo FAIL"
	tests := []struct {
		name      string
		kind      ExecutionKind
		verify    string
		lintMode  string
		wantValid bool
		wantErrs  []string // error-severity finding codes
		wantWarns []string // warning-severity finding codes
	}{
		{"error mode rejects inline", ExecutionKindInline, nonAsserting, agent.VerifierLintError, false, []string{FindingVerifierNotAsserting}, nil},
		{"error mode rejects interactive", ExecutionKindInteractive, nonAsserting, agent.VerifierLintError, false, []string{FindingVerifierNotAsserting}, nil},
		{"error mode rejects external", ExecutionKindExternal, nonAsserting, agent.VerifierLintError, false, []string{FindingVerifierNotAsserting}, nil},
		{"error mode rejects process", ExecutionKindProcess, nonAsserting, agent.VerifierLintError, false, []string{FindingVerifierNotAsserting}, nil},
		{"warn mode downgrades to a warning", ExecutionKindInline, nonAsserting, agent.VerifierLintWarn, true, nil, []string{FindingVerifierNotAsserting}},
		{"off mode discards the finding", ExecutionKindInline, nonAsserting, agent.VerifierLintOff, true, nil, nil},
		{"empty mode defaults to error", ExecutionKindInline, nonAsserting, "", false, []string{FindingVerifierNotAsserting}, nil},
		{"unknown mode normalizes to error", ExecutionKindInline, nonAsserting, "bogus", false, []string{FindingVerifierNotAsserting}, nil},
		{"asserting verifier passes in error mode", ExecutionKindInline, "go test ./...", agent.VerifierLintError, true, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := TaskDef{Agent: "worker", Goal: "do work", Verify: tt.verify, Execution: ExecutionContract{Kind: tt.kind}}
			result := ValidateExecutionContractFull(task, tt.lintMode)
			if result.Valid != tt.wantValid {
				t.Errorf("Valid = %v, want %v; findings=%+v", result.Valid, tt.wantValid, result.Findings)
			}
			var errs, warns []string
			for _, f := range result.Findings {
				switch f.Severity {
				case FindingSeverityError:
					errs = append(errs, f.Code)
				case FindingSeverityWarning:
					warns = append(warns, f.Code)
				}
			}
			if !slices.Equal(errs, tt.wantErrs) {
				t.Errorf("error findings = %v, want %v", errs, tt.wantErrs)
			}
			if !slices.Equal(warns, tt.wantWarns) {
				t.Errorf("warning findings = %v, want %v", warns, tt.wantWarns)
			}
		})
	}
}

func TestValidateExecutionContractFullRejectsMalformedTypedVerifierForAllKinds(t *testing.T) {
	// A malformed typed verifier must be caught before dispatch for every
	// kind, not only interactive and external.
	for _, kind := range []ExecutionKind{ExecutionKindInline, ExecutionKindProcess, ExecutionKindInteractive, ExecutionKindExternal} {
		t.Run(string(kind), func(t *testing.T) {
			task := TaskDef{
				Agent:     "worker",
				Goal:      "do work",
				Execution: ExecutionContract{Kind: kind},
				// file_exists requires a non-empty path.
				VerifySpec: &VerificationSpec{Type: VerifyFileExists},
			}
			result := ValidateExecutionContractFull(task, agent.VerifierLintError)
			if result.Valid || result.Error() == nil {
				t.Fatalf("malformed typed verifier accepted for kind %q; findings=%+v", kind, result.Findings)
			}
			if !strings.Contains(result.Error().Error(), "verifier_invalid") {
				t.Errorf("error = %v, want the verifier_invalid code", result.Error())
			}
		})
	}
}

func TestValidateExecutionContractFullRejectsPrinterLastStage(t *testing.T) {
	// A verifier whose last pipeline stage only prints cannot assert anything.
	for _, verify := range []string{"go test ./... | cat", "echo done", "grep -q foo file | printf found"} {
		t.Run(verify, func(t *testing.T) {
			task := TaskDef{Agent: "worker", Goal: "do work", Verify: verify, Execution: ExecutionContract{Kind: ExecutionKindInline}}
			if result := ValidateExecutionContractFull(task, agent.VerifierLintError); result.Valid {
				t.Fatalf("printer verifier accepted; findings=%+v", result.Findings)
			}
		})
	}
}

func TestValidateExecutionContractFullStructuralErrorsIgnoreLintMode(t *testing.T) {
	// The lint mode only governs verifier assertiveness; structural errors
	// block dispatch in every mode.
	tests := []struct {
		name string
		task TaskDef
	}{
		{"invalid kind", TaskDef{Agent: "worker", Goal: "do work", Execution: ExecutionContract{Kind: ExecutionKind("bogus")}}},
		{"missing verifier with verification required", TaskDef{Agent: "worker", Goal: "do work", Execution: ExecutionContract{Kind: ExecutionKindInteractive, RequiresVerification: true}}},
	}
	for _, tt := range tests {
		for _, mode := range []string{agent.VerifierLintOff, agent.VerifierLintWarn, agent.VerifierLintError} {
			t.Run(tt.name+"/mode="+mode, func(t *testing.T) {
				if result := ValidateExecutionContractFull(tt.task, mode); result.Valid {
					t.Fatalf("structural error accepted with lintMode=%q", mode)
				}
			})
		}
	}
}

func TestValidateExecutionContractFullObservationModeExemptFromLint(t *testing.T) {
	for _, mode := range []string{agent.VerifierLintError, agent.VerifierLintWarn, agent.VerifierLintOff} {
		t.Run("mode="+mode, func(t *testing.T) {
			task := TaskDef{
				Agent:      "worker",
				Goal:       "observe",
				Verify:     "test -f artifact || echo FAIL",
				VerifyMode: "observation",
				Execution:  ExecutionContract{Kind: ExecutionKindInline},
			}
			if result := ValidateExecutionContractFull(task, mode); !result.Valid {
				t.Fatalf("observation verifier rejected in mode %q; findings=%+v", mode, result.Findings)
			}
		})
	}
}

func TestNormalizeVerifierLintMode(t *testing.T) {
	tests := map[string]string{
		"":      agent.VerifierLintError,
		"error": agent.VerifierLintError,
		"warn":  agent.VerifierLintWarn,
		"off":   agent.VerifierLintOff,
		"ERROR": agent.VerifierLintError, // unknown spellings fall back to error
		"bogus": agent.VerifierLintError,
	}
	for input, want := range tests {
		if got := agent.NormalizeVerifierLintMode(input); got != want {
			t.Errorf("NormalizeVerifierLintMode(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestParseTeamYMLVerifierLintMode(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"default when unset", "name: t\n", agent.VerifierLintError},
		{"explicit warn", "name: t\nreliability:\n  verifier-lint: warn\n", agent.VerifierLintWarn},
		{"explicit off", "name: t\nreliability:\n  verifier-lint: off\n", agent.VerifierLintOff},
		{"explicit error", "name: t\nreliability:\n  verifier-lint: error\n", agent.VerifierLintError},
		{"unknown normalizes to error", "name: t\nreliability:\n  verifier-lint: bogus\n", agent.VerifierLintError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(tt.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := parseTeamYML(dir, nil)
			if err != nil {
				t.Fatalf("parseTeamYML: %v", err)
			}
			if got := cfg.Reliability.VerifierLintMode; got != tt.want {
				t.Errorf("VerifierLintMode = %q, want %q", got, tt.want)
			}
		})
	}
}
