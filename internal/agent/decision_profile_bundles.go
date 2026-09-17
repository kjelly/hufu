package agent

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

const (
	DecisionProfileBuiltinLightV2      = "builtin/light@v2"
	DecisionProfileBuiltinStandardV2   = "builtin/standard@v2"
	DecisionProfileBuiltinHighStakesV2 = "builtin/high-stakes@v2"

	decisionRoleResolverV2 = "decision-role-resolver@v2"
)

//go:embed decision_profile_*-v2.json decision_profile_bundles.schema.json
var decisionProfileBundleFS embed.FS

type DecisionProfileRoleSpecV2 struct {
	Count                   int      `json:"count"`
	PreferredCapabilities   []string `json:"preferred-capabilities"`
	RequiredCapabilities    []string `json:"required-capabilities"`
	CandidateOrder          []string `json:"candidate-order"`
	Fallback                string   `json:"fallback"`
	AllowRepeatedDefinition bool     `json:"allow-repeated-definition"`
	ToolPolicy              string   `json:"tool-policy"`
}

type DecisionProfileRolesV2 struct {
	Proposal  DecisionProfileRoleSpecV2 `json:"proposal"`
	Reference DecisionProfileRoleSpecV2 `json:"reference"`
	Judge     DecisionProfileRoleSpecV2 `json:"judge"`
	Challenge DecisionProfileRoleSpecV2 `json:"challenge"`
	Premortem DecisionProfileRoleSpecV2 `json:"premortem"`
}

type DecisionProfileDiversityV2 struct {
	Scope                   string `json:"scope"`
	MinDistinctAgents       int    `json:"min-distinct-agents"`
	MinDistinctModels       int    `json:"min-distinct-models"`
	MinDistinctProviders    int    `json:"min-distinct-providers"`
	PreferDistinctAgents    bool   `json:"prefer-distinct-agents"`
	PreferDistinctModels    bool   `json:"prefer-distinct-models"`
	PreferDistinctProviders bool   `json:"prefer-distinct-providers"`
}

type DecisionProfileRoleResolutionV2 struct {
	Version                   string                     `json:"version"`
	Roles                     DecisionProfileRolesV2     `json:"roles"`
	Revision                  string                     `json:"revision"`
	Finalization              string                     `json:"finalization"`
	Diversity                 DecisionProfileDiversityV2 `json:"diversity"`
	FallbackOnInvocationError bool                       `json:"fallback-on-invocation-error"`
	AllowNewCredentials       bool                       `json:"allow-new-credentials"`
	AllowCrossTeam            bool                       `json:"allow-cross-team"`
}

type DecisionProfileEvidenceV2 struct {
	Collector                            string `json:"collector"`
	MaxCandidates                        uint32 `json:"max-candidates"`
	MaxScanBytes                         uint64 `json:"max-scan-bytes"`
	MaxSelectedItems                     uint32 `json:"max-selected-items"`
	MaxItemViewBytes                     uint64 `json:"max-item-view-bytes"`
	MaxTotalViewBytes                    uint64 `json:"max-total-view-bytes"`
	MaxEvidenceInputTokens               uint64 `json:"max-evidence-input-tokens"`
	FramingReserveTokens                 uint64 `json:"framing-reserve-tokens"`
	PerCallMaxOutputTokens               uint64 `json:"per-call-max-output-tokens"`
	UnknownContextWindow                 string `json:"unknown-context-window"`
	TextExtraction                       string `json:"text-extraction"`
	StructuredTruncation                 string `json:"structured-truncation"`
	MandatoryOverflow                    string `json:"mandatory-overflow"`
	MaxJSONDepth                         uint32 `json:"max-json-depth"`
	MaxJSONNodes                         uint32 `json:"max-json-nodes"`
	MaxLineBytes                         uint64 `json:"max-line-bytes"`
	MinKnownIndependentGroups            uint32 `json:"min-known-independent-groups"`
	RequireArtifactBackedBaseRate        bool   `json:"require-artifact-backed-base-rate"`
	IncludeFailedAssertions              bool   `json:"include-failed-assertions"`
	UnknownProvenanceCountsAsIndependent bool   `json:"unknown-provenance-counts-as-independent"`
}

