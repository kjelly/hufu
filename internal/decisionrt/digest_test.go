package decisionrt_test

import (
	"encoding/json"
	"testing"

	"github.com/kjelly/hufu/internal/decisionrt"
)

func TestDigestCanonicalizesMapOrderAndNumericTypes(t *testing.T) {
	variants := []map[string]any{
		{"a": int(3), "b": "value"},
		{"b": "value", "a": int64(3)},
		{"a": float64(3), "b": "value"},
		{"b": "value", "a": json.Number("3.0")},
	}
	var want string
	for i, context := range variants {
		request := validChoiceRequest()
		request.Context = context
		digest, err := decisionrt.Digest(request)
		if err != nil {
			t.Fatalf("variant %d: %v", i, err)
		}
		if i == 0 {
			want = digest
		} else if digest != want {
			t.Fatalf("variant %d digest = %s, want %s", i, digest, want)
		}
	}
	if len(want) != len("sha256:")+64 {
		t.Fatalf("digest = %q", want)
	}
}

func TestDigestChangesWithSemanticInput(t *testing.T) {
	base := validChoiceRequest()
	baseDigest, err := decisionrt.Digest(base)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []func(*decisionrt.Request){
		func(request *decisionrt.Request) { request.Purpose = "other@v1" },
		func(request *decisionrt.Request) { request.Spec.Version = "v2" },
		func(request *decisionrt.Request) { request.Spec.Options[0].Description = "Tiny" },
		func(request *decisionrt.Request) { request.Context = map[string]any{"risk": "high"} },
	}
	for i, mutate := range mutations {
		request := validChoiceRequest()
		mutate(&request)
		digest, err := decisionrt.Digest(request)
		if err != nil {
			t.Fatalf("mutation %d: %v", i, err)
		}
		if digest == baseDigest {
			t.Fatalf("mutation %d did not change digest", i)
		}
	}
}

func TestDigestTreatsNilAndEmptyCollectionsEqually(t *testing.T) {
	boolean := decisionrt.Request{
		Purpose: "enabled@v1",
		Spec:    decisionrt.Spec{ID: "enabled", Version: "v1", Kind: decisionrt.KindBoolean, Question: "Enabled?"},
	}
	empty := boolean
	empty.Spec.Options = []decisionrt.Option{}
	empty.Context = map[string]any{}

	first, err := decisionrt.Digest(boolean)
	if err != nil {
		t.Fatal(err)
	}
	second, err := decisionrt.Digest(empty)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("nil digest %s != empty digest %s", first, second)
	}
}
