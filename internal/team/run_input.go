package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

const (
	runInputSnapshotVersion            = 1
	maxRunInputDefinitions             = 64
	maxRunInputValueBytes              = 64 * 1024
	maxRunInputSnapshotBytes           = 256 * 1024
	maxRunInputDepth                   = 8
	maxRunInputArrayItems              = 256
	maxRunInputProperties              = 128
	maxRunInputEvidenceItems           = 128
	maxRunInputLocationBytes           = 4 * 1024
	maxRunInputPromptBytes             = 256 * 1024
	maxRunInputResolverOutputBytes     = 128 * 1024
	maxRunInputResolverDiagnosticBytes = 8 * 1024
)

var runInputNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
var runInputHashPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type RunInputSchema struct {
	Type                 string                    `json:"type" yaml:"type"`
	Enum                 []json.RawMessage         `json:"enum,omitempty" yaml:"-"`
	Minimum              *float64                  `json:"minimum,omitempty" yaml:"minimum,omitempty"`
	Maximum              *float64                  `json:"maximum,omitempty" yaml:"maximum,omitempty"`
	MinLength            *int                      `json:"min_length,omitempty" yaml:"min-length,omitempty"`
	MaxLength            *int                      `json:"max_length,omitempty" yaml:"max-length,omitempty"`
	MinItems             *int                      `json:"min_items,omitempty" yaml:"min-items,omitempty"`
	MaxItems             *int                      `json:"max_items,omitempty" yaml:"max-items,omitempty"`
	Items                *RunInputSchema           `json:"items,omitempty" yaml:"items,omitempty"`
	Properties           map[string]RunInputSchema `json:"properties,omitempty" yaml:"properties,omitempty"`
	RequiredProperties   []string                  `json:"required_properties,omitempty" yaml:"required-properties,omitempty"`
	AdditionalProperties *bool                     `json:"additional_properties,omitempty" yaml:"additional-properties,omitempty"`
}

type runInputSchemaYAML struct {
	Type                 string                        `yaml:"type"`
	Enum                 []any                         `yaml:"enum,omitempty"`
	Minimum              *float64                      `yaml:"minimum,omitempty"`
	Maximum              *float64                      `yaml:"maximum,omitempty"`
	MinLength            *int                          `yaml:"min-length,omitempty"`
	MaxLength            *int                          `yaml:"max-length,omitempty"`
	MinItems             *int                          `yaml:"min-items,omitempty"`
	MaxItems             *int                          `yaml:"max-items,omitempty"`
	Items                *runInputSchemaYAML           `yaml:"items,omitempty"`
	Properties           map[string]runInputSchemaYAML `yaml:"properties,omitempty"`
	RequiredProperties   []string                      `yaml:"required-properties,omitempty"`
	AdditionalProperties *bool                         `yaml:"additional-properties,omitempty"`
	Sensitive            bool                          `yaml:"sensitive,omitempty"`
	Secret               bool                          `yaml:"secret,omitempty"`
}

type runInputDefinitionYAML struct {
	runInputSchemaYAML `yaml:",inline"`
	Required           bool                  `yaml:"required,omitempty"`
	Default            any                   `yaml:"default,omitempty"`
	Resolver           *RunInputResolverSpec `yaml:"resolver,omitempty"`
}

type RunInputResolverSpec struct {
	ID         string `json:"id" yaml:"id"`
	Capability string `json:"capability" yaml:"capability"`
	Type       string `json:"type" yaml:"type"`
	Source     string `json:"source" yaml:"source"`
	SideEffect string `json:"side_effect" yaml:"side_effect"`
	Timeout    int    `json:"timeout" yaml:"timeout"`
}

type RunInputDefinition struct {
	Name     string                `json:"name"`
	Schema   RunInputSchema        `json:"schema"`
	Required bool                  `json:"required"`
	Default  json.RawMessage       `json:"default,omitempty"`
	Resolver *RunInputResolverSpec `json:"resolver,omitempty"`
}

type RunInputSource string

const (
	RunInputSourceCLI      RunInputSource = "cli"
	RunInputSourceFile     RunInputSource = "file"
	RunInputSourceResolver RunInputSource = "resolver"
	RunInputSourceDefault  RunInputSource = "default"
)

type InputEvidence struct {
	Source   RunInputSource `json:"source"`
	Location string         `json:"location,omitempty"`
	Start    int            `json:"start,omitzero"`
	End      int            `json:"end,omitzero"`
	Kind     string         `json:"kind,omitempty"`
}

type ResolvedRunInput struct {
	Name            string          `json:"name"`
	Type            string          `json:"type"`
	CanonicalValue  json.RawMessage `json:"value"`
	ValueHash       string          `json:"value_hash"`
	Source          RunInputSource  `json:"source"`
	ResolverID      string          `json:"resolver_id,omitempty"`
	ResolverVersion string          `json:"resolver_version,omitempty"`
	Evidence        []InputEvidence `json:"evidence,omitempty"`
}

type RunInputSnapshot struct {
	Version      int                `json:"version"`
	ID           string             `json:"id"`
	RunID        string             `json:"run_id"`
	InvocationID string             `json:"invocation_id"`
	Team         string             `json:"team"`
	SchemaHash   string             `json:"schema_hash"`
	Inputs       []ResolvedRunInput `json:"inputs"`
	SnapshotHash string             `json:"snapshot_hash"`
}

type RunInputAssignment struct {
	Name            string
	RawValue        []byte
	Source          RunInputSource
	Location        string
	ResolverID      string
	ResolverVersion string
	Evidence        []InputEvidence
}

