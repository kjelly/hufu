package team

import (
	"fmt"
	"path/filepath"
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
	return migrateTeamManifestToV1Alpha1(teamDir, false)
}

// MigrateTeamManifestToV1Alpha1CanonicalAuthoring performs the same dry-run
// migration while rewriting supported legacy decision/request spellings to
// the canonical authoring shape. It never writes the source file.
func MigrateTeamManifestToV1Alpha1CanonicalAuthoring(teamDir string) ([]byte, bool, error) {
	return migrateTeamManifestToV1Alpha1(teamDir, true)
}

func migrateTeamManifestToV1Alpha1(teamDir string, canonicalAuthoring bool) (out []byte, already bool, err error) {
	data, filename, found, err := findTeamManifestFile(teamDir)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, fmt.Errorf("%s: no team.yml or team.yaml found to migrate", teamDir)
	}

	yc, version, err := decodeTeamManifestYAML(filename, data)
	if err != nil {
		return nil, false, err
	}
	if canonicalAuthoring {
		if err := canonicalizeTeamAuthoring(&yc); err != nil {
			return nil, false, err
		}
	}

	out, err = marshalTeamManifestV1Alpha1(teamDir, yc)
	if err != nil {
		return nil, false, err
	}
	return out, version == SchemaVersionV1Alpha1, nil
}

func canonicalizeTeamAuthoring(yc *teamConfigYAML) error {
	if yc == nil {
		return fmt.Errorf("cannot canonicalize nil team manifest")
	}
	decision := &yc.Decision
	if !decision.profileSet && decision.defaultProfileSet {
		decision.Profile = strings.TrimSpace(decision.DefaultProfile)
	}
	decision.DefaultProfile = ""
	if !decision.Routing.hintsSet && decision.routingHintsSet {
		decision.Routing.Hints = append([]agent.RoutingHint(nil), decision.RoutingHints...)
	}
	decision.RoutingHints = nil
	if !yc.requestSet && decision.legacyContractSet && decision.RequestContract != nil && decision.RequestContract.Enabled {
		contract := decision.RequestContract
		yc.Request = RequestAuthoringConfig{Objective: contract.Objective, SuccessCriteria: append([]agent.RequestSuccessCriterion(nil), contract.SuccessCriteria...), Constraints: append([]agent.RequestConstraint(nil), contract.Constraints...), Assumptions: append([]agent.RequestContractAssumption(nil), contract.Assumptions...)}
		yc.requestSet = true
	}
	decision.RequestContract = nil
	return nil
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
