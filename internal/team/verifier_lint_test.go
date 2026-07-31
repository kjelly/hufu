package team

import (
	"testing"
)

// TestLintVerifier_AntiPatterns is the table-driven suite for all §4.3
// anti-patterns and their positive (asserting) counterparts.
//
// Every case is independently named and addressable as a Go sub-test.
// The "want" struct specifies either:
//   - none=true  → no verifier_not_asserting finding expected
//   - code+sev   → at least one finding with that code and severity expected
func TestLintVerifier_AntiPatterns(t *testing.T) {
	type want struct {
		code     string
		severity string
		none     bool
	}

	tests := []struct {
		name   string
		spec   VerificationSpec
		legacy string
		want   want
	}{
		// ── exemptions ──────────────────────────────────────────────────────

		// observation mode — all anti-patterns are exempt
		{
			name:   "observation/exempt: || true is ignored",
			spec:   VerificationSpec{Type: VerifyCommandExit, Mode: "observation", Command: "check || true"},
			want:   want{none: true},
		},
		{
			name:   "observation/exempt: echo alone is ignored",
			spec:   VerificationSpec{Type: VerifyCommandExit, Mode: "observation", Command: "echo status"},
			want:   want{none: true},
		},
		{
			name:   "observation/exempt: grep -c is ignored",
			spec:   VerificationSpec{Type: VerifyCommandExit, Mode: "observation", Command: "ps aux | grep -c running"},
			want:   want{none: true},
		},

		// typed non-command verifiers are always clean
		{
			name: "typed/file_exists is asserting",
			spec: VerificationSpec{Type: VerifyFileExists, Path: "report.md"},
			want: want{none: true},
		},
		{
			name: "typed/file_absent is asserting",
			spec: VerificationSpec{Type: VerifyFileAbsent, Path: "tmp/lock"},
			want: want{none: true},
		},
		{
			name: "typed/json_assert is asserting",
			spec: VerificationSpec{
				Type: VerifyJSONAssert,
				Path: "output.json",
				Assertions: []JSONAssertion{{Path: "status", Equals: "ok"}},
			},
			want: want{none: true},
		},

		// ── anti-pattern 1: tail swallows exit code (§4.3 row 1) ────────────

		// "|| true" always exits 0 → error
		{
			name:   "ap1/error: || true swallows failure",
			legacy: "some-check || true",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		// "|| echo <text>" — echo is terminal (no further pipe) → error
		{
			name:   "ap1/error: || echo FAIL is terminal",
			legacy: "some-check || echo FAIL",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		{
			name:   "ap1/error: || echo (bare) is terminal",
			legacy: "some-check || echo",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		// "|| echo ... | <cmd>" — rhs continues into pipeline → warning (undecidable)
		{
			name:   "ap1/warning: || echo fail | grep -q no-match is undecidable",
			legacy: "false || echo fail | grep -q no-match",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityWarning},
		},
		// "; exit 0" forces exit code → error
		{
			name:   "ap1/error: ; exit 0 forces exit code",
			legacy: "some-check; exit 0",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		// negative: || grep is asserting (grep exits 1 on no match)
		{
			name:   "ap1/clean: || grep -q is asserting",
			legacy: "check-a || grep -q pattern file",
			want:   want{none: true},
		},

		// ── anti-pattern 2: last stage is pure printer (§4.3 row 2) ─────────

		// Standalone pure-printer (single stage) → error (§4.3 explicitly lists echo/cat)
		{
			name:   "ap2/error: standalone echo cannot assert failure",
			legacy: "echo status",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		{
			name:   "ap2/error: standalone cat cannot assert failure",
			legacy: "cat result.txt",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		// Last stage in multi-stage pipeline is a printer → error
		{
			name:   "ap2/error: pipeline ending with echo",
			legacy: "some-assert | echo status",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		{
			name:   "ap2/error: pipeline ending with cat",
			legacy: "some-assert | cat result.txt",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		{
			name:   "ap2/error: pipeline ending with printf",
			legacy: "some-assert | printf '%s' ok",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		{
			name:   "ap2/error: pipeline ending with tee",
			legacy: "some-assert | tee output.log",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		// negative: last stage is grep -q (asserting)
		{
			name:   "ap2/clean: last stage grep -q is asserting",
			legacy: "ls /tmp | grep -q myfile",
			want:   want{none: true},
		},
		// negative: asserting single-command verifier
		{
			name:   "ap2/clean: test -f is asserting",
			legacy: "test -f report.md",
			want:   want{none: true},
		},

		// ── anti-pattern 3: assertion stage error discarded (§4.3 row 3) ───

		// Last stage has 2>/dev/null → error
		{
			name:   "ap3/error: 2>/dev/null on last stage discards assertion errors",
			legacy: "assert-tool 2>/dev/null",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		{
			name:   "ap3/error: 2>/dev/null in pipeline last stage",
			legacy: "some-check | assert-tool 2>/dev/null",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		{
			name:   "ap3/error: 2> /dev/null with space is also caught",
			legacy: "assert-tool 2> /dev/null",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		// negative: 2>/dev/null on a non-final stage does not trigger
		{
			name:   "ap3/clean: 2>/dev/null on non-final stage is ok",
			legacy: "cmd 2>/dev/null | grep -q pattern",
			want:   want{none: true},
		},

		// ── anti-pattern 4: count but don't compare (§4.3 row 4) ────────────

		// grep -c as terminal stage (with upstream pipe) → error
		{
			name:   "ap4/error: grep -c as last pipeline stage",
			legacy: "ls /tmp | grep -c running",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		{
			name:   "ap4/error: grep -ic (combined flags) as last pipeline stage",
			legacy: "ps aux | grep -ic running",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		// grep -c as standalone (no upstream pipe) → error (same reason: polarity ambiguous)
		{
			name:   "ap4/error: standalone grep -c is ambiguous",
			legacy: "grep -c running /var/log/app.log",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		// negative: grep -q is asserting (exits 1 when no match)
		{
			name:   "ap4/clean: grep -q is asserting",
			legacy: "ps aux | grep -q running",
			want:   want{none: true},
		},

		// ── clean real-world asserting commands ──────────────────────────────

		{
			name:   "clean: test -f is asserting",
			legacy: "test -f report.md",
			want:   want{none: true},
		},
		{
			name:   "clean: curl | grep -q is asserting",
			legacy: "curl -sf http://localhost/health | grep -q ok",
			want:   want{none: true},
		},
		{
			name:   "clean: complex pipeline with asserting last stage",
			legacy: "cat output.json | python3 -c \"import json,sys; d=json.load(sys.stdin); sys.exit(0 if d.get('ok') else 1)\"",
			want:   want{none: true},
		},

		// ── legacy command via second arg ────────────────────────────────────

		{
			name:   "legacy/arg: || true via legacyCommand arg",
			spec:   VerificationSpec{},
			legacy: "some-check || true",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		{
			name:   "legacy/arg: standalone echo via legacyCommand arg",
			spec:   VerificationSpec{},
			legacy: "echo hello",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			findings := LintVerifier(tc.spec, tc.legacy)

			if tc.want.none {
				for _, f := range findings {
					if f.Code == FindingVerifierNotAsserting {
						t.Errorf("expected no %s finding, got: severity=%s message=%q",
							FindingVerifierNotAsserting, f.Severity, f.Message)
					}
				}
				return
			}

			var found *ContractFinding
			for i := range findings {
				if findings[i].Code == tc.want.code {
					found = &findings[i]
					break
				}
			}
			if found == nil {
				t.Errorf("expected finding code=%q, got findings: %v", tc.want.code, findings)
				return
			}
			if found.Severity != tc.want.severity {
				t.Errorf("finding severity=%q, want %q; message=%q", found.Severity, tc.want.severity, found.Message)
			}
			if found.Field == "" {
				t.Errorf("finding.Field must not be empty")
			}
			if found.Message == "" {
				t.Errorf("finding.Message must not be empty")
			}
			if found.Hint == "" {
				t.Errorf("finding.Hint must not be empty")
			}
		})
	}
}

// TestSplitPipelineStages verifies the pipeline splitter handles `|` vs `||`
// and basic quoting correctly.
func TestSplitPipelineStages(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{
			name:    "no pipe → single stage",
			command: "test -f file.txt",
			want:    []string{"test -f file.txt"},
		},
		{
			name:    "single pipe → two stages",
			command: "ps aux | grep -q running",
			want:    []string{"ps aux ", " grep -q running"},
		},
		{
			name:    "|| is logical-or, not a pipeline split",
			command: "check || echo FAIL",
			want:    []string{"check || echo FAIL"},
		},
		{
			name:    "multi-stage pipeline",
			command: "cmd1 | cmd2 | grep -c X",
			want:    []string{"cmd1 ", " cmd2 ", " grep -c X"},
		},
		{
			name:    "pipe inside single quotes is not a split",
			command: "echo 'a|b' | grep a",
			want:    []string{"echo 'a|b' ", " grep a"},
		},
		{
			name:    "mixed || and | — || stays in stage, | splits",
			command: "a | b || echo fail",
			want:    []string{"a ", " b || echo fail"},
		},
		{
			name:    "|| echo ... | grep continues pipeline after ||",
			command: "false || echo fail | grep -q no-match",
			want:    []string{"false || echo fail ", " grep -q no-match"},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := SplitPipelineStages(tc.command)
			if len(got) != len(tc.want) {
				t.Fatalf("SplitPipelineStages(%q) = %v (len %d), want %v (len %d)",
					tc.command, got, len(got), tc.want, len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("stage[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestLintVerifier_ObservationExempt validates that observation-mode verifiers
// with known anti-patterns produce no verifier_not_asserting findings.
func TestLintVerifier_ObservationExempt(t *testing.T) {
	antis := []string{
		"check || true",
		"ps aux | grep -c running",
		"check || echo FAIL",
		"echo status",
		"cat result.txt",
		"assert 2>/dev/null",
	}
	for _, cmd := range antis {
		cmd := cmd
		t.Run(cmd, func(t *testing.T) {
			findings := LintVerifier(VerificationSpec{
				Type:    VerifyCommandExit,
				Mode:    "observation",
				Command: cmd,
			}, "")
			for _, f := range findings {
				if f.Code == FindingVerifierNotAsserting {
					t.Errorf("observation mode should be exempt; command=%q got finding: %v", cmd, f)
				}
			}
		})
	}
}