type runInputCandidate struct {
	value           json.RawMessage
	source          RunInputSource
	evidence        []InputEvidence
	resolverID      string
	resolverVersion string
}

type RunInputResolverRequest struct {
	Type          string          `json:"type"`
	InputName     string          `json:"input_name"`
	Prompt        string          `json:"prompt"`
	ExplicitValue json.RawMessage `json:"explicit_value"`
	SchemaHash    string          `json:"schema_hash"`
	ResolverID    string          `json:"resolver_id"`
}

type RunInputResolverEvidence struct {
	Source string `json:"source"`
	Start  int    `json:"start,omitzero"`
	End    int    `json:"end,omitzero"`
	Kind   string `json:"kind,omitempty"`
}

type RunInputResolverResponse struct {
	Status          string                     `json:"status"`
	Value           json.RawMessage            `json:"value,omitempty"`
	Evidence        []RunInputResolverEvidence `json:"evidence,omitempty"`
	ResolverVersion string                     `json:"resolver_version,omitempty"`
	Diagnostic      string                     `json:"diagnostic,omitempty"`
}

func normalizeRunInputDefinitions(raw map[string]runInputDefinitionYAML) ([]RunInputDefinition, error) {
	if len(raw) > maxRunInputDefinitions {
		return nil, fmt.Errorf("inputs: %d definitions exceed maximum %d", len(raw), maxRunInputDefinitions)
	}
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)
	definitions := make([]RunInputDefinition, 0, len(names))
	for _, name := range names {
		if !runInputNamePattern.MatchString(name) {
			return nil, fmt.Errorf("inputs.%s: name must match %s", name, runInputNamePattern)
		}
		authored := raw[name]
		schema, err := normalizeRunInputSchema(authored.runInputSchemaYAML, 1)
		if err != nil {
			return nil, fmt.Errorf("inputs.%s: %w", name, err)
		}
		definition := RunInputDefinition{Name: name, Schema: schema, Required: authored.Required, Resolver: normalizeRunInputResolver(authored.Resolver)}
		if authored.Default != nil {
			encoded, err := canonicalJSONValue(authored.Default)
			if err != nil {
				return nil, fmt.Errorf("inputs.%s.default: %w", name, err)
			}
			canonical, err := validateAndCanonicalizeRunInput(schema, encoded)
			if err != nil {
				return nil, fmt.Errorf("inputs.%s.default: %w", name, err)
			}
			definition.Default = canonical
		}
		if err := validateRunInputResolver(definition.Resolver); err != nil {
			return nil, fmt.Errorf("inputs.%s.resolver: %w", name, err)
		}
		definitions = append(definitions, definition)
	}
	return definitions, nil
}

func normalizeRunInputSchema(raw runInputSchemaYAML, depth int) (RunInputSchema, error) {
	if depth > maxRunInputDepth {
		return RunInputSchema{}, fmt.Errorf("schema nesting exceeds maximum depth %d", maxRunInputDepth)
	}
	if raw.Sensitive || raw.Secret {
		return RunInputSchema{}, errors.New("secret or sensitive run inputs are not supported; use a credential channel")
	}
	schema := RunInputSchema{
		Type: strings.TrimSpace(raw.Type), Minimum: raw.Minimum, Maximum: raw.Maximum,
		MinLength: raw.MinLength, MaxLength: raw.MaxLength, MinItems: raw.MinItems, MaxItems: raw.MaxItems,
		RequiredProperties: slices.Clone(raw.RequiredProperties), AdditionalProperties: raw.AdditionalProperties,
	}
	switch schema.Type {
	case "string", "integer", "number", "boolean", "array", "object":
	default:
		return RunInputSchema{}, fmt.Errorf("unsupported type %q", schema.Type)
	}
	if raw.Items != nil {
		itemSchema, err := normalizeRunInputSchema(*raw.Items, depth+1)
		if err != nil {
			return RunInputSchema{}, fmt.Errorf("items: %w", err)
		}
		schema.Items = &itemSchema
	}
	if len(raw.Properties) > maxRunInputProperties {
		return RunInputSchema{}, fmt.Errorf("%d properties exceed maximum %d", len(raw.Properties), maxRunInputProperties)
	}
	if len(raw.Properties) > 0 {
		schema.Properties = make(map[string]RunInputSchema, len(raw.Properties))
		for name, authored := range raw.Properties {
			if strings.TrimSpace(name) == "" {
				return RunInputSchema{}, errors.New("property name must not be empty")
			}
			property, err := normalizeRunInputSchema(authored, depth+1)
			if err != nil {
				return RunInputSchema{}, fmt.Errorf("properties.%s: %w", name, err)
			}
			schema.Properties[name] = property
		}
	}
	for _, value := range raw.Enum {
		encoded, err := canonicalJSONValue(value)
		if err != nil {
			return RunInputSchema{}, fmt.Errorf("enum: %w", err)
		}
		enumSchema := schema
		enumSchema.Enum = nil
		canonical, err := validateAndCanonicalizeRunInputWithoutEnum(enumSchema, encoded, depth)
		if err != nil {
			return RunInputSchema{}, fmt.Errorf("enum: %w", err)
		}
		schema.Enum = append(schema.Enum, canonical)
	}
	if err := validateRunInputSchemaShape(schema); err != nil {
		return RunInputSchema{}, err
	}
	return schema, nil
}

