// Package catalog binds trusted team declarations to immutable DecisionPrimitive services.
// It does not own persistence, authorization, or task lifecycle.
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"time"

	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/decisionrt/backend"
)

const DefaultEndpoint = "http://127.0.0.1:11434/v1/systemone"

// Entry defines one maintainer-owned decision. Inputs are required scalar
// context fields; no undeclared fields or nested objects may be sent.
type Entry struct {
	Backend           string            `yaml:"backend" json:"backend"`
	Endpoint          string            `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Model             string            `yaml:"model,omitempty" json:"model,omitempty"`
	APIKeyEnv         string            `yaml:"api-key-env,omitempty" json:"api_key_env,omitempty"`
	Version           string            `yaml:"version" json:"version"`
	Kind              decisionrt.Kind   `yaml:"kind" json:"kind"`
	Question          string            `yaml:"question" json:"question"`
	Options           []Option          `yaml:"options,omitempty" json:"options,omitempty"`
	Range             *IntegerRange     `yaml:"range,omitempty" json:"range,omitempty"`
	Inputs            map[string]string `yaml:"inputs" json:"inputs"`
	Agents            []string          `yaml:"agents" json:"agents"`
	Timeout           time.Duration     `yaml:"timeout,omitempty" json:"timeout,omitzero"`
	MinConfidence     *float64          `yaml:"min-confidence,omitempty" json:"min_confidence,omitempty"`
	RequireCalibrated bool              `yaml:"require-calibrated,omitempty" json:"require_calibrated,omitzero"`
	Fallback          string            `yaml:"fallback,omitempty" json:"fallback,omitempty"`
	MaxCalls          int               `yaml:"max-calls,omitempty" json:"max_calls,omitzero"`
	// BlockOn lists choice options that stop the calling worker's attempt:
	// after such a decision the runtime refuses tools that can change state
	// and accepts only a blocked result.
	BlockOn []string `yaml:"block-on,omitempty" json:"block_on,omitempty"`
}

type Option struct {
	ID          string `yaml:"id" json:"id"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

type IntegerRange struct {
	Min int64 `yaml:"min" json:"min"`
	Max int64 `yaml:"max" json:"max"`
}

type boundEntry struct {
	entry   Entry
	spec    decisionrt.Spec
	runtime decisionrt.Runtime
}

type Service struct {
	entries     map[string]boundEntry
	hash        string
	description string
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// New validates and freezes configuration without contacting a model. The
// hash pins resolved credential revisions without storing the credentials.
func New(entries map[string]Entry, opts ...ServiceOption) (*Service, error) {
	settings := options{}
	for _, opt := range opts {
		opt(&settings)
	}
	service := &Service{entries: make(map[string]boundEntry)}
	if len(entries) == 0 {
		return service, nil
	}
	if len(entries) > 64 {
		return nil, fmt.Errorf("decision-primitives exceeds 64 entries")
	}
	identities := make(map[string]any, len(entries))
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		entry := entries[name]
		entry.Agents = slices.Clone(entry.Agents)
		entry.Inputs = maps.Clone(entry.Inputs)
		entry.Options = slices.Clone(entry.Options)
		entry.BlockOn = slices.Clone(entry.BlockOn)
		if entry.Range != nil {
			entry.Range = new(*entry.Range)
		}
		if entry.MinConfidence != nil {
			entry.MinConfidence = new(*entry.MinConfidence)
		}
		if entry.Endpoint == "" && entry.Backend == "systemone" {
			entry.Endpoint = DefaultEndpoint
		}
		if entry.Timeout == 0 {
			entry.Timeout = decisionrt.DefaultTimeout
		}
		if entry.MaxCalls == 0 {
			entry.MaxCalls = 100
		}
		if err := validateEntry(entry); err != nil {
			return nil, fmt.Errorf("decision-primitives.%s: %w", name, err)
		}
		spec := decisionrt.Spec{ID: name, Version: entry.Version, Kind: entry.Kind, Question: entry.Question}
		for _, option := range entry.Options {
			spec.Options = append(spec.Options, decisionrt.Option{ID: option.ID, Description: option.Description})
		}
		if entry.Range != nil {
			spec.Range = &decisionrt.IntegerRange{Min: entry.Range.Min, Max: entry.Range.Max}
		}
		if entry.Backend == "systemone" && spec.Range != nil && spec.Range.Min == spec.Range.Max {
			return nil, fmt.Errorf("decision-primitives.%s: systemone requires at least two integers", name)
		}
		key := ""
		if entry.APIKeyEnv != "" {
			if !envName.MatchString(entry.APIKeyEnv) {
				return nil, fmt.Errorf("decision-primitives.%s: invalid api-key-env", name)
			}
			var present bool
			key, present = os.LookupEnv(entry.APIKeyEnv)
			if !present || key == "" {
				return nil, fmt.Errorf("decision-primitives.%s: credential environment unavailable", name)
			}
		}
		primary, err := settings.primary(&entry, key)
		if err != nil {
			return nil, fmt.Errorf("decision-primitives.%s: %w", name, err)
		}
		var fallback decisionrt.Backend
		if entry.Fallback != "" {
			fallback, err = backend.New(backend.Config{Name: entry.Fallback})
			if err != nil {
				return nil, err
			}
		}
		runtime, err := decisionrt.NewRuntime(decisionrt.RuntimeConfig{Primary: primary, Fallback: fallback, Timeout: entry.Timeout, Policy: decisionrt.AcceptancePolicy{MinConfidence: entry.MinConfidence, RequireCalibratedConfidence: entry.RequireCalibrated}})
		if err != nil {
			return nil, fmt.Errorf("decision-primitives.%s: %w", name, err)
		}
		bound := boundEntry{entry: entry, spec: spec, runtime: runtime}
		if _, err := bound.request(name, inputExamples(entry.Inputs)); err != nil {
			return nil, fmt.Errorf("decision-primitives.%s: %w", name, err)
		}
		service.entries[name] = bound
		credentialHash := sha256.Sum256([]byte(key))
		identities[name] = struct {
			Entry          Entry
			CredentialHash string
		}{entry, hex.EncodeToString(credentialHash[:])}
	}
	encoded, err := json.Marshal(identities)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(encoded)
	service.hash = hex.EncodeToString(hash[:])
	return service, service.freezeDescription()
}

// validateEntry checks an entry's backend, fallback, grants, limits and
// block-on before any backend is built.
func validateEntry(entry Entry) error {
	if entry.Backend != "systemone" && entry.Backend != "sidecar" && entry.Backend != "rule" {
		return fmt.Errorf("backend must be systemone, sidecar or rule")
	}
	if entry.Fallback != "" && (entry.Fallback != "rule" || entry.Backend == "rule") {
		return fmt.Errorf("fallback must be rule for systemone or sidecar")
	}
	if entry.Backend == "sidecar" {
		if err := validateSidecarEntry(entry); err != nil {
			return err
		}
	}
	if len(entry.Agents) == 0 || len(entry.Agents) > 64 || len(entry.Inputs) > 64 || entry.MaxCalls < 1 || entry.MaxCalls > 10000 {
		return fmt.Errorf("invalid grants or limits")
	}
	seen := make(map[string]bool)
	for _, agent := range entry.Agents {
		if agent == "" || seen[agent] {
			return fmt.Errorf("invalid agent grant")
		}
		seen[agent] = true
	}
	return validateBlockOn(entry)
}

// validateBlockOn accepts block-on only for choice decisions, naming each
// option once.
func validateBlockOn(entry Entry) error {
	if len(entry.BlockOn) == 0 {
		return nil
	}
	if entry.Kind != decisionrt.KindChoice {
		return fmt.Errorf("block-on requires kind choice")
	}
	seen := make(map[string]bool, len(entry.BlockOn))
	for _, value := range entry.BlockOn {
		known := slices.ContainsFunc(entry.Options, func(option Option) bool { return option.ID == value })
		if !known || seen[value] {
			return fmt.Errorf("block-on value %q must name a distinct option", value)
		}
		seen[value] = true
	}
	return nil
}

// Blocks reports whether a decided choice stops the calling worker's attempt.
func (s *Service) Blocks(name, choice string) bool {
	if s == nil {
		return false
	}
	bound, ok := s.entries[name]
	return ok && choice != "" && slices.Contains(bound.entry.BlockOn, choice)
}

func (s *Service) freezeDescription() error {
	contracts := make(map[string]any, len(s.entries))
	for name, bound := range s.entries {
		contracts[name] = struct {
			Spec    decisionrt.Spec   `json:"spec"`
			Inputs  map[string]string `json:"inputs"`
			Agents  []string          `json:"agents"`
			BlockOn []string          `json:"block_on,omitempty"`
		}{bound.spec, bound.entry.Inputs, bound.entry.Agents, bound.entry.BlockOn}
	}
	encoded, err := json.Marshal(contracts)
	if err != nil {
		return err
	}
	if len(encoded) > 256<<10 {
		return fmt.Errorf("decision-primitives tool catalog exceeds 256 KiB")
	}
	s.description = string(encoded)
	return nil
}

// ToolDescription exposes each public contract once, rather than duplicating
// it for every grantee. It is frozen with the service and contains no transport.
func (s *Service) ToolDescription() string {
	if s == nil {
		return "{}"
	}
	return s.description
}

func inputExamples(inputs map[string]string) map[string]any {
	examples := make(map[string]any, len(inputs))
	for name, kind := range inputs {
		switch kind {
		case "string":
			examples[name] = ""
		case "boolean":
			examples[name] = false
		case "number":
			examples[name] = float64(0)
		default:
			examples[name] = nil
		}
	}
	return examples
}

func (s *Service) Hash() string {
	if s == nil {
		return ""
	}
	return s.hash
}

// Description returns only the decision contract; transport and credentials
// are never exposed in the model's tool schema.
func (s *Service) Description(agent string) string {
	contracts := make(map[string]any)
	for _, name := range s.Names(agent) {
		bound := s.entries[name]
		contracts[name] = struct {
			Spec    decisionrt.Spec   `json:"spec"`
			Inputs  map[string]string `json:"inputs"`
			BlockOn []string          `json:"block_on,omitempty"`
		}{bound.spec, bound.entry.Inputs, bound.entry.BlockOn}
	}
	encoded, _ := json.Marshal(contracts)
	return string(encoded)
}

func (s *Service) Names(agent string) []string {
	var names []string
	if s == nil {
		return names
	}
	for name, bound := range s.entries {
		if slices.Contains(bound.entry.Agents, agent) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func (s *Service) Request(name, agent string, values map[string]any) (decisionrt.Request, int, error) {
	if s == nil {
		return decisionrt.Request{}, 0, fmt.Errorf("decision service unavailable")
	}
	bound, ok := s.entries[name]
	if !ok || !slices.Contains(bound.entry.Agents, agent) {
		return decisionrt.Request{}, 0, fmt.Errorf("decision not granted")
	}
	request, err := bound.request(name, values)
	return request, bound.entry.MaxCalls, err
}

func (b boundEntry) request(name string, values map[string]any) (decisionrt.Request, error) {
	if len(values) != len(b.entry.Inputs) {
		return decisionrt.Request{}, fmt.Errorf("decision context does not match required inputs")
	}
	for key, kind := range b.entry.Inputs {
		value, exists := values[key]
		valid := false
		switch kind {
		case "string":
			_, valid = value.(string)
		case "boolean":
			_, valid = value.(bool)
		case "number":
			switch value.(type) {
			case float64, json.Number:
				valid = true
			}
		}
		if !exists || !valid {
			return decisionrt.Request{}, fmt.Errorf("decision context field %q has invalid type", key)
		}
	}
	spec := b.spec
	spec.Options = slices.Clone(spec.Options)
	if spec.Range != nil {
		spec.Range = new(*spec.Range)
	}
	request := decisionrt.Request{Purpose: name, Spec: spec, Context: maps.Clone(values)}
	return request, request.Validate()
}

func (s *Service) Decide(ctx context.Context, name string, request decisionrt.Request) (decisionrt.Result, decisionrt.Receipt, error) {
	bound, ok := s.entries[name]
	if !ok {
		return decisionrt.Result{}, decisionrt.Receipt{}, fmt.Errorf("decision unavailable")
	}
	canonical, err := bound.request(name, request.Context)
	if err != nil {
		return decisionrt.Result{}, decisionrt.Receipt{}, err
	}
	return bound.runtime.Decide(ctx, canonical)
}

// ValidatePublication rejects corrupted or stale receipts before replay.
func (s *Service) ValidatePublication(name, digest string, result decisionrt.Result, receipt decisionrt.Receipt) error {
	bound, ok := s.entries[name]
	if !ok {
		return fmt.Errorf("decision unavailable")
	}
	if err := decisionrt.ValidateResult(bound.spec, result); err != nil {
		return err
	}
	backendName, model := bound.entry.Backend, bound.entry.Model
	if backendName == "rule" {
		model = ""
	}
	if result.FallbackUsed {
		if bound.entry.Fallback != "rule" || result.Status != decisionrt.StatusAbstained {
			return fmt.Errorf("invalid decision fallback")
		}
		backendName, model = "rule", ""
	}
	if result.Backend != backendName || result.Model != model || receipt.SchemaVersion != 1 || receipt.Purpose != name || receipt.SpecID != name || receipt.SpecVersion != bound.spec.Version || receipt.RequestDigest != digest || receipt.Backend != result.Backend || receipt.Model != result.Model || receipt.FallbackUsed != result.FallbackUsed || receipt.Status != result.Status || receipt.ReasonCode != result.ReasonCode {
		return fmt.Errorf("decision receipt contract mismatch")
	}
	if backendName == "rule" && (result.Status != decisionrt.StatusAbstained || result.ReasonCode != "backend_abstained") {
		return fmt.Errorf("invalid rule result")
	}
	if backendName == "systemone" && result.Status == decisionrt.StatusDecided && result.ConfidenceSemantics != decisionrt.ConfidenceRaw {
		return fmt.Errorf("invalid systemone confidence semantics")
	}
	if backendName == "sidecar" && result.Status == decisionrt.StatusDecided && result.ConfidenceSemantics != decisionrt.ConfidenceNone {
		return fmt.Errorf("invalid sidecar confidence semantics")
	}
	if result.Status == decisionrt.StatusDecided && (bound.entry.RequireCalibrated && result.ConfidenceSemantics != decisionrt.ConfidenceCalibrated || bound.entry.MinConfidence != nil && (result.ConfidenceSemantics == decisionrt.ConfidenceNone || result.Confidence < *bound.entry.MinConfidence)) {
		return fmt.Errorf("decision does not satisfy policy")
	}
	return nil
}
