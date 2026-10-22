package paired

import (
	"encoding/json/jsontext"
	"math"
	"testing"
)

func TestCanonicalJSONPreservesIntegersAndObjectOrdering(t *testing.T) {
	value := struct {
		Z int64 `json:"z"`
		A any   `json:"a"`
	}{math.MaxInt64, jsontext.Value(`{"z":2,"a":1}`)}
	raw, err := CanonicalJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"a":{"a":1,"z":2},"z":9223372036854775807}` {
		t.Fatalf("unstable or rounded canonical form: %s", raw)
	}
	for _, raw := range []jsontext.Value{
		[]byte(`{"a":1,"a":2}`), []byte(`{"a":NaN}`), {34, 255, 34},
	} {
		if _, err := CanonicalJSON(raw); err == nil {
			t.Fatalf("accepted malformed JSON: %q", raw)
		}
	}
}

func TestDigestDomainsAndFloatProjection(t *testing.T) {
	a, err := Digest("variant", map[string]int{"b": 2, "a": 1})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Digest("context", map[string]int{"a": 1, "b": 2})
	if a == b || len(a) != 64 {
		t.Fatal("digest domains collide")
	}
	if a != "89e88f3fa4a441bf6e74155ae6d81b422a85188b2c2233f50159ba3530aa9791" {
		t.Fatalf("v1 digest golden changed: %s", a)
	}
	for _, v := range []float64{math.NaN(), math.Inf(1)} {
		if _, err := FloatProjection(v); err == nil {
			t.Fatal("non-finite projection accepted")
		}
	}
	if v, err := FloatProjection(math.Copysign(0, -1)); err != nil || v != "0" {
		t.Fatalf("negative zero = %q, %v", v, err)
	}
}