func validateRunInputSchemaShape(schema RunInputSchema) error {
	if schema.Minimum != nil && schema.Maximum != nil && *schema.Minimum > *schema.Maximum {
		return errors.New("minimum exceeds maximum")
	}
	if schema.MinLength != nil && *schema.MinLength < 0 || schema.MaxLength != nil && *schema.MaxLength < 0 {
		return errors.New("string length bounds must be non-negative")
	}
	if schema.MinLength != nil && schema.MaxLength != nil && *schema.MinLength > *schema.MaxLength {
		return errors.New("min-length exceeds max-length")
	}
	if schema.MinItems != nil && *schema.MinItems < 0 || schema.MaxItems != nil && *schema.MaxItems < 0 {
		return errors.New("array item bounds must be non-negative")
	}
	if schema.MinItems != nil && schema.MaxItems != nil && *schema.MinItems > *schema.MaxItems {
		return errors.New("min-items exceeds max-items")
	}
	if schema.MaxItems != nil && *schema.MaxItems > maxRunInputArrayItems {
		return fmt.Errorf("max-items exceeds maximum %d", maxRunInputArrayItems)
	}
	if schema.Type == "array" && schema.Items == nil {
		return errors.New("array schema requires items")
	}
	if schema.Type != "array" && schema.Items != nil {
		return errors.New("items is only valid for array schemas")
	}
	if schema.Type != "object" && (len(schema.Properties) > 0 || len(schema.RequiredProperties) > 0 || schema.AdditionalProperties != nil) {
		return errors.New("properties, required-properties, and additional-properties are only valid for object schemas")
	}
	if schema.Type != "integer" && schema.Type != "number" && (schema.Minimum != nil || schema.Maximum != nil) {
		return errors.New("minimum and maximum are only valid for numeric schemas")
	}
	if schema.Type != "string" && (schema.MinLength != nil || schema.MaxLength != nil) {
		return errors.New("min-length and max-length are only valid for string schemas")
	}
	if schema.Type != "array" && (schema.MinItems != nil || schema.MaxItems != nil) {
		return errors.New("min-items and max-items are only valid for array schemas")
	}
	for _, name := range schema.RequiredProperties {
		if _, ok := schema.Properties[name]; !ok {
			return fmt.Errorf("required property %q has no schema", name)
		}
	}
	return nil
}

func validateRunInputResolver(resolver *RunInputResolverSpec) error {
	if resolver == nil {
		return nil
	}
	if strings.TrimSpace(resolver.ID) == "" || strings.TrimSpace(resolver.Capability) == "" || strings.TrimSpace(resolver.Type) == "" {
		return errors.New("id, capability, and type are required")
	}
	if resolver.Source != "invocation_prompt" {
		return fmt.Errorf("source must be invocation_prompt, got %q", resolver.Source)
	}
	if resolver.SideEffect != "none" {
		return fmt.Errorf("side-effect must be none, got %q", resolver.SideEffect)
	}
	if resolver.Timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if resolver.Timeout > 300 {
		return errors.New("timeout must not exceed 300 seconds")
	}
	if len(resolver.ID) > 256 || len(resolver.Capability) > 256 || len(resolver.Type) > 256 {
		return errors.New("id, capability, and type must not exceed 256 bytes")
	}
	return nil
}

func validateRunInputResolverResponse(response RunInputResolverResponse) error {
	switch response.Status {
	case "matched":
		if len(response.Value) == 0 || !json.Valid(response.Value) {
			return errors.New("input_resolver_failed: matched response requires one valid JSON value")
		}
		if strings.TrimSpace(response.ResolverVersion) == "" {
			return errors.New("input_resolver_failed: matched response requires resolver_version")
		}
	case "no_match":
		if len(response.Value) > 0 && string(response.Value) != "null" {
			return errors.New("input_resolver_failed: no_match response must not contain a value")
		}
	case "ambiguous", "invalid":
		if len(response.Value) > 0 && string(response.Value) != "null" {
			return fmt.Errorf("input_resolver_failed: %s response must not contain a value", response.Status)
		}
		if strings.TrimSpace(response.Diagnostic) == "" {
			return fmt.Errorf("input_resolver_failed: %s response requires a diagnostic", response.Status)
		}
	default:
		return fmt.Errorf("input_resolver_failed: unsupported resolver status %q", response.Status)
	}
	if len(response.Evidence) > maxRunInputEvidenceItems {
		return fmt.Errorf("input_resolver_failed: %d evidence records exceed maximum %d", len(response.Evidence), maxRunInputEvidenceItems)
	}
	if len(response.Diagnostic) > maxRunInputResolverDiagnosticBytes {
		return fmt.Errorf("input_resolver_failed: diagnostic exceeds %d bytes", maxRunInputResolverDiagnosticBytes)
	}
	for _, evidence := range response.Evidence {
		if evidence.Source != "prompt" {
			return fmt.Errorf("input_resolver_failed: evidence source must be prompt, got %q", evidence.Source)
		}
		if evidence.Start < 0 || evidence.End <= evidence.Start || evidence.End > maxRunInputPromptBytes {
			return errors.New("input_resolver_failed: evidence range is invalid")
		}
		if len(evidence.Kind) > 256 {
			return errors.New("input_resolver_failed: evidence kind is too long")
		}
	}
	return nil
}

func validateAndCanonicalizeRunInput(schema RunInputSchema, raw []byte) (json.RawMessage, error) {
	canonical, err := validateAndCanonicalizeRunInputWithoutEnum(schema, raw, 1)
	if err != nil {
		return nil, err
	}
	if len(schema.Enum) > 0 {
		matched := false
		for _, allowed := range schema.Enum {
			if bytes.Equal(canonical, allowed) {
				matched = true
				break
			}
		}
		if !matched {
			return nil, fmt.Errorf("value is not in enum")
		}
	}
	return canonical, nil
}

