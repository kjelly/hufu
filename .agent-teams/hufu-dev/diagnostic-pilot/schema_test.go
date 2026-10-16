package main

import (
	"encoding/json/v2"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestPilotSchemasAndProvenance(t *testing.T) {
	p, model, _, err := loadOfflineInputs()
	if err != nil {
		t.Fatal(err)
	}
	for ref, f := range p.bundles {
		m, err := measure(f, probeInput{ref, "check_source"})
		if err != nil {
			t.Fatal(err)
		}
		for name, value := range map[string]any{"fixture.schema.json": f, "input.schema.json": probeInput{ref, "check_source"}, "measurement.schema.json": m} {
			data, err := resources.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			var document any
			if err := json.Unmarshal(data, &document); err != nil {
				t.Fatal(err)
			}
			compiler := jsonschema.NewCompiler()
			compiler.DefaultDraft(jsonschema.Draft2020)
			const url = "https://hufu.invalid/pilot-schema.json"
			if err := compiler.AddResource(url, document); err != nil {
				t.Fatal(err)
			}
			schema, err := compiler.Compile(url)
			if err != nil {
				t.Fatal(err)
			}
			data, err = json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(decoded); err != nil {
				t.Fatal(err)
			}
			decoded["oracle"] = "source_fault"
			if schema.Validate(decoded) == nil {
				t.Fatal("schema permitted an oracle field")
			}
		}
	}
	data, err := resources.ReadFile("provenance.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Synthetic bool           `json:"synthetic"`
		Source    string         `json:"source"`
		Seeds     []int          `json:"seed_range"`
		Repeats   map[string]int `json:"hypothesis_repetitions"`
		SourceP   []uint64       `json:"source_fail_percent"`
		EnvP      []uint64       `json:"environment_fail_percent"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if !manifest.Synthetic || manifest.Source != "fixture_design_v1" || len(manifest.Seeds) != 2 || manifest.Seeds[0] != 0 || manifest.Seeds[1] != 99 || len(manifest.SourceP) != 3 || len(manifest.EnvP) != 3 {
		t.Fatal("invalid provenance")
	}
	for i, h := range model.Hypotheses {
		if uint64(manifest.Repeats[h.ID])*100_000_000 != h.PriorUnits || manifest.SourceP[i]*10_000_000 != model.Experiments[0].Outcomes[1].LikelihoodUnits[i] || manifest.EnvP[i]*10_000_000 != model.Experiments[1].Outcomes[1].LikelihoodUnits[i] {
			t.Fatal("model differs from declared synthetic generator")
		}
	}
}
