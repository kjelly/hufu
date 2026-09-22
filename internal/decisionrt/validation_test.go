package decisionrt_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/decisionrt"
)

func TestSpecValidate(t *testing.T) {
	tests := map[string]func(*decisionrt.Spec){
		"empty id":              func(spec *decisionrt.Spec) { spec.ID = "" },
		"empty version":         func(spec *decisionrt.Spec) { spec.Version = "" },
		"empty question":        func(spec *decisionrt.Spec) { spec.Question = " " },
		"unknown kind":          func(spec *decisionrt.Spec) { spec.Kind = "other" },
		"one option":            func(spec *decisionrt.Spec) { spec.Options = spec.Options[:1] },
		"duplicate option":      func(spec *decisionrt.Spec) { spec.Options[1].ID = spec.Options[0].ID },
		"choice range":          func(spec *decisionrt.Spec) { spec.Range = &decisionrt.IntegerRange{} },
		"too many options":      func(spec *decisionrt.Spec) { spec.Options = makeOptions(22) },
		"invalid option id":     func(spec *decisionrt.Spec) { spec.Options[0].ID = "not valid" },
		"oversized description": func(spec *decisionrt.Spec) { spec.Options[0].Description = strings.Repeat("x", 1025) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			spec := validChoiceSpec()
			mutate(&spec)
			assertErrorKind(t, spec.Validate(), decisionrt.ErrorInvalidRequest)
		})
	}

	boolean := validChoiceSpec()
	boolean.Kind = decisionrt.KindBoolean
	assertErrorKind(t, boolean.Validate(), decisionrt.ErrorInvalidRequest)
	boolean.Options = nil
	if err := boolean.Validate(); err != nil {
		t.Fatalf("valid boolean: %v", err)
	}

	integer := validChoiceSpec()
	integer.Kind = decisionrt.KindIntegerRange
	integer.Options = nil
	assertErrorKind(t, integer.Validate(), decisionrt.ErrorInvalidRequest)
	integer.Range = &decisionrt.IntegerRange{Min: math.MinInt64, Max: math.MinInt64 + 20}
	if err := integer.Validate(); err != nil {
		t.Fatalf("valid integer range: %v", err)
	}
	integer.Range.Max++
	assertErrorKind(t, integer.Validate(), decisionrt.ErrorInvalidRequest)
	integer.Range = &decisionrt.IntegerRange{Min: math.MinInt64, Max: math.MaxInt64}
	assertErrorKind(t, integer.Validate(), decisionrt.ErrorInvalidRequest)
}

func TestRequestValidateContextDomain(t *testing.T) {
	request := validChoiceRequest()
	request.Context = map[string]any{
		"string": "value",
		"bool":   true,
		"int":    int(1),
		"int8":   int8(2),
		"uint":   uint(3),
		"float":  4.5,
		"number": json.Number("5.25"),
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid context: %v", err)
	}

	invalid := []any{
		nil,
		map[string]any{"nested": true},
		[]string{"nested"},
		math.NaN(),
		math.Inf(1),
		uint64(math.MaxInt64) + 1,
		json.Number("9223372036854775808"),
	}
	for _, value := range invalid {
		request := validChoiceRequest()
		request.Context = map[string]any{"value": value}
		assertErrorKind(t, request.Validate(), decisionrt.ErrorInvalidRequest)
	}

	request = validChoiceRequest()
	request.Context = map[string]any{"not valid": true}
	assertErrorKind(t, request.Validate(), decisionrt.ErrorInvalidRequest)

	request = validChoiceRequest()
	request.Context = make(map[string]any, 65)
	for i := range 65 {
		request.Context["k"+strings.Repeat("x", i)] = i
	}
	assertErrorKind(t, request.Validate(), decisionrt.ErrorInvalidRequest)
}

func TestRequestValidateCanonicalContextSize(t *testing.T) {
	request := validChoiceRequest()
	request.Context = make(map[string]any, 64)
	for i := range 64 {
		request.Context[fmt.Sprintf("key%02d", i)] = strings.Repeat("x", 1024)
	}
	assertErrorKind(t, request.Validate(), decisionrt.ErrorInvalidRequest)
}

func makeOptions(count int) []decisionrt.Option {
	options := make([]decisionrt.Option, count)
	for i := range count {
		options[i] = decisionrt.Option{ID: "option-" + string(rune('A'+i))}
	}
	return options
}

func validChoiceSpec() decisionrt.Spec {
	return decisionrt.Spec{
		ID:       "size-policy",
		Version:  "v1",
		Kind:     decisionrt.KindChoice,
		Question: "Select a size.",
		Options: []decisionrt.Option{
			{ID: "small", Description: "Small"},
			{ID: "large", Description: "Large"},
		},
	}
}

func validChoiceRequest() decisionrt.Request {
	return decisionrt.Request{Purpose: "size-policy@v1", Spec: validChoiceSpec()}
}

func assertErrorKind(t *testing.T, err error, want decisionrt.ErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error", want)
	}
	typed, ok := errors.AsType[*decisionrt.RuntimeError](err)
	if !ok || typed.Kind != want {
		t.Fatalf("error = %#v, want kind %s", err, want)
	}
}