func validateAndCanonicalizeRunInputWithoutEnum(schema RunInputSchema, raw []byte, depth int) (json.RawMessage, error) {
	if len(raw) > maxRunInputValueBytes {
		return nil, fmt.Errorf("value exceeds %d bytes", maxRunInputValueBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("invalid JSON value: %w", err)
	}
	if err := ensureRunInputJSONEOF(decoder); err != nil {
		return nil, err
	}
	if err := validateRunInputValue(schema, value, depth); err != nil {
		return nil, err
	}
	return canonicalJSONValue(value)
}

func ensureRunInputJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("value must contain exactly one JSON document")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}

func validateRunInputValue(schema RunInputSchema, value any, depth int) error {
	if depth > maxRunInputDepth {
		return fmt.Errorf("value nesting exceeds maximum depth %d", maxRunInputDepth)
	}
	var err error
	switch schema.Type {
	case "string":
		err = validateRunInputString(schema, value)
	case "integer":
		err = validateRunInputInteger(schema, value)
	case "number":
		err = validateRunInputNumber(schema, value)
	case "boolean":
		if _, ok := value.(bool); !ok {
			err = errors.New("expected boolean")
		}
	case "array":
		err = validateRunInputArray(schema, value, depth)
	case "object":
		err = validateRunInputObject(schema, value, depth)
	}
	if err != nil {
		return err
	}
	return validateRunInputEnum(schema, value)
}

func validateRunInputString(schema RunInputSchema, value any) error {
	text, ok := value.(string)
	if !ok {
		return errors.New("expected string")
	}
	length := len([]rune(text))
	if schema.MinLength != nil && length < *schema.MinLength || schema.MaxLength != nil && length > *schema.MaxLength {
		return fmt.Errorf("string length %d is outside configured bounds", length)
	}
	return nil
}

func validateRunInputInteger(schema RunInputSchema, value any) error {
	number, ok := value.(json.Number)
	if !ok || strings.ContainsAny(number.String(), ".eE") {
		return errors.New("expected integer")
	}
	integer, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return fmt.Errorf("invalid integer: %w", err)
	}
	return validateNumericBounds(schema, float64(integer))
}

func validateRunInputNumber(schema RunInputSchema, value any) error {
	number, ok := value.(json.Number)
	if !ok {
		return errors.New("expected number")
	}
	floatValue, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.IsInf(floatValue, 0) || math.IsNaN(floatValue) {
		return errors.New("invalid number")
	}
	return validateNumericBounds(schema, floatValue)
}

func validateRunInputArray(schema RunInputSchema, value any, depth int) error {
	values, ok := value.([]any)
	if !ok {
		return errors.New("expected array")
	}
	if len(values) > maxRunInputArrayItems {
		return fmt.Errorf("array contains %d items, maximum is %d", len(values), maxRunInputArrayItems)
	}
	if schema.MinItems != nil && len(values) < *schema.MinItems || schema.MaxItems != nil && len(values) > *schema.MaxItems {
		return fmt.Errorf("array length %d is outside configured bounds", len(values))
	}
	for index, entry := range values {
		if err := validateRunInputValue(*schema.Items, entry, depth+1); err != nil {
			return fmt.Errorf("item %d: %w", index, err)
		}
	}
	return nil
}

func validateRunInputObject(schema RunInputSchema, value any, depth int) error {
	object, ok := value.(map[string]any)
	if !ok {
		return errors.New("expected object")
	}
	if len(object) > maxRunInputProperties {
		return fmt.Errorf("object contains %d properties, maximum is %d", len(object), maxRunInputProperties)
	}
	for _, required := range schema.RequiredProperties {
		if _, ok := object[required]; !ok {
			return fmt.Errorf("required property %q is missing", required)
		}
	}
	allowAdditional := schema.AdditionalProperties == nil || *schema.AdditionalProperties
	for name, entry := range object {
		property, exists := schema.Properties[name]
		if !exists {
			if !allowAdditional {
				return fmt.Errorf("additional property %q is not allowed", name)
			}
			continue
		}
		if err := validateRunInputValue(property, entry, depth+1); err != nil {
			return fmt.Errorf("property %q: %w", name, err)
		}
	}
	return nil
}

func validateRunInputEnum(schema RunInputSchema, value any) error {
	if len(schema.Enum) > 0 {
		canonical, err := canonicalJSONValue(value)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(schema.Enum, func(allowed json.RawMessage) bool { return bytes.Equal(canonical, allowed) }) {
			return errors.New("value is not in enum")
		}
	}
	return nil
}

func validateNumericBounds(schema RunInputSchema, value float64) error {
	if schema.Minimum != nil && value < *schema.Minimum || schema.Maximum != nil && value > *schema.Maximum {
		return fmt.Errorf("number %v is outside configured bounds", value)
	}
	return nil
}

func canonicalJSONValue(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("canonical JSON encode: %w", err)
	}
	if len(encoded) > maxRunInputValueBytes {
		return nil, fmt.Errorf("canonical value exceeds %d bytes", maxRunInputValueBytes)
	}
	return json.RawMessage(encoded), nil
}

func RunInputSchemaHash(definitions []RunInputDefinition) (string, error) {
	definitions = cloneRunInputDefinitions(definitions)
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	type schemaIdentity struct {
		Name     string          `json:"name"`
		Schema   RunInputSchema  `json:"schema"`
		Required bool            `json:"required"`
		Default  json.RawMessage `json:"default,omitempty"`
	}
	identity := make([]schemaIdentity, len(definitions))
	for index, definition := range definitions {
		identity[index] = schemaIdentity{Name: definition.Name, Schema: definition.Schema, Required: definition.Required, Default: definition.Default}
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("encode run input schema: %w", err)
	}
	return runInputHash(encoded), nil
}

