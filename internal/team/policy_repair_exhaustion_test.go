package team

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

// failingCoordinatorTool returns a fixed response together with a Go error,
// the shape runAgentsTool.Run produces once the policy-repair budget is spent.
type failingCoordinatorTool struct {
	recordingTool
	err error
}

func (t *failingCoordinatorTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	t.ran = true
	return t.resp, t.err
}

func exhaustedRepairAgentTool() *failingCoordinatorTool {
	return &failingCoordinatorTool{
		recordingTool: recordingTool{
			name: "agent",
			resp: fantasy.NewTextErrorResponse(coordinatorPolicyRepairExhaustedPrefix + " delegation policy repair budget exhausted"),
		},
		err: errCoordinatorPolicyRepairExhausted,
	}
}

func coordinatorToolContext(allowed ...string) context.Context {
	ctx := tools.SetToolsAllowed(context.Background(), allowed)
	return context.WithValue(ctx, todoIDKey{}, CoordTodoID)
}

func TestPolicyGateKeepsRepairExhaustionSentinel(t *testing.T) {
	cases := []struct {
		name     string
		innerErr error
		want     []error
		notWant  []error
	}{
		{
			name:     "exhausted repair keeps both sentinels",
			innerErr: errCoordinatorPolicyRepairExhausted,
			want:     []error{errCoordinatorToolFailure, errCoordinatorPolicyRepairExhausted},
		},
		{
			name:     "ordinary tool error stays a plain coordinator failure",
			innerErr: errors.New("provider unavailable"),
			want:     []error{errCoordinatorToolFailure},
			notWant:  []error{errCoordinatorPolicyRepairExhausted},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := gateTestCoordinator()
			c.taskTracker = NewTaskTracker()
			inner := exhaustedRepairAgentTool()
			inner.err = tc.innerErr
			gated := c.gatePolicyTools([]fantasy.AgentTool{inner})[0]

			_, err := gated.Run(coordinatorToolContext("agent"), fantasy.ToolCall{ID: "exhausted", Name: "agent"})
			for _, sentinel := range tc.want {
				if !errors.Is(err, sentinel) {
					t.Errorf("gate error = %v, want errors.Is(%v)", err, sentinel)
				}
			}
			for _, sentinel := range tc.notWant {
				if errors.Is(err, sentinel) {
					t.Errorf("gate error = %v, must not match %v", err, sentinel)
				}
			}
		})
	}
}

// TestRepairExhaustionThroughGateFinalizesWithoutModelTurn follows the real
// error chain: the gated coordinator agent tool fails with the exhausted
// sentinel, fantasy returns that tool error unchanged from the stream, and
// attemptWrapUpRecovery must take the LLM-free policy-repair summary instead of
// treating the run as a coordinator tool failure.
func TestRepairExhaustionThroughGateFinalizesWithoutModelTurn(t *testing.T) {
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "completed work"}})[0]
	if err := tracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskDone, "done", "authoritative worker result"); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		session:      &TeamSession{Config: agent.TeamConfig{Name: "repair-exhaustion"}},
		sessionData:  NewSession(),
		taskTracker:  tracker,
		reportStatus: func(StatusEvent) {},
	}
	for i := 0; i <= maxCoordinatorPolicyRepairs; i++ {
		c.coordinatorPolicyRepairPrompt(&delegationPolicyViolation{message: "worker outside allowlist"})
	}
	c.runOrchestratorOverride = func(context.Context, *agent.AgentDef, string) (string, []fantasy.StepResult, error) {
		t.Fatal("policy repair exhaustion must not invoke a wrap-up model turn")
		return "", nil, nil
	}

	gated := c.gatePolicyTools([]fantasy.AgentTool{exhaustedRepairAgentTool()})[0]
	_, runErr := gated.Run(coordinatorToolContext("agent"), fantasy.ToolCall{ID: "exhausted", Name: "agent"})
	if runErr == nil {
		t.Fatal("exhausted delegation must stop the coordinator stream")
	}

	result, steps, recovered := c.attemptWrapUpRecovery(context.Background(), &agent.AgentDef{Name: "coordinator"}, runErr)
	if !recovered {
		t.Fatalf("attemptWrapUpRecovery recovered=false for %v; want the LLM-free policy repair summary", runErr)
	}
	if len(steps) != 0 {
		t.Fatalf("recovery spent %d model steps, want 0", len(steps))
	}
	if !strings.Contains(result, "authoritative worker result") {
		t.Fatalf("recovery result = %q, want the deterministic Todo evidence", result)
	}
	if run := c.LastRunResult(); run == nil {
		t.Fatal("policy repair exhaustion must record a terminal run result")
	}
}
