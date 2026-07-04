package team

import (
	"context"
	"strings"
	"testing"
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