func executionRunInputPolicyHash(session *TeamSession) (string, error) {
	if session == nil {
		return "", errors.New("run input policy requires a team session")
	}
	type providerIdentity struct {
		Capability string   `json:"capability"`
		Provider   string   `json:"provider,omitempty"`
		Runtime    string   `json:"runtime,omitempty"`
		Source     string   `json:"source,omitempty"`
		Mode       string   `json:"mode,omitempty"`
		Command    []string `json:"command,omitempty"`
		Dir        string   `json:"dir,omitempty"`
		Timeout    int64    `json:"timeout,omitzero"`
	}
	providers := make([]providerIdentity, 0)
	seen := make(map[string]struct{})
	for _, definition := range session.RunInputDefinitions {
		if definition.Resolver == nil {
			continue
		}
		capability := strings.TrimSpace(definition.Resolver.Capability)
		if _, ok := seen[capability]; ok {
			continue
		}
		provider, ok := configuredActionProvider(session.Config.ActionProviders, capability)
		if !ok {
			return "", fmt.Errorf("run input resolver capability %q has no action provider", capability)
		}
		seen[capability] = struct{}{}
		providerName := ""
		if session.ProviderRegistry != nil {
			providerName = session.ProviderRegistry.ProviderName(capability)
		}
		providers = append(providers, providerIdentity{
			Capability: capability, Provider: providerName, Runtime: provider.Runtime, Source: provider.Source, Mode: provider.Mode,
			Command: slices.Clone(provider.Command), Dir: provider.Dir, Timeout: provider.Timeout,
		})
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].Capability < providers[j].Capability })
	definitions := cloneRunInputDefinitions(session.RunInputDefinitions)
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	actions := make([]any, 0)
	for _, task := range session.ContractTasks {
		if task.Action != nil && len(task.Action.InputBindings) > 0 {
			actions = append(actions, actionContractIdentity(task.Action))
		}
	}
	encoded, err := json.Marshal(struct {
		Definitions []RunInputDefinition `json:"definitions"`
		Providers   []providerIdentity   `json:"providers,omitempty"`
		Actions     []any                `json:"actions,omitempty"`
	}{definitions, providers, actions})
	if err != nil {
		return "", fmt.Errorf("encode run input policy: %w", err)
	}
	return runInputHash(encoded), nil
}

func configuredActionProvider(providers map[string]agent.ActionProviderConfig, capability string) (agent.ActionProviderConfig, bool) {
	for name, provider := range providers {
		if normalizeCapability(name) == normalizeCapability(capability) {
			return provider, true
		}
	}
	return agent.ActionProviderConfig{}, false
}

func ResolveRunInputSnapshot(definitions []RunInputDefinition, assignments []RunInputAssignment, runID, invocationID, teamName string) (*RunInputSnapshot, error) {
	if len(definitions) > maxRunInputDefinitions {
		return nil, fmt.Errorf("input_schema_invalid: %d definitions exceed maximum %d", len(definitions), maxRunInputDefinitions)
	}
	if len(definitions) == 0 {
		if len(assignments) > 0 {
			return nil, fmt.Errorf("input_unknown: team %q declares no run inputs", teamName)
		}
		return nil, nil
	}
	definitions, byName, err := validateAndIndexRunInputDefinitions(definitions)
	if err != nil {
		return nil, err
	}
	candidates, err := collectRunInputCandidates(byName, assignments, teamName)
	if err != nil {
		return nil, err
	}
	resolved, err := selectResolvedRunInputs(definitions, candidates)
	if err != nil {
		return nil, err
	}
	schemaHash, err := RunInputSchemaHash(definitions)
	if err != nil {
		return nil, err
	}
	snapshot := &RunInputSnapshot{
		Version: runInputSnapshotVersion, RunID: runID, InvocationID: invocationID, Team: teamName,
		SchemaHash: schemaHash, Inputs: resolved,
	}
	identity, err := runInputSnapshotIdentity(snapshot)
	if err != nil {
		return nil, err
	}
	snapshot.SnapshotHash = runInputHash(identity)
	snapshot.ID = "run-inputs-" + strings.TrimPrefix(snapshot.SnapshotHash, "sha256:")[:24]
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	if len(encoded) > maxRunInputSnapshotBytes {
		return nil, fmt.Errorf("run input snapshot exceeds %d bytes", maxRunInputSnapshotBytes)
	}
	if err := ValidateRunInputSnapshot(snapshot); err != nil {
		return nil, fmt.Errorf("resolved run input snapshot is invalid: %w", err)
	}
	return snapshot, nil
}

func validateAndIndexRunInputDefinitions(definitions []RunInputDefinition) ([]RunInputDefinition, map[string]RunInputDefinition, error) {
	definitions = cloneRunInputDefinitions(definitions)
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	byName := make(map[string]RunInputDefinition, len(definitions))
	for index := range definitions {
		definition := &definitions[index]
		if !runInputNamePattern.MatchString(definition.Name) {
			return nil, nil, fmt.Errorf("input schema contains invalid name %q", definition.Name)
		}
		if _, duplicate := byName[definition.Name]; duplicate {
			return nil, nil, fmt.Errorf("input schema repeats name %q", definition.Name)
		}
		if err := validateRunInputSchemaRecursive(definition.Schema, 1); err != nil {
			return nil, nil, fmt.Errorf("input schema %q: %w", definition.Name, err)
		}
		if len(definition.Default) > 0 {
			canonical, err := validateAndCanonicalizeRunInput(definition.Schema, definition.Default)
			if err != nil {
				return nil, nil, fmt.Errorf("input schema %q default: %w", definition.Name, err)
			}
			definition.Default = canonical
		}
		byName[definition.Name] = *definition
	}
	return definitions, byName, nil
}

