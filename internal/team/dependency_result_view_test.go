package team

import (
	"reflect"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestDependencyResultViewStrictYAML(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		invalid     bool
		fields      []string
	}{
		{name: "full", input: "mode: full"},
		{name: "selected canonical order", input: "mode: selected\nfields: [verification, summary, findings]", fields: []string{"summary", "findings", "verification"}},
		{name: "missing mode", input: "fields: [summary]", invalid: true},
		{name: "empty mode", input: "mode: ''", invalid: true},
		{name: "case mismatch", input: "mode: SELECTED\nfields: [summary]", invalid: true},
		{name: "unknown mode", input: "mode: auto", invalid: true},
		{name: "full empty fields", input: "mode: full\nfields: []", invalid: true},
		{name: "full null fields", input: "mode: full\nfields: null", invalid: true},
		{name: "selected missing fields", input: "mode: selected", invalid: true},
		{name: "selected empty fields", input: "mode: selected\nfields: []", invalid: true},
		{name: "selected scalar fields", input: "mode: selected\nfields: summary", invalid: true},
		{name: "selected null fields", input: "mode: selected\nfields: null", invalid: true},
		{name: "duplicate field", input: "mode: selected\nfields: [summary, summary]", invalid: true},
		{name: "unknown field", input: "mode: selected\nfields: [details]", invalid: true},
		{name: "wrong field case", input: "mode: selected\nfields: [Summary]", invalid: true},
		{name: "nonstring field", input: "mode: selected\nfields: [3]", invalid: true},
		{name: "unknown key", input: "mode: selected\nfields: [summary]\noptional: true", invalid: true},
		{name: "duplicate key", input: "mode: full\nmode: selected\nfields: [summary]", invalid: true},
		{name: "aliased fields", input: "mode: selected\nfields: &fields [verification, summary]\n", fields: []string{"summary", "verification"}},
		{name: "merged aliased fields", input: "<<: &view {mode: selected, fields: &fields [verification, summary]}\nfields: *fields", fields: []string{"summary", "verification"}},
		{name: "merged policy", input: "<<: &view {mode: selected, fields: [verification, summary]}", fields: []string{"summary", "verification"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var spec DependencyResultViewSpec
			err := yaml.Unmarshal([]byte(tc.input), &spec)
			if (err != nil) != tc.invalid {
				t.Fatalf("decode error=%v, invalid=%v", err, tc.invalid)
			}
			if err != nil {
				return
			}
			canonical, err := spec.normalized()
			if err != nil {
				t.Fatal(err)
			}
			if tc.fields == nil {
				if canonical != nil {
					t.Fatalf("full did not normalize to absence: %+v", canonical)
				}
				return
			}
			if canonical == nil || canonical.Mode != "selected" || !reflect.DeepEqual(canonical.Fields, tc.fields) {
				t.Fatalf("policy=%+v, expected fields=%v", canonical, tc.fields)
			}
		})
	}
}

func TestDependencyResultViewNormalizationDetachesPolicy(t *testing.T) {
	source := &DependencyResultViewSpec{Mode: "selected", Fields: []string{"verification", "summary"}}
	canonical, err := source.normalized()
	if err != nil {
		t.Fatal(err)
	}
	if source.Fields[0] != "verification" {
		t.Fatal("normalization mutated source")
	}
	clone := canonical.clone()
	clone.Fields[0] = "artifacts"
	if canonical.Fields[0] != "summary" {
		t.Fatal("clone aliases source")
	}
	canonical.Fields[0] = "findings"
	if source.Fields[1] != "summary" {
		t.Fatal("normalized fields alias authored source")
	}
	var absent *DependencyResultViewSpec
	if absent.clone() != nil {
		t.Fatal("nil clone acquired a policy")
	}
	if got, err := absent.normalized(); err != nil || got != nil {
		t.Fatalf("absent policy=%+v, err=%v", got, err)
	}
}
