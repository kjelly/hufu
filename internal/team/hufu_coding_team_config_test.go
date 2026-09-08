package team

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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

// TestHufuCodingExecuteTasksAdmitsBatchWithoutContractMismatch drives the
// REAL production dispatch entrypoint (Coordinator.ExecuteTasks), not a
// direct newDAGScheduler construction like every other test in this package
// — the gap a prior review correctly identified: OnFailureClasses must
// survive the durable TodoSpec/TodoItem round trip
// (task_occurrence_projection.go's compareTaskDefWithTodoOccurrence rebuilds
// the scheduler's TaskDef from the durable TodoItem via taskDefFromTodoItem
// and reflect.DeepEqual-rejects the entire batch on any mismatch), or a real
// hufu-coding run never reaches a dagScheduler at all. This test lets
// bindTaskGoalContracts derive OnFailureClasses from the real team.yaml
// contract by goal-text match (exactly as coordinator.md instructs the
// coordinator to phrase goals) rather than setting the field directly, so it
// exercises the identical path production uses. A short-timeout context
// bounds real (and here, expected-to-fail: no live model/Codex configured)
// worker execution — this test only cares that admission itself does not
// reject the batch over OnFailureClasses before a single worker is dispatched.
func TestHufuCodingExecuteTasksAdmitsBatchWithoutContractMismatch(t *testing.T) {
	session := loadHufuCodingTeam(t)
	workspace := t.TempDir()
	session.Workspace = workspace
	session.Dir = workspace
	store, err := NewEventStore(workspace, "hufu-coding-admission-test", "hufu-coding-admission-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c := &Coordinator{
		session: session, projectDir: workspace,
		taskTracker: NewTaskTracker(), sessionData: NewSession(),
		eventStore: store, executionRunID: "hufu-coding-admission-test",
		reportStatus:    func(StatusEvent) {},
		delegatedTasks:  make(map[string]int),
		taskResultCache: make(map[string][]cachedTaskEntry),
		maxConcurrent:   1,
	}
	tasks := []TaskDef{
		{Agent: "sa", Goal: "SA_ANALYZE: implement a small safe test change"},
		{Agent: "coder", Goal: "CODER_IMPLEMENT: implement the change", DependsOn: []int{0}},
		{Agent: "verifier", Goal: "VERIFY_IMPLEMENTATION: run required checks", DependsOn: []int{0, 1}, OnFailure: intPtr(1), MaxRetries: 4},
		{Agent: "reviewer", Goal: "REVIEW_CODE: review the change", DependsOn: []int{0, 1, 2}, OnFailure: intPtr(1), MaxRetries: 4},
		{Agent: "final-sa", Goal: "FINAL_SA_GATE: accept or reject the result", DependsOn: []int{0, 1, 2, 3}, OnFailure: intPtr(1), MaxRetries: 2},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.ExecuteTasks(ctx, tasks); err != nil && strings.Contains(err.Error(), "OnFailureClasses") {
		t.Fatalf("ExecuteTasks rejected the hufu-coding batch on admission due to an OnFailureClasses mismatch between the supplied TaskDef and the durable Todo occurrence: %v", err)
	}
	// Confirm the static contract actually bound OnFailureClasses onto the
	// durable occurrence, so a passing test above isn't just "nothing ran".
	items := c.taskTracker.TodoList().Items()
	if len(items) != 5 {
		t.Fatalf("want 5 durable Todo occurrences admitted, got %d", len(items))
	}
	for _, idx := range []int{2, 3, 4} {
		if len(items[idx].OnFailureClasses) != 1 || items[idx].OnFailureClasses[0] != FailureVerify {
			t.Fatalf("items[%d] (%s) OnFailureClasses = %v, want [verification] bound from the static contract", idx, items[idx].Agent, items[idx].OnFailureClasses)
		}
	}
}

// TestHufuCodingVerifierReceivesSAResult and
// TestHufuCodingFinalSAReceivesAllUpstreamResults are the direct regression
// for a prior review finding: coordinator.md's batch used to list only each
// task's immediately preceding index in depends_on, but
// Coordinator.dependencyResultsForTask (coordinator_task_run.go) only ever
// exposes a task's *direct* dependencies' results — never a transitive walk
// — to its compiled prompt. With the widened depends_on lists coordinator.md
// now instructs ([0,1] for verifier, [0,1,2,3] for final-sa), every
// downstream task actually receives every upstream typed result its prompt
// assumes is present.
func TestHufuCodingVerifierReceivesSAResult(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "sa", Desc: "SA_ANALYZE"},
		{Agent: "coder", Desc: "CODER_IMPLEMENT"},
		{Agent: "verifier", Desc: "VERIFY_IMPLEMENTATION"},
	})
	items[2].DependsOn = []string{items[0].ID, items[1].ID}
	for i, summary := range []string{"sa contract", "coder change"} {
		c.taskTracker.TodoList().UpdateStatus(items[i].ID, TaskDone, "done")
		c.storeSubmittedTaskResult(items[i].ID, &TaskResult{TaskID: items[i].ID, Status: "success", Summary: summary, Source: "submitted"})
	}

	got := c.dependencyResultsForTask(items[2].ID)
	if len(got) != 2 {
		t.Fatalf("verifier's dependency results = %d, want 2 (sa, coder): %+v", len(got), got)
	}
	summaries := map[string]bool{}
	for _, r := range got {
		summaries[r.Summary] = true
	}
	if !summaries["sa contract"] {
		t.Fatalf("verifier did not receive SA's result: %+v", got)
	}
	if !summaries["coder change"] {
		t.Fatalf("verifier did not receive coder's result: %+v", got)
	}
}

