package evalharness

import (
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestEvalEventAssertionOrdering(t *testing.T) {
	events := []team.StatusEvent{
		{Type: "start", Agent: "coordinator"},
		{Type: "step"},
		{Type: "todos_updated"},
		{Type: "start", Agent: "worker"},
		{Type: "done", Agent: "worker"},
		{Type: "done", Agent: "coordinator"},
	}

	if findings := assertEventOrder([]string{"start", "todos_updated", "done"}, events); len(findings) != 0 {
		t.Errorf("expected in-order subsequence to pass, got findings: %+v", findings)
	}
	if findings := assertEventOrder(nil, events); len(findings) != 0 {
		t.Errorf("empty order should never produce findings, got: %+v", findings)
	}
	if findings := assertEventOrder([]string{"done", "start"}, events); len(findings) == 0 {
		t.Error("expected reversed order to fail, got no findings")
	}
	if findings := assertEventOrder([]string{"start", "budget_exceeded"}, events); len(findings) == 0 {
		t.Error("expected a never-observed event in the sequence to fail, got no findings")
	}
}

func TestEvalNormalizesOpaqueIDs(t *testing.T) {
	first := "run-20260912T093923.493930048Z-97f23d1c6c58"
	second := "run-20260101T000000.000000000Z-deadbeef0000"
	if got := normalizeOpaqueID(first); got != normalizeOpaqueID(second) {
		t.Errorf("two distinct run IDs normalized to different strings: %q vs %q", normalizeOpaqueID(first), normalizeOpaqueID(second))
	}
	embedded := "run ended before completion (" + first + ")"
	if got := normalizeOpaqueID(embedded); got == embedded {
		t.Errorf("normalizeOpaqueID left the run id untouched in %q", embedded)
	}
	plain := "no opaque id in this string"
	if got := normalizeOpaqueID(plain); got != plain {
		t.Errorf("normalizeOpaqueID modified a string with no run id: got %q, want unchanged %q", got, plain)
	}
}