type DecisionProfileLimitsV2 struct {
	TotalDecisionTokens        uint64 `json:"total-decision-tokens"`
	ActiveDecisionDurationMS   uint64 `json:"active-decision-duration-ms"`
	MaxGenerations             uint32 `json:"max-generations"`
	MaxStageInvocationAttempts uint32 `json:"max-stage-invocation-attempts"`
	UnknownUsageReservation    string `json:"unknown-usage-reservation"`
	TransportRetry             string `json:"transport-retry"`
	UnknownResultReplay        string `json:"unknown-result-replay"`
	CleanupTimeoutMS           uint64 `json:"cleanup-timeout-ms"`
}

type DecisionRoleCapabilitiesV1 struct {
	Proposal  []string `json:"proposal" yaml:"proposal,omitempty"`
	Reference []string `json:"reference" yaml:"reference,omitempty"`
	Judge     []string `json:"judge" yaml:"judge,omitempty"`
	Challenge []string `json:"challenge" yaml:"challenge,omitempty"`
	Premortem []string `json:"premortem" yaml:"premortem,omitempty"`
}

type DecisionRoleDiversityConstraintsV1 struct {
	MinDistinctAgents    int `json:"min_distinct_agents" yaml:"min-distinct-agents,omitempty"`
	MinDistinctModels    int `json:"min_distinct_models" yaml:"min-distinct-models,omitempty"`
	MinDistinctProviders int `json:"min_distinct_providers" yaml:"min-distinct-providers,omitempty"`
}

type DecisionRoleConstraintsV1 struct {
	SchemaVersion         int                                `json:"schema_version" yaml:"-"`
	Kind                  string                             `json:"kind" yaml:"-"`
	RequiredCapabilities  DecisionRoleCapabilitiesV1         `json:"required_capabilities" yaml:"required-capabilities,omitempty"`
	PreferredCapabilities DecisionRoleCapabilitiesV1         `json:"preferred_capabilities" yaml:"preferred-capabilities,omitempty"`
	Diversity             DecisionRoleDiversityConstraintsV1 `json:"diversity" yaml:"diversity,omitempty"`
	Fallback              string                             `json:"fallback" yaml:"fallback,omitempty"`
	CandidateLimit        int                                `json:"candidate_limit" yaml:"candidate-limit,omitempty"`
}

func DefaultDecisionRoleConstraintsV1() DecisionRoleConstraintsV1 {
	return DecisionRoleConstraintsV1{SchemaVersion: 1, Kind: "decision_role_constraints", Fallback: "inherit", CandidateLimit: 64}
}

func (value DecisionRoleConstraintsV1) Validate() error {
	if value.SchemaVersion != 1 || value.Kind != "decision_role_constraints" || value.CandidateLimit < 1 || value.CandidateLimit > 64 || value.Fallback != "inherit" && value.Fallback != "forbid" {
		return fmt.Errorf("invalid decision role constraints envelope")
	}
	if value.Diversity.MinDistinctAgents < 0 || value.Diversity.MinDistinctModels < 0 || value.Diversity.MinDistinctProviders < 0 {
		return fmt.Errorf("decision role diversity floors cannot be negative")
	}
	for _, set := range []DecisionRoleCapabilitiesV1{value.RequiredCapabilities, value.PreferredCapabilities} {
		for _, values := range [][]string{set.Proposal, set.Reference, set.Judge, set.Challenge, set.Premortem} {
			if len(values) > 64 {
				return fmt.Errorf("decision role capability list exceeds 64 entries")
			}
			seen := make(map[string]struct{}, len(values))
			for _, capability := range values {
				capability = strings.TrimSpace(capability)
				if capability == "" {
					return fmt.Errorf("decision role capability cannot be empty")
				}
				if _, duplicate := seen[capability]; duplicate {
					return fmt.Errorf("decision role capability %q is duplicated", capability)
				}
				seen[capability] = struct{}{}
			}
		}
	}
	return nil
}

