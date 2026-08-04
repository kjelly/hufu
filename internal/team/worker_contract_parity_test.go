package team

import (
	"strings"
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
)

// TestResultProtocolInstructionsStateTheContract covers the prompt half of the
// worker result protocol. ExecuteTasks marks submit_result mandatory for every
// non-sidecar task, so a worker that is never told about it fails the contract
// by writing an ordinary prose summary — and that failure is indistinguishable
// from genuine non-completion.
func TestResultProtocolInstructionsStateTheContract(t *testing.T) {
	granted := map[string]bool{"submit_result": true}

	tests := []struct {
		name    string
		task    TaskDef
		granted map[string]bool
		want    bool
	}{
		{
			name:    "required and granted",
			task:    TaskDef{Execution: ExecutionContract{RequiresResult: true}},
			granted: granted,
			want:    true,
		},
		{
			name:    "not required",
			task:    TaskDef{},
			granted: granted,
			want:    false,
		},
		{
			name:    "required but tool not granted",
			task:    TaskDef{Execution: ExecutionContract{RequiresResult: true}},
			granted: map[string]bool{},
			want:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resultProtocolInstructions(tc.task, tc.granted)
			if mentions := strings.Contains(got, "submit_result"); mentions != tc.want {
				t.Fatalf("mentions submit_result = %t, want %t (got %q)", mentions, tc.want, got)
			}
			if !tc.want {
				return
			}
			// The instruction is only useful if it says a prose ending fails and
			// that a truthful non-success status is preferred over a false one.
			for _, needle := range []string{"not complete until", "partial"} {
				if !strings.Contains(got, needle) {
					t.Errorf("result protocol instructions missing %q: %q", needle, got)
				}
			}
		})
	}
}

// TestSharedKnowledgeInstructionsOnlyNameGrantedTools is the prompt-side twin of
// the allowlist invariant. The worker prompt used to instruct every worker to
// call stm_write, which the default team never grants; obeying the instruction
// aborted the attempt at the stream gate.
func TestSharedKnowledgeInstructionsOnlyNameGrantedTools(t *testing.T) {
	c := &Coordinator{session: &TeamSession{
		Config:    agent.TeamConfig{Name: "team"},
		Workspace: t.TempDir(),
	}}

	tests := []struct {
		name        string
		granted     map[string]bool
		wantStmTool bool
	}{
		{name: "stm_write granted", granted: map[string]bool{"stm_write": true}, wantStmTool: true},
		{name: "only file tools", granted: map[string]bool{"write": true}, wantStmTool: false},
		{name: "read only", granted: map[string]bool{"view": true}, wantStmTool: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := c.sharedKnowledgeInstructions(tc.granted)
			if strings.TrimSpace(got) == "" {
				t.Fatal("shared knowledge instructions should never be empty")
			}
			if mentions := strings.Contains(got, "`stm_write`"); mentions != tc.wantStmTool {
				t.Fatalf("instructs stm_write = %t, want %t (got %q)", mentions, tc.wantStmTool, got)
			}
		})
	}
}

// TestStepBudgetHonoursOverrides pins the precedence the hardcoded
// AgentConfig.MaxSteps call sites used to bypass. CreateAgent prefers an
// explicit AgentConfig.MaxSteps over resolveMaxSteps, so passing a non-zero
// constant made --max-steps and team.yaml max-steps silently ineffective.
func TestStepBudgetHonoursOverrides(t *testing.T) {
	tests := []struct {
		name       string
		agentSteps int
		teamSteps  int
		fallback   int
		want       int
	}{
		{name: "agent wins", agentSteps: 7, teamSteps: 50, fallback: 30, want: 7},
		{name: "team wins over fallback", teamSteps: 50, fallback: 30, want: 50},
		{name: "fallback when unset", fallback: 30, want: 30},
		{name: "role-specific fallback preserved", fallback: agent.DefaultCoordinatorMaxSteps, want: agent.DefaultCoordinatorMaxSteps},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{MaxSteps: tc.teamSteps}}}
			def := &agent.AgentDef{Name: "worker", MaxSteps: tc.agentSteps}
			if got := c.stepBudget(def, tc.fallback); got != tc.want {
				t.Fatalf("stepBudget = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestDefaultTeamStepBudget checks the --default path a runbook run actually
// uses: workers get headroom for multi-step infrastructure work while
// orchestration turns keep their own, narrower budget.
func TestDefaultTeamStepBudget(t *testing.T) {
	session, err := LoadDefaultTeam(t.TempDir(), nil, "bash,terminal")
	if err != nil {
		t.Fatalf("LoadDefaultTeam: %v", err)
	}
	c := &Coordinator{session: session}

	worker := c.stepBudget(session.Agents["helper"], agent.DefaultMaxSteps)
	if worker <= agent.DefaultMaxSteps {
		t.Errorf("default-team worker step budget = %d, want more than DefaultMaxSteps (%d)", worker, agent.DefaultMaxSteps)
	}
	if coord := c.stepBudget(session.Agents["coordinator"], agent.DefaultCoordinatorMaxSteps); coord != agent.DefaultCoordinatorMaxSteps {
		t.Errorf("default-team coordinator step budget = %d, want %d", coord, agent.DefaultCoordinatorMaxSteps)
	}
}

// TestLocalFailureHintForBudgetExhaustion guards against the hint that turned
// truncated-but-progressing tasks into thrashing loops. A worker cut off by the
// step budget carries its prior conversation into the retry, so it must be told
// to resume — not to change an approach that was never wrong.
func TestLocalFailureHintForBudgetExhaustion(t *testing.T) {
	hint := localFailureHint("protocol incomplete: step budget exhausted (30/30 steps) before submit_result")

	for _, needle := range []string{"continue", "cut off"} {
		if !strings.Contains(strings.ToLower(hint), needle) {
			t.Errorf("budget-exhaustion hint missing %q: %q", needle, hint)
		}
	}
	if strings.Contains(hint, "Change your approach") {
		t.Errorf("budget-exhaustion hint must not tell a truncated worker to change approach: %q", hint)
	}
}