func collectRunInputCandidates(byName map[string]RunInputDefinition, assignments []RunInputAssignment, teamName string) (map[string]map[RunInputSource]*runInputCandidate, error) {
	candidates := make(map[string]map[RunInputSource]*runInputCandidate)
	for _, assignment := range assignments {
		definition, ok := byName[assignment.Name]
		if !ok {
			return nil, fmt.Errorf("input_unknown: %q is not declared by team %q", assignment.Name, teamName)
		}
		if assignment.Source != RunInputSourceCLI && assignment.Source != RunInputSourceFile && assignment.Source != RunInputSourceResolver {
			return nil, fmt.Errorf("input_source_invalid: %q", assignment.Source)
		}
		canonical, err := parseRunInputAssignment(definition.Schema, assignment.RawValue)
		if err != nil {
			return nil, fmt.Errorf("input_invalid: %s: %w", assignment.Name, err)
		}
		if candidates[assignment.Name] == nil {
			candidates[assignment.Name] = make(map[RunInputSource]*runInputCandidate)
		}
		existing := candidates[assignment.Name][assignment.Source]
		if existing != nil && !bytes.Equal(existing.value, canonical) {
			if assignment.Source == RunInputSourceResolver {
				return nil, fmt.Errorf("input_resolver_failed: %s resolver returned conflicting values", assignment.Name)
			}
			return nil, fmt.Errorf("input_explicit_conflict: %s has different %s values", assignment.Name, assignment.Source)
		}
		if existing == nil {
			existing = &runInputCandidate{value: canonical, source: assignment.Source, resolverID: assignment.ResolverID, resolverVersion: assignment.ResolverVersion}
			candidates[assignment.Name][assignment.Source] = existing
		}
		if assignment.Source == RunInputSourceResolver {
			existing.evidence = append(existing.evidence, assignment.Evidence...)
		} else {
			existing.evidence = append(existing.evidence, InputEvidence{Source: assignment.Source, Location: assignment.Location})
		}
	}
	return candidates, nil
}

func selectResolvedRunInputs(definitions []RunInputDefinition, candidates map[string]map[RunInputSource]*runInputCandidate) ([]ResolvedRunInput, error) {
	resolved := make([]ResolvedRunInput, 0, len(definitions))
	for _, definition := range definitions {
		input, present, err := selectResolvedRunInput(definition, candidates[definition.Name])
		if err != nil {
			return nil, err
		}
		if present {
			resolved = append(resolved, input)
		}
	}
	return resolved, nil
}

func selectResolvedRunInput(definition RunInputDefinition, perSource map[RunInputSource]*runInputCandidate) (ResolvedRunInput, bool, error) {
	cli, file, resolver := perSource[RunInputSourceCLI], perSource[RunInputSourceFile], perSource[RunInputSourceResolver]
	if cli != nil && file != nil && !bytes.Equal(cli.value, file.value) {
		return ResolvedRunInput{}, false, fmt.Errorf("input_explicit_conflict: %s differs between CLI and file", definition.Name)
	}
	chosen := cli
	if chosen == nil {
		chosen = file
	}
	if chosen != nil && resolver != nil && !bytes.Equal(chosen.value, resolver.value) {
		return ResolvedRunInput{}, false, fmt.Errorf("input_prompt_conflict: %s explicit value differs from resolver candidate", definition.Name)
	}
	if chosen == nil {
		chosen = resolver
	}
	if chosen == nil && len(definition.Default) > 0 {
		chosen = &runInputCandidate{value: slices.Clone(definition.Default), source: RunInputSourceDefault, evidence: []InputEvidence{{Source: RunInputSourceDefault, Location: "team manifest default"}}}
	}
	if chosen == nil {
		if definition.Required {
			return ResolvedRunInput{}, false, fmt.Errorf("input_missing: required input %q has no value", definition.Name)
		}
		return ResolvedRunInput{}, false, nil
	}
	evidence := slices.Clone(chosen.evidence)
	if cli != nil && file != nil {
		evidence = append(evidence, file.evidence...)
	}
	if resolver != nil && resolver != chosen {
		evidence = append(evidence, resolver.evidence...)
	}
	resolverID, resolverVersion := chosen.resolverID, chosen.resolverVersion
	if resolver != nil {
		resolverID, resolverVersion = resolver.resolverID, resolver.resolverVersion
	}
	return ResolvedRunInput{
		Name: definition.Name, Type: definition.Schema.Type, CanonicalValue: slices.Clone(chosen.value),
		ValueHash: runInputHash(chosen.value), Source: chosen.source, ResolverID: resolverID,
		ResolverVersion: resolverVersion, Evidence: evidence,
	}, true, nil
}

