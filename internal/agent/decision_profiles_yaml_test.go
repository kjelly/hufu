package agent

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDecisionProfilesYAMLSupportsLegacyTaggedAndPresetForms(t *testing.T) {
	data := `default-profile: preset
profiles:
  legacy:
    independent-judgments: 2
  tagged:
    policy:
      independent-judgments: 3
  preset:
    preset: builtin/standard@v1
`
	var cfg DecisionConfig
	if err := yaml.Unmarshal([]byte(data), &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(cfg.Profiles) != 3 || len(cfg.ProfileSpecs) != 3 {
		t.Fatalf("profiles/specs = %d/%d", len(cfg.Profiles), len(cfg.ProfileSpecs))
	}
	if cfg.ProfileSpecs["legacy"].Policy == nil || cfg.ProfileSpecs["tagged"].Policy == nil {
		t.Fatalf("inline specs = %#v", cfg.ProfileSpecs)
	}
	if got := cfg.ProfileSpecs["preset"].Preset; got == nil || got.Name != DecisionProfileBuiltinStandardV1 {
		t.Fatalf("preset = %#v", got)
	}
	if cfg.Profiles["preset"].IndependentJudgments != 3 {
		t.Fatalf("preset compatibility policy = %#v", cfg.Profiles["preset"])
	}
}

func TestDecisionProfilesYAMLStrictlyRejectsInvalidShapes(t *testing.T) {
	tests := map[string]string{
		"both tagged fields": `profiles:
  bad:
    preset: builtin/light@v1
    policy:
      independent-judgments: 1
`,
		"mixed tagged legacy": `profiles:
  bad:
    preset: builtin/light@v1
    independent-judgments: 1
`,
		"unknown nested policy": `profiles:
  bad:
    policy:
      independent-judgments: 1
      unknown-field: true
`,
		"unknown decision field": `profiles: {}
unknown-field: true
`,
		"null entry": `profiles:
  bad: null
`,
		"scalar entry": `profiles:
  bad: builtin/light@v1
`,
		"reserved off": `profiles:
  off:
    independent-judgments: 1
`,
		"reserved builtin namespace": `profiles:
  builtin/local:
    independent-judgments: 1
`,
		"unversioned preset": `profiles:
  bad:
    preset: builtin/light
`,
		"unknown version": `profiles:
  bad:
    preset: builtin/light@v2
`,
		"duplicate field": `profiles:
  bad:
    policy:
      independent-judgments: 1
    policy:
      independent-judgments: 2
`,
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			var cfg DecisionConfig
			if err := yaml.Unmarshal([]byte(data), &cfg); err == nil {
				t.Fatalf("invalid YAML was accepted: %#v", cfg)
			}
		})
	}
}

func TestDecisionProfilesYAMLRoundTripPreservesPresetAndInlineIdentity(t *testing.T) {
	input := `default-profile: standard
profiles:
  standard:
    preset: builtin/standard@v1
  custom:
    independent-judgments: 2
`
	var first DecisionConfig
	if err := yaml.Unmarshal([]byte(input), &first); err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if !strings.Contains(text, "preset: builtin/standard@v1") || !strings.Contains(text, "policy:") {
		t.Fatalf("round-trip YAML lost profile identity:\n%s", text)
	}
	var second DecisionConfig
	if err := yaml.Unmarshal(encoded, &second); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"standard", "custom"} {
		left, _, ok, err := ResolveDecisionProfileSpec(first, name, BuiltInDecisionProfileCatalog())
		if err != nil || !ok {
			t.Fatalf("resolve first %q: ok=%v err=%v", name, ok, err)
		}
		right, _, ok, err := ResolveDecisionProfileSpec(second, name, BuiltInDecisionProfileCatalog())
		if err != nil || !ok || !EqualMaterializedDecisionPolicies(left, right) {
			t.Fatalf("round-trip profile %q changed: ok=%v err=%v", name, ok, err)
		}
	}
}