func TestHufuCodingFinalSAReceivesAllUpstreamResults(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "sa", Desc: "SA_ANALYZE"},
		{Agent: "coder", Desc: "CODER_IMPLEMENT"},
		{Agent: "verifier", Desc: "VERIFY_IMPLEMENTATION"},
		{Agent: "reviewer", Desc: "REVIEW_CODE"},
		{Agent: "final-sa", Desc: "FINAL_SA_GATE"},
	})
	items[4].DependsOn = []string{items[0].ID, items[1].ID, items[2].ID, items[3].ID}
	summaries := []string{"sa contract", "coder change", "verifier evidence", "reviewer result"}
	for i, summary := range summaries {
		c.taskTracker.TodoList().UpdateStatus(items[i].ID, TaskDone, "done")
		c.storeSubmittedTaskResult(items[i].ID, &TaskResult{TaskID: items[i].ID, Status: "success", Summary: summary, Source: "submitted"})
	}

	got := c.dependencyResultsForTask(items[4].ID)
	if len(got) != 4 {
		t.Fatalf("final-sa's dependency results = %d, want 4 (sa, coder, verifier, reviewer): %+v", len(got), got)
	}
	gotSummaries := map[string]bool{}
	for _, r := range got {
		gotSummaries[r.Summary] = true
	}
	for _, want := range summaries {
		if !gotSummaries[want] {
			t.Fatalf("final-sa did not receive result summary %q: %+v", want, got)
		}
	}
}

// TestHufuCodingNoVerifySpecOnSemanticRoles pins the fix for a prior review
// finding: verifier/reviewer/final-sa deliberately carry no verify-spec at
// all. A task_result_assert verify-spec enforces at submit_result admission
// time (task_result_contract.go) and would reject the tool call itself for
// an honest, confirmed `status: failed` finding — making a true positive
// literally un-submittable. The semantic verdict is carried by `status`
// alone; see TestHufuCodingGenuineFailedReportAuthorizesReset and
// TestHufuCodingInfraFailureWithNoStoredResultDoesNotAuthorizeReset in
// hufu_coding_classification_test.go for the runtime mechanism that reads it.
func TestHufuCodingNoVerifySpecOnSemanticRoles(t *testing.T) {
	session := loadHufuCodingTeam(t)
	for _, task := range session.ContractTasks {
		name := strings.ToLower(strings.TrimSpace(task.Agent))
		if name == "verifier" || name == "reviewer" || name == "final-sa" {
			if task.VerifySpec != nil {
				t.Fatalf("%s has a verify-spec %+v, want none (see comment above)", name, task.VerifySpec)
			}
		}
	}
}