func resolveExplicitRunInputValues(definitions []RunInputDefinition, assignments []RunInputAssignment, teamName string) (map[string]json.RawMessage, error) {
	byName := make(map[string]RunInputDefinition, len(definitions))
	for _, definition := range definitions {
		byName[definition.Name] = definition
	}
	values := make(map[string]json.RawMessage)
	counts := make(map[string]int)
	for _, assignment := range assignments {
		if assignment.Source != RunInputSourceCLI && assignment.Source != RunInputSourceFile {
			continue
		}
		definition, ok := byName[assignment.Name]
		if !ok {
			return nil, fmt.Errorf("input_unknown: %q is not declared by team %q", assignment.Name, teamName)
		}
		counts[assignment.Name]++
		if counts[assignment.Name] > maxRunInputEvidenceItems {
			return nil, fmt.Errorf("input_invalid: %s has more than %d explicit source records", assignment.Name, maxRunInputEvidenceItems)
		}
		if len(assignment.Location) > maxRunInputLocationBytes {
			return nil, fmt.Errorf("input_invalid: %s source location exceeds %d bytes", assignment.Name, maxRunInputLocationBytes)
		}
		canonical, err := parseRunInputAssignment(definition.Schema, assignment.RawValue)
		if err != nil {
			return nil, fmt.Errorf("input_invalid: %s: %w", assignment.Name, err)
		}
		if prior, exists := values[assignment.Name]; exists && !bytes.Equal(prior, canonical) {
			return nil, fmt.Errorf("input_explicit_conflict: %s has different explicit values", assignment.Name)
		}
		values[assignment.Name] = canonical
	}
	return values, nil
}

func validateRunInputSchemaRecursive(schema RunInputSchema, depth int) error {
	if depth > maxRunInputDepth {
		return fmt.Errorf("schema nesting exceeds maximum depth %d", maxRunInputDepth)
	}
	switch schema.Type {
	case "string", "integer", "number", "boolean", "array", "object":
	default:
		return fmt.Errorf("unsupported type %q", schema.Type)
	}
	if err := validateRunInputSchemaShape(schema); err != nil {
		return err
	}
	if schema.Items != nil {
		if err := validateRunInputSchemaRecursive(*schema.Items, depth+1); err != nil {
			return fmt.Errorf("items: %w", err)
		}
	}
	if len(schema.Properties) > maxRunInputProperties {
		return fmt.Errorf("%d properties exceed maximum %d", len(schema.Properties), maxRunInputProperties)
	}
	for name, property := range schema.Properties {
		if err := validateRunInputSchemaRecursive(property, depth+1); err != nil {
			return fmt.Errorf("property %q: %w", name, err)
		}
	}
	return nil
}

func ValidateRunInputSnapshot(snapshot *RunInputSnapshot) error {
	if snapshot == nil || snapshot.Version != runInputSnapshotVersion {
		return fmt.Errorf("unsupported run input snapshot version")
	}
	if strings.TrimSpace(snapshot.ID) == "" || strings.TrimSpace(snapshot.RunID) == "" || strings.TrimSpace(snapshot.InvocationID) == "" || strings.TrimSpace(snapshot.Team) == "" {
		return errors.New("run input snapshot identity is incomplete")
	}
	if !runInputHashPattern.MatchString(snapshot.SchemaHash) || !runInputHashPattern.MatchString(snapshot.SnapshotHash) {
		return errors.New("run input snapshot hashes are invalid")
	}
	if len(snapshot.Inputs) > maxRunInputDefinitions {
		return fmt.Errorf("run input snapshot contains %d inputs, maximum is %d", len(snapshot.Inputs), maxRunInputDefinitions)
	}
	previous := ""
	for _, input := range snapshot.Inputs {
		if !runInputNamePattern.MatchString(input.Name) || input.Name <= previous {
			return errors.New("run input snapshot inputs are not uniquely sorted")
		}
		if err := validateResolvedRunInput(input); err != nil {
			return err
		}
		previous = input.Name
	}
	identity, err := runInputSnapshotIdentity(snapshot)
	if err != nil {
		return err
	}
	if snapshot.SnapshotHash != runInputHash(identity) {
		return errors.New("run input snapshot hash mismatch")
	}
	wantID := "run-inputs-" + strings.TrimPrefix(snapshot.SnapshotHash, "sha256:")[:24]
	if snapshot.ID != wantID {
		return errors.New("run input snapshot ID mismatch")
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if len(encoded) > maxRunInputSnapshotBytes {
		return fmt.Errorf("run input snapshot exceeds %d bytes", maxRunInputSnapshotBytes)
	}
	return nil
}

func validateResolvedRunInput(input ResolvedRunInput) error {
	if len(input.CanonicalValue) == 0 || len(input.CanonicalValue) > maxRunInputValueBytes || !json.Valid(input.CanonicalValue) {
		return fmt.Errorf("run input %q has an invalid canonical value", input.Name)
	}
	decoder := json.NewDecoder(bytes.NewReader(input.CanonicalValue))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return fmt.Errorf("run input %q has an invalid canonical value: %w", input.Name, err)
	}
	canonical, err := canonicalJSONValue(decoded)
	if err != nil || !bytes.Equal(canonical, input.CanonicalValue) {
		return fmt.Errorf("run input %q value is not canonical JSON", input.Name)
	}
	if !isRunInputType(input.Type) {
		return fmt.Errorf("run input %q has invalid type %q", input.Name, input.Type)
	}
	if !isRunInputSource(input.Source) {
		return fmt.Errorf("run input %q has invalid source %q", input.Name, input.Source)
	}
	if input.Source == RunInputSourceResolver && (strings.TrimSpace(input.ResolverID) == "" || strings.TrimSpace(input.ResolverVersion) == "") {
		return fmt.Errorf("run input %q resolved by a resolver lacks resolver identity", input.Name)
	}
	if len(input.ResolverID) > 256 || len(input.ResolverVersion) > 128 {
		return fmt.Errorf("run input %q resolver identity is too long", input.Name)
	}
	if len(input.Evidence) > maxRunInputEvidenceItems {
		return fmt.Errorf("run input %q has too many evidence records", input.Name)
	}
	for _, evidence := range input.Evidence {
		if err := validateRunInputEvidence(input.Name, evidence); err != nil {
			return err
		}
	}
	if !runInputHashPattern.MatchString(input.ValueHash) || input.ValueHash != runInputHash(input.CanonicalValue) {
		return fmt.Errorf("run input %q value hash mismatch", input.Name)
	}
	return nil
}

