package paired

import (
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestVerifierAddressesCanonicalPayloadValue(t *testing.T) {
	fixture := Verifier{Type: "task_result_assert", Mode: "blocking", Assertions: []agent.TaskResultAssertion{
		{Pointer: "/structured_payload/category", Op: "equals", Value: "allowed"},
		{Pointer: "/structured_payload", Op: "non_empty"},
		{Pointer: "/structured_payload/a~1b/~0", Op: "exists"},
	}}
	spec := fixture.RuntimeSpec()
	if spec.Mode != "success" {
		t.Fatal("blocking fixture did not use success verifier")
	}
	for i, want := range []string{"/structured_payload/value/category", "/structured_payload/value", "/structured_payload/value/a~1b/~0"} {
		if spec.TaskResultAssertions[i].Pointer != want {
			t.Fatalf("pointer %d: %s", i, spec.TaskResultAssertions[i].Pointer)
		}
	}
	if fixture.Assertions[0].Pointer != "/structured_payload/category" {
		t.Fatal("runtime mapping mutated the frozen fixture")
	}
}
