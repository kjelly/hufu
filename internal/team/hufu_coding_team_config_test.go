package team

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// hufuCodingTeamDir locates .agent-teams/hufu-coding/ relative to this
// source file (runtime.Caller), independent of the test binary's working
// directory — the same pattern task_result_contract_test.go's
// TestReviewerPromptMatchesEffectiveWorksetResultContract uses for
// .agent-teams/hufu-code-review/.
func hufuCodingTeamDir(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(sourceFile), "..", "..", ".agent-teams", "hufu-coding")
}

func loadHufuCodingTeam(t *testing.T) *TeamSession {
	t.Helper()
	session, err := LoadTeam(hufuCodingTeamDir(t), nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam(hufu-coding): %v", err)
	}
	return session
}

// TestHufuCodingTeamLoadsWithoutContractFindings pins spec.md §16 Phase 1's
// acceptance criterion ("hufu team validate --team hufu-coding passes") as a
// regression: a future edit to team.yaml or an agent's frontmatter that
// breaks a static contract must fail this test, not just a manual CLI run.
func TestHufuCodingTeamLoadsWithoutContractFindings(t *testing.T) {
	session := loadHufuCodingTeam(t)
	findings := ValidateTeamTaskContracts(session)
	for _, f := range findings {
		if f.Severity == FindingSeverityError {
			t.Fatalf("hufu-coding has an invalid static task contract: %s: %s", f.Field, f.Message)
		}
	}
	for _, name := range []string{"sa", "coder", "verifier", "reviewer", "final-sa"} {
		if session.Agents[name] == nil {
			t.Fatalf("hufu-coding is missing the %q worker", name)
		}
	}
}

// codexCommandDisablesNativeMultiAgent reports whether a codex-app-server
// command line passes both overrides spec.md §6 requires. Codex's
// features.multi_agent_v2 can take precedence over the legacy
// [agents].enabled, so both must be forced off — neither alone is
// sufficient. Exported as a plain function (not a t.Helper closure) so the
// opt-in live smoke suite (spec.md §16 Phase 5) can reuse the exact same
// check against the real installed Codex CLI's effective config, rather than
// re-deriving the two flag strings a second time.
func codexCommandDisablesNativeMultiAgent(cmd []string) (agentsDisabled, multiAgentV2Disabled bool) {
	joined := strings.Join(cmd, " ")
	agentsDisabled = strings.Contains(joined, "agents.enabled=false")
	multiAgentV2Disabled = strings.Contains(joined, "features.multi_agent_v2=false")
	return agentsDisabled, multiAgentV2Disabled
}

// TestHufuCodingCodexProviderDisablesNativeMultiAgent is the config-lint half
// of matrix item I (spec.md §6/§16 Phase 1 acceptance, "Codex command
// disables native multi-agent"). It proves Hufu's own shipped configuration
// is correct; it cannot observe whether the real Codex CLI actually honors
// these flags — that behavioral half is the opt-in HUFU_CODEX_SMOKE suite
// (spec.md §16 Phase 5), which must exercise both legacy [agents].enabled
// and features.multi_agent_v2 precedence per test-matrix item I so enabling
// one cannot silently override the other.
func TestHufuCodingCodexProviderDisablesNativeMultiAgent(t *testing.T) {
	session := loadHufuCodingTeam(t)
	provider, ok := session.Config.SubagentProviders["codex"]
	if !ok {
		t.Fatal("hufu-coding does not configure a \"codex\" subagent-provider")
	}
	agentsDisabled, multiAgentV2Disabled := codexCommandDisablesNativeMultiAgent(provider.Command)
	if !agentsDisabled {
		t.Fatalf("codex command does not disable legacy [agents].enabled: %v", provider.Command)
	}
	if !multiAgentV2Disabled {
		t.Fatalf("codex command does not disable features.multi_agent_v2 (which can override [agents].enabled): %v", provider.Command)
	}
}

// TestHufuCodingCoderIsBoundToCodex pins spec.md §5.3/§16 Phase 1 acceptance
// ("coder is configuration-bound to Codex external provider").
func TestHufuCodingCoderIsBoundToCodex(t *testing.T) {
	session := loadHufuCodingTeam(t)
	coder := session.Agents["coder"]
	if coder == nil {
		t.Fatal("hufu-coding is missing the coder worker")
	}
	if coder.SubagentProvider != "codex" {
		t.Fatalf("coder.SubagentProvider = %q, want \"codex\"", coder.SubagentProvider)
	}
}

// TestHufuCodingReadOnlyRolesCannotWrite is the static half of matrix item M
// ("SA/reviewer/final-SA attempts to write must fail closed"): the
// frontmatter tool grant itself must never include a write-capable tool for
// these three roles, or the runtime's tool-policy enforcement never gets a
// chance to run at all.
func TestHufuCodingReadOnlyRolesCannotWrite(t *testing.T) {
	session := loadHufuCodingTeam(t)
	forbidden := []string{"edit", "write", "multiedit"}
	for _, name := range []string{"sa", "reviewer", "final-sa"} {
		def := session.Agents[name]
		if def == nil {
			t.Fatalf("hufu-coding is missing the %q worker", name)
		}
		for _, tool := range hufuCodingAgentToolNames(def) {
			for _, bad := range forbidden {
				if tool == bad {
					t.Fatalf("read-only role %q grants write-capable tool %q", name, tool)
				}
			}
		}
	}
}