// MergeDecisionRoleConstraints monotonically tightens team constraints with
// operator constraints. No field in extra can widen the admitted set.
func MergeDecisionRoleConstraints(base, extra DecisionRoleConstraintsV1) (DecisionRoleConstraintsV1, error) {
	if err := base.Validate(); err != nil {
		return DecisionRoleConstraintsV1{}, fmt.Errorf("base decision role constraints: %w", err)
	}
	if err := extra.Validate(); err != nil {
		return DecisionRoleConstraintsV1{}, fmt.Errorf("extra decision role constraints: %w", err)
	}
	result := DefaultDecisionRoleConstraintsV1()
	result.RequiredCapabilities = mergeDecisionRoleCapabilitySets(base.RequiredCapabilities, extra.RequiredCapabilities)
	result.PreferredCapabilities = mergeDecisionRoleCapabilitySets(base.PreferredCapabilities, extra.PreferredCapabilities)
	result.Diversity = DecisionRoleDiversityConstraintsV1{
		MinDistinctAgents:    max(base.Diversity.MinDistinctAgents, extra.Diversity.MinDistinctAgents),
		MinDistinctModels:    max(base.Diversity.MinDistinctModels, extra.Diversity.MinDistinctModels),
		MinDistinctProviders: max(base.Diversity.MinDistinctProviders, extra.Diversity.MinDistinctProviders),
	}
	result.CandidateLimit = min(base.CandidateLimit, extra.CandidateLimit)
	if base.Fallback == "forbid" || extra.Fallback == "forbid" {
		result.Fallback = "forbid"
	}
	return result, result.Validate()
}

func mergeDecisionRoleCapabilitySets(left, right DecisionRoleCapabilitiesV1) DecisionRoleCapabilitiesV1 {
	return DecisionRoleCapabilitiesV1{
		Proposal:  mergeDecisionCapabilities(left.Proposal, right.Proposal),
		Reference: mergeDecisionCapabilities(left.Reference, right.Reference),
		Judge:     mergeDecisionCapabilities(left.Judge, right.Judge),
		Challenge: mergeDecisionCapabilities(left.Challenge, right.Challenge),
		Premortem: mergeDecisionCapabilities(left.Premortem, right.Premortem),
	}
}

func mergeDecisionCapabilities(left, right []string) []string {
	result := append(slices.Clone(left), right...)
	slices.Sort(result)
	return slices.Compact(result)
}

// DecisionProfileBundleV2 is the complete immutable catalog entry. RawJSON is
// retained so callers can persist the exact validated bundle snapshot.
type DecisionProfileBundleV2 struct {
	SchemaVersion  int                             `json:"schema_version"`
	Kind           string                          `json:"kind"`
	Ref            string                          `json:"ref"`
	PolicyRaw      json.RawMessage                 `json:"policy"`
	RoleResolution DecisionProfileRoleResolutionV2 `json:"role-resolution"`
	Evidence       DecisionProfileEvidenceV2       `json:"evidence"`
	Limits         DecisionProfileLimitsV2         `json:"limits"`
	Policy         DecisionPolicy                  `json:"-"`
	BundleDigest   string                          `json:"-"`
	RawJSON        []byte                          `json:"-"`
}

var decisionProfileBundleFiles = map[string]string{
	DecisionProfileBuiltinLightV2:      "decision_profile_light-v2.json",
	DecisionProfileBuiltinStandardV2:   "decision_profile_standard-v2.json",
	DecisionProfileBuiltinHighStakesV2: "decision_profile_high-stakes-v2.json",
}

var decisionProfileBundleDigests = map[string]string{
	DecisionProfileBuiltinLightV2:      "856a78bcd82dc4c8e648eb8309ac773a9a7bb36527ba6c592da5acb4cc181753",
	DecisionProfileBuiltinStandardV2:   "281bb57c062e7329505239d46bf586ab3d935ea3077a094820e0cf9c55c4ce37",
	DecisionProfileBuiltinHighStakesV2: "cb2e3124822a8b8800e0d7753d283ed112c1e4ae6a1def3fe4d0c87adab2a9b1",
}

var (
	decisionProfileBundleSchemaOnce sync.Once
	decisionProfileBundleSchema     *jsonschema.Schema
	decisionProfileBundleSchemaErr  error
)

