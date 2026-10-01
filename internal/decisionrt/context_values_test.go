package decisionrt_test

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/decisionrt"
)

type namedContextString string

func TestCanonicalContextValues(t *testing.T) {
	tests := []struct {
		name  string
		input map[string]any
		want  map[string]any
	}{
		{name: "nil", input: nil, want: map[string]any{}},
		{name: "empty", input: map[string]any{}, want: map[string]any{}},
		{
			name: "every accepted type",
			input: map[string]any{
				"text": "value", "named": namedContextString("named"), "flag": true, "off": false,
				"int": int(3), "int8": int8(-4), "uint": uint(5), "max": uint64(math.MaxInt64),
				"integral": float64(3), "fraction": 2.5, "single": float32(0.1), "large": 1e21,
				"negative_zero": math.Copysign(0, -1), "number": json.Number("3.0"), "exponent": json.Number("1e2"),
			},
			want: map[string]any{
				"text": "value", "named": "named", "flag": true, "off": false,
				"int": json.Number("3"), "int8": json.Number("-4"), "uint": json.Number("5"),
				"max": json.Number("9223372036854775807"), "integral": json.Number("3"), "fraction": json.Number("2.5"),
				"single": json.Number("0.10000000149011612"), "large": json.Number("1e+21"),
				"negative_zero": json.Number("0"), "number": json.Number("3"), "exponent": json.Number("100"),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := decisionrt.CanonicalContextValues(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestCanonicalContextValuesEquivalentNumbersMatch(t *testing.T) {
	var want map[string]any
	for index, value := range []any{int(3), int64(3), uint8(3), float64(3), json.Number("3.0"), json.Number("3")} {
		got, err := decisionrt.CanonicalContextValues(map[string]any{"n": value})
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			want = got
		} else if !reflect.DeepEqual(got, want) {
			t.Fatalf("value %#v gives %#v, want %#v", value, got, want)
		}
	}
}

func TestCanonicalContextValuesRejectsInvalidContext(t *testing.T) {
	tests := map[string]map[string]any{
		"nil value":          {"key": nil},
		"invalid key":        {"bad key": "value"},
		"unsupported value":  {"key": []string{"value"}},
		"non-finite":         {"key": math.Inf(1)},
		"integer overflow":   {"key": uint64(math.MaxInt64) + 1},
		"oversized string":   {"key": strings.Repeat("x", 4097)},
		"JSON integer range": {"key": json.Number("99999999999999999999")},
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := decisionrt.CanonicalContextValues(input)
			assertErrorKind(t, err, decisionrt.ErrorInvalidRequest)
			request := validChoiceRequest()
			request.Context = input
			assertErrorKind(t, request.Validate(), decisionrt.ErrorInvalidRequest)
		})
	}
}
