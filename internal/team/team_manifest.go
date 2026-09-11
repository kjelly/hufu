package team

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/notify"
)

// Schema versions a team.yaml/team.yml manifest can declare. See
// docs/architecture/team-schema-versioning.md.
const (
	// SchemaVersionLegacyFlatV0 identifies a manifest with no apiVersion
	// envelope: the original flat top-level shape (§3 principle 3).
	SchemaVersionLegacyFlatV0 = "legacy-flat-v0"
	// SchemaVersionV1Alpha1 is the `apiVersion: hufu.io/v1alpha1` envelope.
	SchemaVersionV1Alpha1 = "hufu.io/v1alpha1"

	manifestKindAgentTeam = "AgentTeam"
)

// UnsupportedSchemaVersionError is returned when a manifest declares an
// apiVersion this build does not recognize. Per §3 principle 4, an unknown
// version fails closed rather than falling back to best-effort parsing.
type UnsupportedSchemaVersionError struct {
	File       string
	APIVersion string
}

func (e *UnsupportedSchemaVersionError) Error() string {
	return fmt.Sprintf("%s: unsupported apiVersion %q (supported: %q, or omit apiVersion for the legacy flat schema)", e.File, e.APIVersion, SchemaVersionV1Alpha1)
}

// UnsupportedManifestKindError is returned when a hufu.io/v1alpha1 manifest
// declares a kind other than AgentTeam.
type UnsupportedManifestKindError struct {
	File string
	Kind string
}

func (e *UnsupportedManifestKindError) Error() string {
	return fmt.Sprintf("%s: unsupported kind %q (expected %q)", e.File, e.Kind, manifestKindAgentTeam)
}

// ManifestMetadataNameMismatchError is returned when a hufu.io/v1alpha1
// manifest declares both metadata.name and spec.name with different,
// non-empty values. v1alpha1 keeps spec.name for now (§7: no field renames
// in this version) but metadata.name is the manifest's canonical identity,
// so the two disagreeing is an authoring error, not a value to silently
// pick a winner for.
type ManifestMetadataNameMismatchError struct {
	File         string
	MetadataName string
	SpecName     string
}

func (e *ManifestMetadataNameMismatchError) Error() string {
	return fmt.Sprintf("%s: metadata.name %q does not match spec.name %q; remove one or make them match", e.File, e.MetadataName, e.SpecName)
}