// ResolveBuiltInDecisionProfileBundle resolves and fully validates one exact
// V2 catalog reference. Unversioned aliases are deliberately rejected.
func ResolveBuiltInDecisionProfileBundle(ref string) (DecisionProfileBundleV2, error) {
	ref = strings.TrimSpace(ref)
	filename, ok := decisionProfileBundleFiles[ref]
	if !ok {
		return DecisionProfileBundleV2{}, fmt.Errorf("unknown decision profile bundle %q", ref)
	}
	raw, err := decisionProfileBundleFS.ReadFile(filename)
	if err != nil {
		return DecisionProfileBundleV2{}, fmt.Errorf("read decision profile bundle %q: %w", ref, err)
	}
	value, err := decodeDecisionProfileBundleJSON(raw)
	if err != nil {
		return DecisionProfileBundleV2{}, fmt.Errorf("decode decision profile bundle %q: %w", ref, err)
	}
	schema, err := compiledDecisionProfileBundleSchema()
	if err != nil {
		return DecisionProfileBundleV2{}, err
	}
	if err := schema.Validate(value); err != nil {
		return DecisionProfileBundleV2{}, fmt.Errorf("validate decision profile bundle %q: %w", ref, err)
	}
	var bundle DecisionProfileBundleV2
	if err := decodeStrictBundleJSON(raw, &bundle); err != nil {
		return DecisionProfileBundleV2{}, err
	}
	if bundle.Ref != ref || bundle.SchemaVersion != 2 || bundle.Kind != "decision_profile_bundle" || bundle.RoleResolution.Version != decisionRoleResolverV2 {
		return DecisionProfileBundleV2{}, fmt.Errorf("decision profile bundle %q has mismatched identity", ref)
	}
	if err := yaml.Unmarshal(bundle.PolicyRaw, &bundle.Policy); err != nil {
		return DecisionProfileBundleV2{}, fmt.Errorf("project decision profile bundle %q policy: %w", ref, err)
	}
	if err := bundle.Policy.Validate(); err != nil {
		return DecisionProfileBundleV2{}, fmt.Errorf("validate decision profile bundle %q policy: %w", ref, err)
	}
	digest, err := decisionProfileBundleDigest(value)
	if err != nil {
		return DecisionProfileBundleV2{}, err
	}
	if digest != decisionProfileBundleDigests[ref] {
		return DecisionProfileBundleV2{}, fmt.Errorf("decision profile bundle %q digest mismatch", ref)
	}
	bundle.BundleDigest = digest
	bundle.RawJSON = slices.Clone(raw)
	bundle.PolicyRaw = slices.Clone(bundle.PolicyRaw)
	return bundle, nil
}

func BuiltInDecisionProfileBundleRefs() []string {
	refs := make([]string, 0, len(decisionProfileBundleFiles))
	for ref := range decisionProfileBundleFiles {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	return refs
}

func compiledDecisionProfileBundleSchema() (*jsonschema.Schema, error) {
	decisionProfileBundleSchemaOnce.Do(func() {
		raw, err := decisionProfileBundleFS.ReadFile("decision_profile_bundles.schema.json")
		if err != nil {
			decisionProfileBundleSchemaErr = err
			return
		}
		value, err := decodeDecisionProfileBundleJSON(raw)
		if err != nil {
			decisionProfileBundleSchemaErr = err
			return
		}
		compiler := jsonschema.NewCompiler()
		compiler.DefaultDraft(jsonschema.Draft2020)
		if err := compiler.AddResource("decision_profile_bundles.schema.json", value); err != nil {
			decisionProfileBundleSchemaErr = err
			return
		}
		decisionProfileBundleSchema, decisionProfileBundleSchemaErr = compiler.Compile("decision_profile_bundles.schema.json")
	})
	return decisionProfileBundleSchema, decisionProfileBundleSchemaErr
}

func decodeDecisionProfileBundleJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return value, nil
}

func decodeStrictBundleJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("decision profile bundle has trailing JSON")
	}
	return nil
}

func decisionProfileBundleDigest(value any) (string, error) {
	var canonical bytes.Buffer
	if err := appendDecisionBundleCanonicalJSON(&canonical, value); err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("hufu/decision-bundle/v2"))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(canonical.Bytes())
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func appendDecisionBundleCanonicalJSON(out *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		out.WriteString(strconv.FormatBool(typed))
	case string:
		encoded, _ := json.Marshal(typed)
		out.Write(encoded)
	case json.Number:
		text := typed.String()
		integer, err := strconv.ParseUint(text, 10, 64)
		if err != nil || integer > 1<<53-1 || strconv.FormatUint(integer, 10) != text {
			return fmt.Errorf("decision bundle contains non-canonical number %q", text)
		}
		out.WriteString(text)
	case []any:
		out.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				out.WriteByte(',')
			}
			if err := appendDecisionBundleCanonicalJSON(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				out.WriteByte(',')
			}
			encoded, _ := json.Marshal(key)
			out.Write(encoded)
			out.WriteByte(':')
			if err := appendDecisionBundleCanonicalJSON(out, typed[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("decision bundle contains unsupported value %T", value)
	}
	return nil
}
