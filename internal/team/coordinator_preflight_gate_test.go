package team

import (
	"context"
	"testing"

	"charm.land/fantasy"
)

// TestCoordinatorPreflightRestoresGatedToolsOnLaterSteps guards E-25: the
// preflight is built from the raw orchestrator tools, and on every step after
// the first it hands its tool set back to Fantasy. Without bindTools that set
// is the raw one, so later coordinator calls skip the policy gate and the
// schema-repair wrapper.
func TestCoordinatorPreflightRestoresGatedToolsOnLaterSteps(t *testing.T) {
	raw := []fantasy.AgentTool{
		fantasy.NewAgentTool("agent", "delegate", func(context.Context, coordinatorPreflightTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("ok"), nil
		}),
		fantasy.NewAgentTool("finish", "finish", func(context.Context, coordinatorPreflightTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("ok"), nil
		}),
	}
	c := gateTestCoordinator()
	gated := c.wrapWithProtocolRepair(c.gatePolicyTools(raw))

	preflight := newCoordinatorRequestPreflightWithWindow("preflight-gate-model", "prompt", "system", raw, 1_000_000)
	preflight.bindTools(gated)

	for _, step := range []int{1, 2, 5} {
		_, tools, applied, err := preflight.prepare(context.Background(), nil, "prompt", 256, step)
		if err != nil {
			t.Fatalf("step %d: prepare() error = %v", step, err)
		}
		if !applied || len(tools) != len(raw) {
			t.Fatalf("step %d: prepare() = applied %v with %d tools, want the full restored set", step, applied, len(tools))
		}
		for _, tool := range tools {
			wrapper, ok := tool.(*protocolRepairWrapper)
			if !ok {
				t.Fatalf("step %d: tool %q is %T, want the protocol-repair wrapper", step, tool.Info().Name, tool)
			}
			if _, ok := wrapper.base.(*policyGatedTool); !ok {
				t.Fatalf("step %d: tool %q wraps %T, want the policy gate", step, tool.Info().Name, wrapper.base)
			}
		}
	}
	_, configured := preflight.configuration()
	if _, ok := configured[0].(*protocolRepairWrapper); !ok {
		t.Fatalf("configuration() tool = %T, want the gated set used by model downshift", configured[0])
	}
}

func TestCoordinatorPreflightBindToolsIgnoresEmptySet(t *testing.T) {
	raw := []fantasy.AgentTool{fantasy.NewAgentTool("agent", "delegate", func(context.Context, coordinatorPreflightTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})}
	preflight := newCoordinatorRequestPreflightWithWindow("preflight-gate-model", "prompt", "system", raw, 1_000_000)
	preflight.bindTools(nil)
	if _, tools := preflight.configuration(); len(tools) != 1 {
		t.Fatalf("bindTools(nil) replaced the tool set: %d tools", len(tools))
	}
}
