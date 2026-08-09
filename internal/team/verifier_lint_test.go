package team

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
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
			name: "observation/exempt: || true is ignored",
			spec: VerificationSpec{Type: VerifyCommandExit, Mode: "observation", Command: "check || true"},
			want: want{none: true},
		},
		{
			name: "observation/exempt: echo alone is ignored",
			spec: VerificationSpec{Type: VerifyCommandExit, Mode: "observation", Command: "echo status"},
			want: want{none: true},
		},
		{
			name: "observation/exempt: grep -c is ignored",
			spec: VerificationSpec{Type: VerifyCommandExit, Mode: "observation", Command: "ps aux | grep -c running"},
			want: want{none: true},
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
				Type:       VerifyJSONAssert,
				Path:       "output.json",
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
		{
			name:   "ap1/error: || printf is terminal",
			legacy: "some-check || printf 'failed'",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		{
			name:   "ap1/error: || cat is terminal",
			legacy: "some-check || cat fail.log",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		{
			name:   "ap1/error: || tee is terminal",
			legacy: "some-check || tee fail.log",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		{
			name:   "ap1/error: || echo fail | cat ends with pure printer",
			legacy: "false || echo fail | cat",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		// "|| echo ... | <asserting_cmd>" — rhs ends with asserting command → clean
		{
			name:   "ap1/clean: || echo fail | grep -q no-match ends with asserting stage",
			legacy: "false || echo fail | grep -q no-match",
			want:   want{none: true},
		},
		{
			name:   "ap1/clean: assert || echo FAIL && false",
			legacy: "test -f file || echo FAIL && false",
			want:   want{none: true},
		},
		{
			name:   "ap1/clean: assert || false && true",
			legacy: "test -f file || false && true",
			want:   want{none: true},
		},
		{
			name:   "ap1/clean: assert || echo FAIL && false && true",
			legacy: "test -f file || echo FAIL && false && true",
			want:   want{none: true},
		},
		{
			name:   "ap2/clean: assert | echo STATUS && false && true",
			legacy: "test -f file | echo STATUS && false && true",
			want:   want{none: true},
		},
		{
			name:   "ap1/clean: assert || echo FAIL | cat && false",
			legacy: "test -f file || echo FAIL | cat && false",
			want:   want{none: true},
		},
		{
			name:   "ap1/error: assert || echo FAIL && true",
			legacy: "test -f file || echo FAIL && true",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		{
			name:   "ap1/error: assert || echo FAIL && exit 0",
			legacy: "test -f file || echo FAIL && exit 0",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		{
			name:   "ap1/error: assert || echo FAIL && true && exit 0",
			legacy: "test -f file || echo FAIL && true && exit 0",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		{
			name:   "ap1/warning: assert || echo FAIL && check_custom_script",
			legacy: "test -f file || echo FAIL && check_custom_script",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityWarning},
		},
		{
			name:   "ap1/warning: assert || echo FAIL && true && check_custom_script",
			legacy: "test -f file || echo FAIL && true && check_custom_script",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityWarning},
		},
		{
			name:   "ap2/error: assert | echo STATUS && true",
			legacy: "test -f file | echo STATUS && true",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		{
			name:   "ap2/error: assert | echo STATUS && exit 0",
			legacy: "test -f file | echo STATUS && exit 0",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		{
			name:   "ap2/error: assert | echo STATUS && true && exit 0",
			legacy: "test -f file | echo STATUS && true && exit 0",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		{
			name:   "ap2/warning: assert | echo STATUS && check_custom_script",
			legacy: "test -f file | echo STATUS && check_custom_script",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityWarning},
		},
		{
			name:   "ap2/warning: assert | echo STATUS && true && check_custom_script",
			legacy: "test -f file | echo STATUS && true && check_custom_script",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityWarning},
		},
		{
			name:   "ap2/clean: assert | echo STATUS && false",
			legacy: "test -f file | echo STATUS && false",
			want:   want{none: true},
		},
		// "; exit 0" forces exit code → error
		{
			name:   "ap1/error: ; exit 0 forces exit code",
			legacy: "some-check; exit 0",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		{
			name:   "ap1/error: ; EXIT 0 uppercase forces exit code",
			legacy: "some-check; EXIT 0",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		// negative: ; exit 1 is asserting
		{
			name:   "ap1/clean: ; exit 1 exits non-zero",
			legacy: "some-check; exit 1",
			want:   want{none: true},
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
		{
			name:   "ap2/error: standalone printf cannot assert failure",
			legacy: "printf 'ok\n'",
			want:   want{code: FindingVerifierNotAsserting, severity: FindingSeverityError},
		},
		{
			name:   "ap2/error: standalone print cannot assert failure",
			legacy: "print 'ok'",
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

func TestLintTeamContracts(t *testing.T) {
	tests := []struct {
		name      string
		session   *TeamSession
		wantCount int
		wantCode  string
	}{
		{
			name:      "nil session -> no findings",
			session:   nil,
			wantCount: 0,
		},
		{
			name: "valid acceptance -> no findings",
			session: &TeamSession{
				Config: agent.TeamConfig{
					Acceptance: "test -f report.md",
				},
			},
			wantCount: 0,
		},
		{
			name: "invalid commands beyond first",
			session: &TeamSession{
				Config: agent.TeamConfig{
					AcceptanceSpec: &agent.AcceptanceSpec{
						Commands: []string{"test -f report.md", "some-check || echo FAIL"},
					},
				},
			},
			wantCount: 1,
			wantCode:  FindingVerifierNotAsserting,
		},
		{
			name: "invalid verifications element",
			session: &TeamSession{
				Config: agent.TeamConfig{
					AcceptanceSpec: &agent.AcceptanceSpec{
						Verifications: []agent.VerificationSpec{
							{Type: VerifyCommandExit, Command: "some-check || true"},
						},
					},
				},
			},
			wantCount: 1,
			wantCode:  FindingVerifierNotAsserting,
		},
		{
			name: "invalid criterion element",
			session: &TeamSession{
				Config: agent.TeamConfig{
					AcceptanceSpec: &agent.AcceptanceSpec{
						Criteria: []agent.AcceptanceCriterion{
							{
								ID: "c1",
								Verify: agent.VerificationSpec{
									Type:    VerifyCommandExit,
									Command: "echo status",
								},
							},
						},
					},
				},
			},
			wantCount: 1,
			wantCode:  FindingVerifierNotAsserting,
		},
		{
			name: "observation mode in VerificationSpec exempts anti-patterns",
			session: &TeamSession{
				Config: agent.TeamConfig{
					AcceptanceSpec: &agent.AcceptanceSpec{
						Verifications: []agent.VerificationSpec{
							{Type: VerifyCommandExit, Command: "some-check || true", Mode: "observation"},
						},
					},
				},
			},
			wantCount: 0,
		},
		{
			name: "observation mode in Criteria.Verify exempts anti-patterns",
			session: &TeamSession{
				Config: agent.TeamConfig{
					AcceptanceSpec: &agent.AcceptanceSpec{
						Criteria: []agent.AcceptanceCriterion{
							{
								ID: "c1",
								Verify: agent.VerificationSpec{
									Type:    VerifyCommandExit,
									Command: "echo status",
									Mode:    "observation",
								},
							},
						},
					},
				},
			},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := LintTeamContracts(tt.session)
			if len(findings) != tt.wantCount {
				t.Fatalf("LintTeamContracts() returned %d findings, want %d: %v", len(findings), tt.wantCount, findings)
			}
			if tt.wantCount > 0 && tt.wantCode != "" {
				if findings[0].Code != tt.wantCode {
					t.Errorf("finding code = %q, want %q", findings[0].Code, tt.wantCode)
				}
			}
		})
	}
}

func TestLintTeamContracts_LoadTeam_Observation(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `name: obs-team
acceptance:
  verifications:
    - type: command_exit
      command: "some-check || true"
      mode: observation
  criteria:
    - id: c1
      required: true
      verify:
        type: command_exit
        command: "echo status"
        mode: observation
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write team.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worker.md"), []byte("name: worker\n"), 0644); err != nil {
		t.Fatalf("failed to write worker.md: %v", err)
	}

	session, err := LoadTeam(dir, nil, nil)
	if err != nil {
		t.Fatalf("LoadTeam failed: %v", err)
	}

	findings := LintTeamContracts(session)
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings for observation verifications/criteria, got %d: %v", len(findings), findings)
	}
}

func TestTeamContractPreflightIncludesStaticTasksAndExecutables(t *testing.T) {
	tests := []struct {
		name             string
		session          *TeamSession
		wantLintCode     string
		wantResolveCode  string
		wantLintField    string
		wantResolveField string
	}{
		{
			name: "static task verifier is linted and resolved",
			session: &TeamSession{
				Dir: t.TempDir(),
				ContractTasks: []TaskDef{{
					Verify: "missing-wp18-command || true",
				}},
			},
			wantLintCode:     FindingVerifierNotAsserting,
			wantResolveCode:  FindingExecutableUnresolved,
			wantLintField:    "tasks[0].verify",
			wantResolveField: "tasks[0].execution",
		},
		{
			name: "acceptance criterion command is resolved",
			session: &TeamSession{
				Dir: t.TempDir(),
				Config: agent.TeamConfig{AcceptanceSpec: &agent.AcceptanceSpec{Criteria: []agent.AcceptanceCriterion{{
					ID:     "artifact",
					Verify: agent.VerificationSpec{Type: VerifyCommandExit, Command: "missing-wp18-criterion-command"},
				}}}},
			},
			wantResolveCode:  FindingExecutableUnresolved,
			wantResolveField: "acceptance.criteria[0].execution",
		},
		{
			name: "typed non-command contract needs no executable resolution",
			session: &TeamSession{ContractTasks: []TaskDef{{
				VerifySpec: &VerificationSpec{Type: VerifyFileExists, Path: "result.txt"},
			}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lintFindings := LintTeamContracts(tt.session)
			if tt.wantLintCode == "" {
				if len(lintFindings) != 0 {
					t.Fatalf("LintTeamContracts() = %#v, want no findings", lintFindings)
				}
			} else if !hasContractFinding(lintFindings, tt.wantLintCode, tt.wantLintField) {
				t.Fatalf("LintTeamContracts() = %#v, want %s at %s", lintFindings, tt.wantLintCode, tt.wantLintField)
			}

			resolveFindings := ResolveTeamContractExecutables(tt.session, tt.session.Dir)
			if tt.wantResolveCode == "" {
				if len(resolveFindings) != 0 {
					t.Fatalf("ResolveTeamContractExecutables() = %#v, want no findings", resolveFindings)
				}
			} else if !hasContractFinding(resolveFindings, tt.wantResolveCode, tt.wantResolveField) {
				t.Fatalf("ResolveTeamContractExecutables() = %#v, want %s at %s", resolveFindings, tt.wantResolveCode, tt.wantResolveField)
			}
		})
	}
}

func TestLoadTeamRetainsStaticContractTasks(t *testing.T) {
	dir := t.TempDir()
	teamYAML := `name: doctor-contracts
tasks:
  - agent: worker
    goal: verify the artifact
    verify-spec:
      type: command_exit
      command: test -f artifact.txt
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(teamYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	agentMarkdown := "---\nname: worker\nrole: worker\n---\nWorker.\n"
	if err := os.WriteFile(filepath.Join(dir, "worker.md"), []byte(agentMarkdown), 0o600); err != nil {
		t.Fatal(err)
	}

	session, err := LoadTeam(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(session.ContractTasks) != 1 || session.ContractTasks[0].VerifySpec == nil || session.ContractTasks[0].VerifySpec.Command != "test -f artifact.txt" {
		t.Fatalf("ContractTasks = %#v, want static task contract", session.ContractTasks)
	}
}

func hasContractFinding(findings []ContractFinding, code, field string) bool {
	for _, finding := range findings {
		if finding.Code == code && finding.Field == field {
			return true
		}
	}
	return false
}

func TestEvaluateAndChain(t *testing.T) {
	tests := []struct {
		name         string
		andParts     []string
		wantSeverity string
		wantFinding  bool
	}{
		{
			name:         "asserting failure: single false",
			andParts:     []string{"false"},
			wantSeverity: "",
			wantFinding:  false,
		},
		{
			name:         "asserting failure: false followed by true",
			andParts:     []string{"false", "true"},
			wantSeverity: "",
			wantFinding:  false,
		},
		{
			name:         "asserting failure: custom_script followed by false",
			andParts:     []string{"custom_script", "false"},
			wantSeverity: "",
			wantFinding:  false,
		},
		{
			name:         "always success: true followed by exit 0",
			andParts:     []string{"true", "exit 0"},
			wantSeverity: FindingSeverityError,
			wantFinding:  true,
		},
		{
			name:         "always success: echo followed by true",
			andParts:     []string{"echo hello", "true"},
			wantSeverity: FindingSeverityError,
			wantFinding:  true,
		},
		{
			name:         "unknown control-list: single custom_script",
			andParts:     []string{"custom_script"},
			wantSeverity: FindingSeverityWarning,
			wantFinding:  true,
		},
		{
			name:         "unknown control-list: true followed by custom_script",
			andParts:     []string{"true", "custom_script"},
			wantSeverity: FindingSeverityWarning,
			wantFinding:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			severity, isFinding := evaluateAndChain(tt.andParts)
			if isFinding != tt.wantFinding {
				t.Errorf("evaluateAndChain(%v) isFinding = %v, want %v", tt.andParts, isFinding, tt.wantFinding)
			}
			if severity != tt.wantSeverity {
				t.Errorf("evaluateAndChain(%v) severity = %q, want %q", tt.andParts, severity, tt.wantSeverity)
			}
		})
	}
}