// TeamMetadata is the hufu.io/v1alpha1 manifest's metadata block.
type TeamMetadata struct {
	Name        string            `yaml:"name"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

// TeamSpecV1Alpha1 is the `spec:` block of a hufu.io/v1alpha1 AgentTeam
// manifest. It inlines teamManifestSpecFields — the exact same field set
// the legacy flat schema decodes at its top level — because v1alpha1 does
// not rename or restructure the authored contract in this version (§7): it
// only wraps the existing contract in an envelope. Unlike the legacy
// schema, it has no `advanced:` alias namespace; strict decoding rejects
// that key as unknown here (§7).
type TeamSpecV1Alpha1 struct {
	teamManifestSpecFields `yaml:",inline"`
}

// TeamManifest is the raw decoded shape of a hufu.io/v1alpha1 team.yaml.
type TeamManifest struct {
	APIVersion string           `yaml:"apiVersion"`
	Kind       string           `yaml:"kind"`
	Metadata   TeamMetadata     `yaml:"metadata"`
	Spec       TeamSpecV1Alpha1 `yaml:"spec"`
}

// NormalizedTeamConfig pairs a loaded team's detected schema version with
// its normalized runtime config, for callers that need to report or gate on
// the source format (e.g. a future `hufu team validate` schema line)
// without re-detecting it.
type NormalizedTeamConfig struct {
	Version string
	Config  agent.TeamConfig
}

// teamManifestSpecFields is the shared authoring field set for both the
// legacy flat team.yaml top level and the hufu.io/v1alpha1 `spec:` block.
// Declaring it once and inline-embedding it into both teamConfigYAML
// (parse.go) and TeamSpecV1Alpha1 (above) is what makes
// TestLegacyAndV1Alpha1NormalizeIdentically true by construction — there is
// exactly one field/tag declaration to keep in sync, not two independently
// maintained structs that could silently drift apart.
type teamManifestSpecFields struct {
	Name                     string `yaml:"name"`
	Description              string `yaml:"description"`
	MaxRounds                int    `yaml:"max-rounds"`
	MinimumCoordinatorRounds int    `yaml:"minimum-coordinator-rounds"`
	MaxSteps                 int    `yaml:"max-steps"`
	Workspace                string `yaml:"workspace"`
	Timeout                  int64  `yaml:"timeout"`
	VerifyTimeout            int64  `yaml:"verify-timeout"`
	// MaxRetries is a pointer so an omitted key (built-in default) can be
	// distinguished from an explicit "max-retries: 0" override; the zero
	// value of a plain int is indistinguishable from an explicit 0.
	MaxRetries           *int                             `yaml:"max-retries"`
	AutoReport           bool                             `yaml:"auto-report"`
	AllowFreeTextResults bool                             `yaml:"allow-free-text-results"`
	Model                string                           `yaml:"model"`
	WorkerModel          string                           `yaml:"worker-model"`
	CoordinatorModel     string                           `yaml:"coordinator-model"`
	DefaultLLMBackend    string                           `yaml:"default-llm-backend"`
	ContextWindow        int                              `yaml:"context-window"`
	Temperature          string                           `yaml:"temperature"`
	MaxTokens            string                           `yaml:"max-tokens"`
	TopP                 string                           `yaml:"top-p"`
	TopK                 string                           `yaml:"top-k"`
	ReasoningEffort      string                           `yaml:"reasoning-effort"`
	Skills               string                           `yaml:"skills"`
	SkillsExclude        string                           `yaml:"skills-exclude"`
	ProviderURL          string                           `yaml:"provider-url"`
	ProviderAPIKey       string                           `yaml:"provider-api-key"`
	Providers            map[string]config.ProviderConfig `yaml:"providers"`
	Backends             map[string]config.BackendConfig  `yaml:"backends"`
	ModelList            []config.ModelEntry              `yaml:"model-list"`
	SidecarModel         string                           `yaml:"sidecar-model"`
	GuardModel           string                           `yaml:"guard-model"`
	JudgeModel           string                           `yaml:"judge-model"`
	PlanReviewerModel    string                           `yaml:"plan-reviewer-model"`
	MaxConcurrent        int                              `yaml:"max-concurrent"`
	StallThreshold       string                           `yaml:"stall-threshold"`
	MaxCoordinatorTurns  int                              `yaml:"max-coordinator-turns"`
	EscalateOnRetry      bool                             `yaml:"escalate-on-retry"`
	AutoSkills           bool                             `yaml:"auto-skills"`
	Notify               notify.NotifyConfig              `yaml:"notify"`
	AllowedPaths         interface{}                      `yaml:"allowed-paths"`
	RestrictedPath       string                           `yaml:"restricted-path"`
	NoNet                bool                             `yaml:"no-net"`
	ForceMCP             bool                             `yaml:"force-mcp"`
	ProjectContext       bool                             `yaml:"project-context"`
	Shell                string                           `yaml:"shell"`
	Vars                 map[string]interface{}           `yaml:"vars"`
	// WorkerContextSize is a token budget, not a character count; the YAML
	// key is kept as-is for backward compatibility.
	WorkerContextSize       int                                     `yaml:"worker-context-size"`
	ToolsAllowed            interface{}                             `yaml:"tools"` // tools.allowed/tools.denied in YAML - string or []string
	Requirements            agent.ContractRequirements              `yaml:"requires"`
	Delegation              rawDelegationPolicy                     `yaml:"delegation"`
	Preflight               []agent.CapabilityRequirement           `yaml:"preflight"`
	RequiredResources       []agent.RequiredResourceSpec            `yaml:"required-resources"`
	Workflow                agent.WorkflowConfig                    `yaml:"workflow"`
	Policies                agent.WorkflowPolicies                  `yaml:"policies"`
	Capabilities            agent.CapabilityConfig                  `yaml:"capabilities"`
	Verification            agent.VerificationConfig                `yaml:"verification"`
	Retry                   agent.RetryConfig                       `yaml:"retry"`
	Decision                agent.DecisionConfig                    `yaml:"decision"`
	CapabilityRegistry      map[string][]agent.DeclaredCapability   `yaml:"capability-registry"`
	RoutingPolicy           agent.RoutingPolicyConfig               `yaml:"routing-policy"`
	ActionProviders         map[string]agent.ActionProviderConfig   `yaml:"action-providers"`
	SubagentProviderDefault string                                  `yaml:"subagent-provider-default"`
	SubagentProviders       map[string]agent.SubagentProviderConfig `yaml:"subagent-providers"`
	// Kept as an opaque map here because MCP server loading is owned by the
	// session layer; declaring the key preserves this long-standing manifest
	// field while strict validation still rejects unknown top-level keys.
	MCPServers       map[string]interface{}  `yaml:"mcp-servers"`
	Unattended       bool                    `yaml:"unattended"`
	AutoApprove      bool                    `yaml:"auto-approve"`
	MaxWallClock     int64                   `yaml:"max-duration"`
	MaxTotalTokens   int64                   `yaml:"max-total-tokens"`
	Acceptance       interface{}             `yaml:"acceptance"`
	Rollback         string                  `yaml:"rollback"`
	ExecutionProfile string                  `yaml:"execution-profile"`
	GoalMode         string                  `yaml:"goal-mode"`
	Reliability      rawReliabilityConfig    `yaml:"reliability"`
	WorkerMemory     rawWorkerMemoryPolicy   `yaml:"worker-memory"`
	MemoryLearning   rawMemoryLearningPolicy `yaml:"memory-learning"`
	Compaction       rawCompactionPolicy     `yaml:"compaction"`
	Tasks            []TaskDef               `yaml:"tasks"`
}

// manifestEnvelope is a minimal, non-strict probe used only to detect
// apiVersion/kind before committing to a full decode (§5: "不得先 decode
// current struct 再猜版本" — detect the envelope first, then dispatch to the
// matching strict decoder, never the other way around). A legacy-flat
// manifest simply has neither key, which is not an error.
type manifestEnvelope struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
}

// readTeamManifestSource finds team.yml/team.yaml under teamDir, applies
// templating, and returns its bytes. found is false only when neither file
// exists — that is not an error; a team.yaml is optional and callers fall
// back to defaults.
func readTeamManifestSource(teamDir string, vars map[string]string) (data []byte, filename string, found bool, err error) {
	for _, name := range []string{"team.yml", "team.yaml"} {
		d, readErr := os.ReadFile(filepath.Join(teamDir, name))
		if readErr == nil {
			data, filename, found = d, name, true
			break
		}
	}
	if !found {
		return nil, "", false, nil
	}
	templated, err := applyTemplate(string(data), filename, vars)
	if err != nil {
		return nil, "", false, fmt.Errorf("template error in team config: %w", err)
	}
	return []byte(templated), filename, true, nil
}

// DetectTeamSchemaVersion reports which schema a team.yaml/team.yml
// declares, without fully decoding or validating it: SchemaVersionLegacyFlatV0
// (no team.yaml, or no apiVersion key), or the exact apiVersion string
// otherwise. Intended for `hufu team validate`/`team explain`-style
// schema/source-format reporting (§9); it does not itself validate that the
// version is supported.
func DetectTeamSchemaVersion(teamDir string, vars map[string]string) (string, error) {
	data, _, found, err := readTeamManifestSource(teamDir, vars)
	if err != nil {
		return "", err
	}
	if !found {
		return SchemaVersionLegacyFlatV0, nil
	}
	var envelope manifestEnvelope
	if err := yaml.Unmarshal(data, &envelope); err != nil {
		return "", fmt.Errorf("detect team manifest envelope: %w", err)
	}
	if envelope.APIVersion == "" {
		return SchemaVersionLegacyFlatV0, nil
	}
	return envelope.APIVersion, nil
}

// decodeTeamManifestYAML is the schema-version dispatch point every
// team.yaml/team.yml decode goes through (§5/§6 loader pipeline: detect
// envelope → strict decoder for version → normalize). It returns the same
// teamConfigYAML shape regardless of source schema, so every line of
// parseTeamYML downstream of this call is identical for both
// legacy-flat-v0 and hufu.io/v1alpha1 sources — the mechanism that makes
// TestLegacyAndV1Alpha1NormalizeIdentically hold by construction rather
// than by two independently maintained code paths agreeing by luck.
func decodeTeamManifestYAML(file string, data []byte) (teamConfigYAML, string, error) {
	var envelope manifestEnvelope
	if err := yaml.Unmarshal(data, &envelope); err != nil {
		return teamConfigYAML{}, "", fmt.Errorf("detect team manifest envelope: %w", err)
	}

	switch envelope.APIVersion {
	case "":
		var yc teamConfigYAML
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&yc); err != nil {
			return teamConfigYAML{}, "", fmt.Errorf("failed to parse team config: %w", err)
		}
		if err := mergeAdvancedNamespace(&yc); err != nil {
			return teamConfigYAML{}, "", fmt.Errorf("invalid team config: %w", err)
		}
		return yc, SchemaVersionLegacyFlatV0, nil

	case SchemaVersionV1Alpha1:
		if envelope.Kind != manifestKindAgentTeam {
			return teamConfigYAML{}, "", &UnsupportedManifestKindError{File: file, Kind: envelope.Kind}
		}
		var manifest TeamManifest
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&manifest); err != nil {
			return teamConfigYAML{}, "", fmt.Errorf("failed to parse team config: %w", err)
		}
		metadataName := strings.TrimSpace(manifest.Metadata.Name)
		if metadataName == "" {
			return teamConfigYAML{}, "", fmt.Errorf("%s: metadata.name is required for apiVersion %s", file, SchemaVersionV1Alpha1)
		}
		if specName := strings.TrimSpace(manifest.Spec.Name); specName != "" && specName != metadataName {
			return teamConfigYAML{}, "", &ManifestMetadataNameMismatchError{File: file, MetadataName: metadataName, SpecName: specName}
		}
		yc := teamConfigYAML{teamManifestSpecFields: manifest.Spec.teamManifestSpecFields}
		yc.Name = metadataName
		return yc, SchemaVersionV1Alpha1, nil

	default:
		return teamConfigYAML{}, "", &UnsupportedSchemaVersionError{File: file, APIVersion: envelope.APIVersion}
	}
}
