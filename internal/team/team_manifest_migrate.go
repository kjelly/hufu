package team

import (
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/agent"
)

// MigrateTeamManifestToV1Alpha1 reads teamDir's team.yml/team.yaml — in
// whatever schema it currently declares — and returns the stable,
// canonical hufu.io/v1alpha1 YAML text semantically equivalent to it. It
// never writes anything; §9 deliberately keeps the first version
// dry-run-only. already reports whether the source already declared
// hufu.io/v1alpha1: out is still a canonical re-serialization in that case
// (not necessarily byte-identical to the input — re-marshaling is itself a
// normalize/format check), it just did not need converting.
//
// It decodes the *raw*, untemplated source (findTeamManifestFile, not
// readTeamManifestSource): running the migrator's own `--var` substitution
// through Go's text/template and then re-serializing the rendered result
// would silently bake a specific run's variable values into the migrated
// file, destroying any `{{.Var}}` placeholders the original author wrote.
// The legacy `advanced:` alias namespace is expanded into its canonical
// fields for free by decodeTeamManifestYAML (it always calls
// mergeAdvancedNamespace on the legacy path before returning), so the
// migrated output never contains an `advanced:` block.
func MigrateTeamManifestToV1Alpha1(teamDir string) (out []byte, already bool, err error) {
	out, already, _, err = migrateTeamManifestToV1Alpha1(teamDir, false)
	return out, already, err
}

// MigrateTeamManifestToV1Alpha1CanonicalAuthoring performs the same dry-run
// migration while rewriting supported legacy decision/request spellings to
// the canonical authoring shape. It never writes the source file.
func MigrateTeamManifestToV1Alpha1CanonicalAuthoring(teamDir string) ([]byte, bool, error) {
	out, already, _, err := migrateTeamManifestToV1Alpha1(teamDir, true)
	return out, already, err
}

// MigrateTeamManifestToV1Alpha1CanonicalAuthoringDetailed also returns
// warnings for legacy authoring that cannot be removed without changing its
// disabled semantics.
func MigrateTeamManifestToV1Alpha1CanonicalAuthoringDetailed(teamDir string) (out []byte, already bool, warnings []string, err error) {
	return migrateTeamManifestToV1Alpha1(teamDir, true)
}

func migrateTeamManifestToV1Alpha1(teamDir string, canonicalAuthoring bool) (out []byte, already bool, warnings []string, err error) {
	data, filename, found, err := findTeamManifestFile(teamDir)
	if err != nil {
		return nil, false, nil, err
	}
	if !found {
		return nil, false, nil, fmt.Errorf("%s: no team.yml or team.yaml found to migrate", teamDir)
	}
	if canonicalAuthoring && authoringContainsTemplateMarker(data) {
		return nil, false, nil, fmt.Errorf("canonical authoring migration requires resolved decision/request templates")
	}

	yc, version, err := decodeTeamManifestYAML(filename, data)
	if err != nil {
		return nil, false, nil, err
	}
	original := yc
	if canonicalAuthoring {
		warnings, err = canonicalizeTeamAuthoring(&yc)
		if err != nil {
			return nil, false, nil, err
		}
	}

	out, err = marshalTeamManifestV1Alpha1(teamDir, yc)
	if err != nil {
		return nil, false, nil, err
	}
	if canonicalAuthoring {
		migrated, _, decodeErr := decodeTeamManifestYAML("team.yaml", out)
		if decodeErr != nil {
			return nil, false, nil, fmt.Errorf("reload canonical authoring migration: %w", decodeErr)
		}
		if verifyErr := verifyCanonicalAuthoringMigration(original, migrated); verifyErr != nil {
			return nil, false, nil, verifyErr
		}
	}
	return out, version == SchemaVersionV1Alpha1, warnings, nil
}

func canonicalizeTeamAuthoring(yc *teamConfigYAML) ([]string, error) {
	if yc == nil {
		return nil, fmt.Errorf("cannot canonicalize nil team manifest")
	}
	var warnings []string
	decision := &yc.Decision
	if !decision.profileSet && decision.defaultProfileSet {
		decision.Profile = strings.TrimSpace(decision.DefaultProfile)
	}
	decision.DefaultProfile = ""
	if !decision.Routing.hintsSet && decision.routingHintsSet {
		decision.Routing.Hints = append([]agent.RoutingHint(nil), decision.RoutingHints...)
	}
	decision.RoutingHints = nil
	if !yc.requestSet && decision.legacyContractSet && decision.RequestContract != nil {
		contract := decision.RequestContract
		switch {
		case contract.Enabled:
			yc.Request = RequestAuthoringConfig{Objective: contract.Objective, SuccessCriteria: slices.Clone(contract.SuccessCriteria), Constraints: slices.Clone(contract.Constraints), Assumptions: slices.Clone(contract.Assumptions)}
			yc.requestSet = true
			decision.RequestContract = nil
		case requestContractHasAuthoredContent(*contract):
			warnings = append(warnings, "disabled decision.request-contract contains authored content and was preserved")
		default:
			decision.RequestContract = nil
		}
	} else {
		decision.RequestContract = nil
	}
	return warnings, nil
}