func validateRunInputEvidence(inputName string, evidence InputEvidence) error {
	if !isRunInputSource(evidence.Source) {
		return fmt.Errorf("run input %q has invalid evidence source %q", inputName, evidence.Source)
	}
	if len(evidence.Location) > maxRunInputLocationBytes {
		return fmt.Errorf("run input %q evidence location exceeds %d bytes", inputName, maxRunInputLocationBytes)
	}
	if evidence.Start < 0 || evidence.End < 0 || evidence.End != 0 && evidence.End < evidence.Start {
		return fmt.Errorf("run input %q has invalid evidence range", inputName)
	}
	if len(evidence.Kind) > 256 {
		return fmt.Errorf("run input %q evidence kind is too long", inputName)
	}
	return nil
}

func isRunInputType(value string) bool {
	switch value {
	case "string", "integer", "number", "boolean", "array", "object":
		return true
	default:
		return false
	}
}

func isRunInputSource(value RunInputSource) bool {
	switch value {
	case RunInputSourceCLI, RunInputSourceFile, RunInputSourceResolver, RunInputSourceDefault:
		return true
	default:
		return false
	}
}

func parseRunInputAssignment(schema RunInputSchema, raw []byte) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if schema.Type == "string" && !json.Valid(trimmed) {
		encoded, err := json.Marshal(string(trimmed))
		if err != nil {
			return nil, err
		}
		trimmed = encoded
	}
	return validateAndCanonicalizeRunInput(schema, trimmed)
}

func runInputSnapshotIdentity(snapshot *RunInputSnapshot) ([]byte, error) {
	identity := struct {
		Version      int                `json:"version"`
		RunID        string             `json:"run_id"`
		InvocationID string             `json:"invocation_id"`
		Team         string             `json:"team"`
		SchemaHash   string             `json:"schema_hash"`
		Inputs       []ResolvedRunInput `json:"inputs"`
	}{snapshot.Version, snapshot.RunID, snapshot.InvocationID, snapshot.Team, snapshot.SchemaHash, snapshot.Inputs}
	return json.Marshal(identity)
}

func runInputHash(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func cloneRunInputResolver(src *RunInputResolverSpec) *RunInputResolverSpec {
	if src == nil {
		return nil
	}
	clone := *src
	return &clone
}

func normalizeRunInputResolver(src *RunInputResolverSpec) *RunInputResolverSpec {
	resolver := cloneRunInputResolver(src)
	if resolver == nil {
		return nil
	}
	resolver.ID = strings.TrimSpace(resolver.ID)
	resolver.Capability = normalizeCapability(resolver.Capability)
	resolver.Type = strings.TrimSpace(resolver.Type)
	resolver.Source = strings.ToLower(strings.TrimSpace(resolver.Source))
	resolver.SideEffect = strings.ToLower(strings.TrimSpace(resolver.SideEffect))
	return resolver
}

func cloneRunInputSchema(src RunInputSchema) RunInputSchema {
	clone := src
	clone.Enum = make([]json.RawMessage, len(src.Enum))
	for index := range src.Enum {
		clone.Enum[index] = slices.Clone(src.Enum[index])
	}
	clone.RequiredProperties = slices.Clone(src.RequiredProperties)
	if src.Items != nil {
		clone.Items = new(cloneRunInputSchema(*src.Items))
	}
	if src.Properties != nil {
		clone.Properties = make(map[string]RunInputSchema, len(src.Properties))
		for name, property := range src.Properties {
			clone.Properties[name] = cloneRunInputSchema(property)
		}
	}
	return clone
}

func cloneRunInputDefinitions(src []RunInputDefinition) []RunInputDefinition {
	clone := make([]RunInputDefinition, len(src))
	for index := range src {
		clone[index] = src[index]
		clone[index].Schema = cloneRunInputSchema(src[index].Schema)
		clone[index].Default = slices.Clone(src[index].Default)
		clone[index].Resolver = cloneRunInputResolver(src[index].Resolver)
	}
	return clone
}

func CloneRunInputSnapshot(src *RunInputSnapshot) *RunInputSnapshot {
	if src == nil {
		return nil
	}
	clone := *src
	clone.Inputs = make([]ResolvedRunInput, len(src.Inputs))
	for index := range src.Inputs {
		clone.Inputs[index] = src.Inputs[index]
		clone.Inputs[index].CanonicalValue = slices.Clone(src.Inputs[index].CanonicalValue)
		clone.Inputs[index].Evidence = slices.Clone(src.Inputs[index].Evidence)
	}
	return &clone
}

func CloneRunInputSnapshots(src []RunInputSnapshot) []RunInputSnapshot {
	clone := make([]RunInputSnapshot, len(src))
	for index := range src {
		clone[index] = *CloneRunInputSnapshot(&src[index])
	}
	return clone
}

func appendRunInputSnapshot(snapshots []RunInputSnapshot, snapshot RunInputSnapshot) []RunInputSnapshot {
	for index := range snapshots {
		if snapshots[index].ID == snapshot.ID {
			return snapshots
		}
	}
	return append(snapshots, *CloneRunInputSnapshot(&snapshot))
}
