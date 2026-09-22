package rule_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/decisionrt/backend/rule"
)

func TestAlwaysAbstain(t *testing.T) {
	backend := rule.AlwaysAbstain()
	if backend.Name() != "rule" {
		t.Fatalf("name = %q", backend.Name())
	}
	result, err := backend.Decide(t.Context(), decisionrt.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != decisionrt.StatusAbstained || result.ConfidenceSemantics != decisionrt.ConfidenceNone {
		t.Fatalf("result = %#v", result)
	}
}

func TestFunc(t *testing.T) {
	want := decisionrt.BackendResult{
		Status:              decisionrt.StatusDecided,
		Value:               decisionrt.Value{Choice: "small"},
		ConfidenceSemantics: decisionrt.ConfidenceNone,
	}
	backend := rule.Func{
		BackendName: "policy",
		DecideFunc: func(context.Context, decisionrt.Request) (decisionrt.BackendResult, error) {
			return want, nil
		},
	}
	if backend.Name() != "policy" {
		t.Fatalf("name = %q", backend.Name())
	}
	got, err := backend.Decide(t.Context(), decisionrt.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != want.Status || got.Value.Choice != want.Value.Choice {
		t.Fatalf("result = %#v", got)
	}
}

func TestFuncRejectsInvalidConfiguration(t *testing.T) {
	for _, backend := range []rule.Func{{BackendName: "policy"}, {DecideFunc: func(context.Context, decisionrt.Request) (decisionrt.BackendResult, error) {
		return decisionrt.BackendResult{}, nil
	}}} {
		_, err := backend.Decide(t.Context(), decisionrt.Request{})
		if err == nil {
			t.Fatal("expected configuration error")
		}
		if typed, ok := errors.AsType[*decisionrt.RuntimeError](err); !ok || typed.Kind != decisionrt.ErrorConfiguration {
			t.Fatalf("error = %#v", err)
		}
	}
}