func TestDecisionConfigMarshalYAMLPreservesAuthoringProvenance(t *testing.T) {
	cfg := DecisionConfig{
		DefaultProfile: "preset",
		Profiles: map[string]DecisionPolicy{
			"legacy": {IndependentJudgments: 2},
		},
		ProfileSpecs: map[string]DecisionProfileSpec{
			"preset": {Preset: &DecisionProfileRef{Name: DecisionProfileBuiltinStandardV1}},
		},
	}

	wire, err := cfg.MarshalYAML()
	if err != nil {
		t.Fatalf("MarshalYAML: %v", err)
	}
	encoded, err := yaml.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal wire value: %v", err)
	}

	var document yaml.Node
	if err := yaml.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("parse serialized wire value: %v", err)
	}
	if len(document.Content) != 1 {
		t.Fatalf("serialized YAML document nodes = %d, want 1", len(document.Content))
	}
	root := document.Content[0]
	profiles := mappingValue(t, root, "profiles")
	preset := mappingValue(t, profiles, "preset")
	if got := scalarMappingValue(t, preset, "preset"); got != DecisionProfileBuiltinStandardV1 {
		t.Fatalf("preset wire value = %q, want %q", got, DecisionProfileBuiltinStandardV1)
	}
	if hasMappingKey(preset, "policy") {
		t.Fatal("preset provenance was replaced by materialized policy")
	}

	legacy := mappingValue(t, profiles, "legacy")
	legacyPolicy := mappingValue(t, legacy, "policy")
	if got := scalarMappingValue(t, legacyPolicy, "independent-judgments"); got != "2" {
		t.Fatalf("legacy inline policy wire value = %q, want 2", got)
	}
}

func mappingValue(t *testing.T, mapping *yaml.Node, key string) *yaml.Node {
	t.Helper()
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		t.Fatalf("YAML node for %q is not a mapping: %#v", key, mapping)
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	t.Fatalf("YAML mapping is missing key %q", key)
	return nil
}

func scalarMappingValue(t *testing.T, mapping *yaml.Node, key string) string {
	t.Helper()
	node := mappingValue(t, mapping, key)
	if node.Kind != yaml.ScalarNode {
		t.Fatalf("YAML value for %q is not scalar: %#v", key, node)
	}
	return node.Value
}

func hasMappingKey(mapping *yaml.Node, key string) bool {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return true
		}
	}
	return false
}

func TestMaterializeDecisionConfigAdaptsCompatibilityMapsAndRejectsConflicts(t *testing.T) {
	legacy := DecisionPolicy{IndependentJudgments: 2}
	materialized, err := MaterializeDecisionConfig(DecisionConfig{
		Profiles: map[string]DecisionPolicy{"legacy": legacy},
	}, BuiltInDecisionProfileCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if materialized.ProfileSpecs["legacy"].Policy == nil {
		t.Fatal("legacy compatibility map did not produce an inline spec")
	}

	preset := DecisionProfileSpec{Preset: &DecisionProfileRef{Name: DecisionProfileBuiltinLightV1}}
	materialized, err = MaterializeDecisionConfig(DecisionConfig{
		ProfileSpecs: map[string]DecisionProfileSpec{"preset": preset},
	}, BuiltInDecisionProfileCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if materialized.Profiles["preset"].IndependentJudgments != 2 {
		t.Fatalf("spec did not produce compatibility policy: %#v", materialized.Profiles["preset"])
	}

	conflict := DecisionConfig{
		Profiles:     map[string]DecisionPolicy{"same": {IndependentJudgments: 2}},
		ProfileSpecs: map[string]DecisionProfileSpec{"same": {Policy: &DecisionPolicy{IndependentJudgments: 3}}},
	}
	if _, err := MaterializeDecisionConfig(conflict, BuiltInDecisionProfileCatalog()); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflicting maps error = %v", err)
	}
	if _, err := yaml.Marshal(conflict); err == nil {
		t.Fatal("serialization accepted conflicting compatibility maps")
	}
}

func TestInlineProfileNamedStandardIsNotReinterpretedAsBuiltin(t *testing.T) {
	cfg := DecisionConfig{Profiles: map[string]DecisionPolicy{
		"standard": {IndependentJudgments: 1},
	}}
	policy, metadata, ok, err := ResolveDecisionProfileSpec(cfg, "standard", BuiltInDecisionProfileCatalog())
	if err != nil || !ok {
		t.Fatalf("resolve inline standard: ok=%v err=%v", ok, err)
	}
	if metadata.Origin != DecisionProfileOriginTeamInline || metadata.Ref != "" || policy.IndependentJudgments != 1 {
		t.Fatalf("inline standard was reinterpreted: policy=%#v metadata=%#v", policy, metadata)
	}
}
