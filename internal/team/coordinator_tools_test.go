package team

import (
	"context"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestDelegationChain(t *testing.T) {
	tests := []struct {
		name       string
		ctxChain   string
		callerName string
		want       []string
	}{
		{"root call seeds chain from caller", "", "planner", []string{"planner"}},
		{"root call with no caller yields empty chain", "", "", nil},
		{"propagated chain from context wins over caller", "A/B", "B", []string{"A", "B"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.ctxChain != "" {
				ctx = context.WithValue(ctx, delegationChainKey{}, tt.ctxChain)
			}
			got := delegationChain(ctx, tt.callerName)
			if strings.Join(got, "/") != strings.Join(tt.want, "/") {
				t.Errorf("delegationChain() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRunAgentsDescriptionForbidsSuccessfulRedispatch(t *testing.T) {
	tool := &runAgentsTool{coordinator: &Coordinator{
		session: &TeamSession{Agents: map[string]*agent.AgentDef{
			"worker": {Name: "worker"},
		}},
	}}
	description := tool.Info().Description
	for _, want := range []string{"Never redispatch", "team_info", "task_result"} {
		if !strings.Contains(description, want) {
			t.Fatalf("agent tool description missing %q: %s", want, description)
		}
	}
}

func TestRunAgentsToolInfoPinsFreshInitialDelegationSchema(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{Config: agent.TeamConfig{
			Delegation: agent.DelegationPolicy{
				InitialBatch:             []string{"surface", "reader"},
				RequireExactInitialBatch: true,
				BindInitialTaskContracts: true,
			},
		}, Agents: map[string]*agent.AgentDef{
			"surface": {Name: "surface", Role: "worker"},
			"reader":  {Name: "reader", Role: "worker"},
			"planner": {Name: "planner", Role: "worker"},
		}},
		taskTracker: NewTaskTracker(),
		sessionData: NewSession(),
	}

	info := (&runAgentsTool{coordinator: c}).Info()
	tasks, ok := info.Parameters["tasks"].(map[string]any)
	if !ok {
		t.Fatalf("tasks schema = %#v, want object", info.Parameters["tasks"])
	}
	if got := tasks["minItems"]; got != 2 {
		t.Fatalf("initial minItems = %#v, want 2", got)
	}
	if got := tasks["maxItems"]; got != 2 {
		t.Fatalf("initial maxItems = %#v, want 2", got)
	}
	items, ok := tasks["items"].(map[string]any)
	if !ok {
		t.Fatalf("task item schema = %#v, want object", tasks["items"])
	}
	properties, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("task properties = %#v, want object", items["properties"])
	}
	agentSchema, ok := properties["agent"].(map[string]any)
	if !ok {
		t.Fatalf("agent schema = %#v, want object", properties["agent"])
	}
	agents, ok := agentSchema["enum"].([]string)
	if !ok || strings.Join(agents, ",") != "surface,reader" {
		t.Fatalf("fresh agent enum = %#v, want only ordered initial workers", agentSchema["enum"])
	}
	for _, forbidden := range []string{"execution", "output_mode", "context_files"} {
		if _, exists := properties[forbidden]; exists {
			t.Fatalf("fresh initial task schema exposed runtime-bound field %q", forbidden)
		}
	}
	prefix, ok := tasks["prefixItems"].([]map[string]any)
	if !ok || len(prefix) != 2 {
		t.Fatalf("prefixItems = %#v, want ordered schemas for both initial workers", tasks["prefixItems"])
	}
	if !strings.Contains(info.Description, "initial_pending") {
		t.Fatalf("fresh initial agent tool description omitted canonical phase: %q", info.Description)
	}
}

func TestCheckDelegationLimits(t *testing.T) {
	tests := []struct {
		name     string
		chain    []string
		selected string
		wantErr  bool
	}{
		{"first hop is fine", nil, "A", false},
		{"distinct chain under the depth cap is fine", []string{"A", "B", "C"}, "D", false},
		{"direct self-delegation is circular", []string{"A"}, "A", true},
		{"case-insensitive self-delegation is circular", []string{"A"}, "a", true},
		{"re-entering an ancestor is circular", []string{"A", "B"}, "A", true},
		{"chain at the depth cap is blocked", []string{"A", "B", "C", "D", "E"}, "F", true},
		{"chain just under the depth cap is allowed", []string{"A", "B", "C", "D"}, "E", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkDelegationLimits(tt.chain, tt.selected)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkDelegationLimits(%v, %q) error = %v, wantErr %v", tt.chain, tt.selected, err, tt.wantErr)
			}
		})
	}
}

// TestDelegationChainPropagatesAcrossHops verifies the fix for the bug where
// the chain used to reset to a single agent name on every nested
// request_agent call (because it was read from the coordinator's mutable
// snapshot, which only ever holds the immediate agent's flat name). It must
// instead accumulate across hops so depth and cycle checks are meaningful.
func TestDelegationChainPropagatesAcrossHops(t *testing.T) {
	// Hop 1: top-level agent "A" delegates to "B".
	ctx := context.Background()
	chain := delegationChain(ctx, "A")
	if err := checkDelegationLimits(chain, "B"); err != nil {
		t.Fatalf("A -> B should be allowed: %v", err)
	}
	subLabel := strings.Join(append(chain, "B"), "/")
	if subLabel != "A/B" {
		t.Fatalf("subLabel = %q, want %q", subLabel, "A/B")
	}

	// Hop 2: "B" (now carrying the propagated chain) tries to delegate back
	// to "A". This must be caught as a cycle.
	ctx2 := context.WithValue(context.Background(), delegationChainKey{}, subLabel)
	chain2 := delegationChain(ctx2, "B")
	if strings.Join(chain2, "/") != "A/B" {
		t.Fatalf("propagated chain = %v, want [A B]", chain2)
	}
	if err := checkDelegationLimits(chain2, "A"); err == nil {
		t.Fatal("expected B -> A to be blocked as a circular delegation, got nil error")
	}
}