func requestContractHasAuthoredContent(contract agent.RequestContractConfig) bool {
	return strings.TrimSpace(contract.Objective) != "" || len(contract.SuccessCriteria) > 0 || len(contract.Constraints) > 0 || len(contract.Assumptions) > 0
}

func authoringContainsTemplateMarker(data []byte) bool {
	var document yaml.Node
	if yaml.Unmarshal(data, &document) != nil || len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return false
	}
	mapping := document.Content[0]
	if spec := yamlMappingValue(mapping, "spec"); spec != nil && spec.Kind == yaml.MappingNode {
		mapping = spec
	}
	for _, key := range []string{"decision", "request"} {
		if value := yamlMappingValue(mapping, key); value != nil && yamlNodeContainsTemplateMarker(value) {
			return true
		}
	}
	return false
}

func yamlMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func yamlNodeContainsTemplateMarker(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.ScalarNode && (strings.Contains(node.Value, "{{") || strings.Contains(node.Value, "}}")) {
		return true
	}
	for _, child := range node.Content {
		if yamlNodeContainsTemplateMarker(child) {
			return true
		}
	}
	return false
}

func verifyCanonicalAuthoringMigration(before, after teamConfigYAML) error {
	beforeDecision, beforeRequest, _, err := NormalizeDecisionAuthoring(before.Decision, before.Request, before.requestSet, agent.BuiltInDecisionProfileCatalog())
	if err != nil {
		return fmt.Errorf("normalize source authoring: %w", err)
	}
	afterDecision, afterRequest, _, err := NormalizeDecisionAuthoring(after.Decision, after.Request, after.requestSet, agent.BuiltInDecisionProfileCatalog())
	if err != nil {
		return fmt.Errorf("normalize migrated authoring: %w", err)
	}
	if !reflect.DeepEqual(beforeRequest, afterRequest) {
		return fmt.Errorf("canonical authoring migration changed normalized request contract")
	}
	if !reflect.DeepEqual(beforeDecision, afterDecision) {
		return fmt.Errorf("canonical authoring migration changed normalized decision configuration")
	}
	names := make(map[string]struct{}, len(beforeDecision.ProfileSpecs)+len(afterDecision.ProfileSpecs))
	maps.Copy(names, mapKeysAsSet(beforeDecision.ProfileSpecs))
	maps.Copy(names, mapKeysAsSet(afterDecision.ProfileSpecs))
	for _, name := range slices.Sorted(maps.Keys(names)) {
		beforePolicy, _, beforeOK, beforeErr := agent.ResolveDecisionProfileSpec(beforeDecision, name, agent.BuiltInDecisionProfileCatalog())
		afterPolicy, _, afterOK, afterErr := agent.ResolveDecisionProfileSpec(afterDecision, name, agent.BuiltInDecisionProfileCatalog())
		if beforeErr != nil || afterErr != nil || beforeOK != afterOK {
			return fmt.Errorf("canonical authoring migration changed profile %q resolution", name)
		}
		beforeDigest, beforeErr := agent.DecisionPolicyDigest(beforePolicy)
		afterDigest, afterErr := agent.DecisionPolicyDigest(afterPolicy)
		if beforeErr != nil || afterErr != nil || beforeDigest != afterDigest {
			return fmt.Errorf("canonical authoring migration changed profile %q digest", name)
		}
		beforePlan, beforeErr := CompileDecisionExecutionPlan(beforePolicy)
		afterPlan, afterErr := CompileDecisionExecutionPlan(afterPolicy)
		if beforeErr != nil || afterErr != nil || !reflect.DeepEqual(beforePlan, afterPlan) {
			return fmt.Errorf("canonical authoring migration changed profile %q execution plan", name)
		}
	}
	return nil
}

func mapKeysAsSet[V any](values map[string]V) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for key := range values {
		result[key] = struct{}{}
	}
	return result
}

// marshalTeamManifestV1Alpha1 renders yc as a hufu.io/v1alpha1 manifest.
// yaml.v3 marshals struct fields in declaration order and sorts map keys,
// so the output is deterministic for a given yc (§3 principle 5: migration
// pure/deterministic).
func marshalTeamManifestV1Alpha1(teamDir string, yc teamConfigYAML) ([]byte, error) {
	name := strings.TrimSpace(yc.Name)
	if name == "" {
		absDir, err := filepath.Abs(teamDir)
		if err != nil {
			return nil, fmt.Errorf("invalid team directory: %w", err)
		}
		name = filepath.Base(absDir)
	}

	spec := yc.teamManifestSpecFields
	// Identity now lives solely in metadata.name. Keeping spec.name around
	// too would just reintroduce the mismatch this schema version's
	// metadata/spec name check exists to prevent (§7).
	spec.Name = ""

	manifest := TeamManifest{
		APIVersion: SchemaVersionV1Alpha1,
		Kind:       manifestKindAgentTeam,
		Metadata:   TeamMetadata{Name: name},
		Spec:       TeamSpecV1Alpha1{teamManifestSpecFields: spec},
	}

	out, err := yaml.Marshal(&manifest)
	if err != nil {
		return nil, fmt.Errorf("marshal v1alpha1 manifest: %w", err)
	}
	return out, nil
}