// TestHufuCodingVerifierCannotWrite proves the verifier's bash access does
// not also come with a direct file-write tool — its side effect is
// classified none in team.yaml precisely because it must only run commands,
// never edit source, per spec.md §5.4.
func TestHufuCodingVerifierCannotWrite(t *testing.T) {
	session := loadHufuCodingTeam(t)
	def := session.Agents["verifier"]
	if def == nil {
		t.Fatal("hufu-coding is missing the verifier worker")
	}
	for _, tool := range hufuCodingAgentToolNames(def) {
		if tool == "edit" || tool == "write" || tool == "multiedit" {
			t.Fatalf("verifier grants write-capable tool %q", tool)
		}
	}
}

// hufuCodingAgentToolNames splits AgentDef.Tools' comma-joined frontmatter
// value into a plain slice for the read-only assertions above. It is
// distinct from coordinator_task_run.go's agentToolNames, which normalizes
// resolved fantasy.AgentTool instances rather than the raw frontmatter field.
func hufuCodingAgentToolNames(def *agent.AgentDef) []string {
	var names []string
	for _, name := range strings.Split(def.Tools, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// TestHufuCodingOnFailureClassesFreezeSemanticRejectionOnly pins spec.md
// §10.2 as it actually ships: verifier/reviewer/final-sa's static contracts
// restrict on_failure to a genuine semantic verification rejection, and the
// coder contract carries no such restriction (it is only ever a reset
// target in this batch, never itself an on_failure source).
func TestHufuCodingOnFailureClassesFreezeSemanticRejectionOnly(t *testing.T) {
	session := loadHufuCodingTeam(t)
	byAgent := map[string]TaskDef{}
	for _, task := range session.ContractTasks {
		byAgent[strings.ToLower(strings.TrimSpace(task.Agent))] = task
	}
	for _, name := range []string{"verifier", "reviewer", "final-sa"} {
		task, ok := byAgent[name]
		if !ok {
			t.Fatalf("hufu-coding has no static task contract for %q", name)
		}
		if len(task.OnFailureClasses) != 1 || task.OnFailureClasses[0] != FailureVerify {
			t.Fatalf("%s.on-failure-classes = %v, want [verification] so only a genuine semantic rejection resets the coder", name, task.OnFailureClasses)
		}
	}
	coder, ok := byAgent["coder"]
	if !ok {
		t.Fatal("hufu-coding has no static task contract for \"coder\"")
	}
	if coder.SideEffect != SideEffectWorkspaceWrite {
		t.Fatalf("coder.side_effect = %q, want %q", coder.SideEffect, SideEffectWorkspaceWrite)
	}
}

// hufuCodingReviewerVerifySpec is exactly the verify-spec
// .agent-teams/hufu-coding/team.yaml binds to the reviewer's static
// contract, loaded from the real file so a future edit to the shipped YAML
// is what these two tests actually exercise, not a hand-copied duplicate.
func hufuCodingReviewerVerifySpec(t *testing.T) VerificationSpec {
	t.Helper()
	session := loadHufuCodingTeam(t)
	for _, task := range session.ContractTasks {
		if strings.EqualFold(strings.TrimSpace(task.Agent), "reviewer") {
			if task.VerifySpec == nil {
				t.Fatal("hufu-coding reviewer contract has no verify-spec")
			}
			return *task.VerifySpec
		}
	}
	t.Fatal("hufu-coding has no static task contract for \"reviewer\"")
	return VerificationSpec{}
}

// TestHufuCodingReviewerCompletedWithGapsPassesVerifySpec is matrix item J
// ("evidence gap is not coder failure") exercised against the real
// verify-spec mechanism (executeTaskResultAssertVerification), not just the
// DAG-level simulation in hufu_coding_workflow_test.go: a reviewer that
// honestly reports completed_with_gaps with no confirmed must-fix finding
// passes the same verify-spec team.yaml ships, so the task reaches TaskDone
// exactly like success — no on_failure edge is ever consulted, let alone
// fires, for an evidence gap.
func TestHufuCodingReviewerCompletedWithGapsPassesVerifySpec(t *testing.T) {
	spec := hufuCodingReviewerVerifySpec(t)
	result := &TaskResult{
		Status: TaskResultStatusCompletedWithGaps, Summary: "one cited file could not be read",
		Facts: map[string]any{"must_fix_found": false},
	}
	vr, err := executeTaskResultAssertVerification(t.TempDir(), spec, result)
	if err != nil {
		t.Fatalf("expected completed_with_gaps + must_fix_found:false to pass the reviewer's verify-spec, got err=%v vr=%#v", err, vr)
	}
	if vr == nil || vr.ExitCode != 0 {
		t.Fatalf("verification result = %#v, want ExitCode 0", vr)
	}
}

// TestHufuCodingReviewerMustFixFindingFailsVerifySpecAsVerification proves
// the contrasting case classifies as a genuine (FailureVerify-eligible)
// rejection: must_fix_found:true fails the same verify-spec.
func TestHufuCodingReviewerMustFixFindingFailsVerifySpecAsVerification(t *testing.T) {
	spec := hufuCodingReviewerVerifySpec(t)
	result := &TaskResult{
		Status: TaskResultStatusFailed, Summary: "found a concrete bug",
		Facts: map[string]any{"must_fix_found": true},
	}
	vr, err := executeTaskResultAssertVerification(t.TempDir(), spec, result)
	if err == nil {
		t.Fatal("expected must_fix_found:true to fail the reviewer's verify-spec")
	}
	if vr == nil || vr.ExitCode == 0 {
		t.Fatalf("verification result = %#v, want a non-zero ExitCode", vr)
	}
}
