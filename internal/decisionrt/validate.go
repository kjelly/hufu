package decisionrt

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxChoiceOptions       = 21
	maxQuestionBytes       = 4096
	maxDescriptionBytes    = 1024
	maxContextEntries      = 64
	maxContextStringBytes  = 4096
	maxCanonicalContextLen = 64 << 10
	maxModelBytes          = 256
)

var machineIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,127}$`)
var backendNamePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,127}$`)

type canonicalContextEntry struct {
	Key   string `json:"key"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

func (s Spec) Validate() error {
	if !machineIDPattern.MatchString(s.ID) || !machineIDPattern.MatchString(s.Version) {
		return runtimeError(ErrorInvalidRequest, "", fmt.Errorf("invalid spec identifier"))
	}
	if !utf8.ValidString(s.Question) || strings.TrimSpace(s.Question) == "" || len(s.Question) > maxQuestionBytes {
		return runtimeError(ErrorInvalidRequest, "", fmt.Errorf("invalid question"))
	}

	switch s.Kind {
	case KindChoice:
		if len(s.Options) < 2 || len(s.Options) > maxChoiceOptions || s.Range != nil {
			return runtimeError(ErrorInvalidRequest, "", fmt.Errorf("invalid choice domain"))
		}
		seen := make(map[string]struct{}, len(s.Options))
		for _, option := range s.Options {
			if !machineIDPattern.MatchString(option.ID) || !utf8.ValidString(option.Description) || len(option.Description) > maxDescriptionBytes {
				return runtimeError(ErrorInvalidRequest, "", fmt.Errorf("invalid choice option"))
			}
			if _, ok := seen[option.ID]; ok {
				return runtimeError(ErrorInvalidRequest, "", fmt.Errorf("duplicate choice option"))
			}
			seen[option.ID] = struct{}{}
		}
	case KindBoolean:
		if len(s.Options) != 0 || s.Range != nil {
			return runtimeError(ErrorInvalidRequest, "", fmt.Errorf("boolean cannot declare a domain"))
		}
	case KindIntegerRange:
		if len(s.Options) != 0 || s.Range == nil || s.Range.Min > s.Range.Max || integerRangeWidth(s.Range.Min, s.Range.Max) > 20 {
			return runtimeError(ErrorInvalidRequest, "", fmt.Errorf("invalid integer range"))
		}
	default:
		return runtimeError(ErrorInvalidRequest, "", fmt.Errorf("unknown decision kind"))
	}

	return nil
}

func (r Request) Validate() error {
	if !machineIDPattern.MatchString(r.Purpose) {
		return runtimeError(ErrorInvalidRequest, "", fmt.Errorf("invalid purpose"))
	}
	if err := r.Spec.Validate(); err != nil {
		return err
	}
	_, err := canonicalizeContext(r.Context)
	return err
}

func integerRangeWidth(minimum, maximum int64) uint64 {
	return uint64(maximum) - uint64(minimum)
}

func canonicalizeContext(values map[string]any) ([]canonicalContextEntry, error) {
	if len(values) > maxContextEntries {
		return nil, runtimeError(ErrorInvalidRequest, "", fmt.Errorf("too many context entries"))
	}

	entries := make([]canonicalContextEntry, 0, len(values))
	for key, value := range values {
		if !machineIDPattern.MatchString(key) {
			return nil, runtimeError(ErrorInvalidRequest, "", fmt.Errorf("invalid context key"))
		}
		entryType, encoded, err := canonicalContextValue(value)
		if err != nil {
			return nil, err
		}
		entries = append(entries, canonicalContextEntry{Key: key, Type: entryType, Value: encoded})
	}

	slices.SortFunc(entries, func(a, b canonicalContextEntry) int {
		return cmp.Compare(a.Key, b.Key)
	})
	encoded, err := json.Marshal(entries)
	if err != nil {
		return nil, runtimeError(ErrorInvalidRequest, "", err)
	}
	if len(encoded) > maxCanonicalContextLen {
		return nil, runtimeError(ErrorInvalidRequest, "", fmt.Errorf("canonical context is too large"))
	}
	return entries, nil
}

func canonicalContextValue(value any) (string, string, error) {
	if value == nil {
		return "", "", runtimeError(ErrorInvalidRequest, "", fmt.Errorf("nil context value"))
	}
	if number, ok := value.(json.Number); ok {
		encoded, err := canonicalJSONNumber(number)
		return "number", encoded, err
	}

	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.String:
		text := reflected.String()
		if !utf8.ValidString(text) || len(text) > maxContextStringBytes {
			return "", "", runtimeError(ErrorInvalidRequest, "", fmt.Errorf("invalid context string"))
		}
		return "string", text, nil
	case reflect.Bool:
		return "bool", strconv.FormatBool(reflected.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "number", strconv.FormatInt(reflected.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		unsigned := reflected.Uint()
		if unsigned > math.MaxInt64 {
			return "", "", runtimeError(ErrorInvalidRequest, "", fmt.Errorf("context integer exceeds int64"))
		}
		return "number", strconv.FormatInt(int64(unsigned), 10), nil
	case reflect.Float32, reflect.Float64:
		return canonicalFloat(reflected.Float())
	default:
		return "", "", runtimeError(ErrorInvalidRequest, "", fmt.Errorf("unsupported context value"))
	}
}

func canonicalJSONNumber(number json.Number) (string, error) {
	if integer, err := number.Int64(); err == nil {
		return strconv.FormatInt(integer, 10), nil
	}
	text := number.String()
	if !strings.ContainsAny(text, ".eE") {
		return "", runtimeError(ErrorInvalidRequest, "", fmt.Errorf("JSON integer exceeds int64"))
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return "", runtimeError(ErrorInvalidRequest, "", fmt.Errorf("invalid JSON number"))
	}
	_, encoded, err := canonicalFloat(value)
	return encoded, err
}

func canonicalFloat(value float64) (string, string, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "", "", runtimeError(ErrorInvalidRequest, "", fmt.Errorf("non-finite context number"))
	}
	const upperInt64Bound = float64(uint64(1) << 63)
	if math.Trunc(value) == value && value >= math.MinInt64 && value < upperInt64Bound {
		return "number", strconv.FormatInt(int64(value), 10), nil
	}
	return "number", strconv.FormatFloat(value, 'g', -1, 64), nil
}

func validateBackendResult(spec Spec, result BackendResult) error {
	if err := validateModel(result.Model); err != nil {
		return err
	}

	switch result.Status {
	case StatusAbstained:
		if !isZeroValue(result.Value) || len(result.Candidates) != 0 || result.Confidence != 0 || result.ConfidenceSemantics != ConfidenceNone {
			return invalidBackendOutput("invalid abstained result")
		}
		return nil
	case StatusDecided:
		if err := validateValue(spec, result.Value); err != nil {
			return err
		}
	default:
		return invalidBackendOutput("unknown result status")
	}

	if err := validateConfidence(result.Confidence, result.ConfidenceSemantics); err != nil {
		return err
	}
	if err := validateCandidates(spec, result.Value, result.Candidates, result.Confidence, result.ConfidenceSemantics); err != nil {
		return err
	}
	return nil
}

func validateValue(spec Spec, value Value) error {
	fields := 0
	if value.Choice != "" {
		fields++
	}
	if value.Boolean != nil {
		fields++
	}
	if value.Integer != nil {
		fields++
	}
	if fields != 1 {
		return invalidBackendOutput("decision value must contain exactly one field")
	}

	switch spec.Kind {
	case KindChoice:
		if value.Boolean != nil || value.Integer != nil {
			return invalidBackendOutput("wrong value kind")
		}
		for _, option := range spec.Options {
			if value.Choice == option.ID {
				return nil
			}
		}
	case KindBoolean:
		if value.Boolean != nil && value.Choice == "" && value.Integer == nil {
			return nil
		}
	case KindIntegerRange:
		if value.Integer != nil && value.Choice == "" && value.Boolean == nil && *value.Integer >= spec.Range.Min && *value.Integer <= spec.Range.Max {
			return nil
		}
	}
	return invalidBackendOutput("decision value is outside the declared domain")
}

func validateConfidence(confidence float64, semantics ConfidenceSemantics) error {
	switch semantics {
	case ConfidenceNone:
		if confidence != 0 {
			return invalidBackendOutput("confidence without semantics")
		}
	case ConfidenceRaw, ConfidenceCalibrated:
		if math.IsNaN(confidence) || math.IsInf(confidence, 0) || confidence < 0 || confidence > 1 {
			return invalidBackendOutput("invalid confidence")
		}
	default:
		return invalidBackendOutput("unknown confidence semantics")
	}
	return nil
}

func validateCandidates(spec Spec, selected Value, candidates []Candidate, confidence float64, semantics ConfidenceSemantics) error {
	if len(candidates) == 0 {
		return nil
	}
	domain := decisionDomain(spec)
	if len(candidates) != len(domain) {
		return invalidBackendOutput("candidate distribution is incomplete")
	}
	allowed := make(map[string]struct{}, len(domain))
	for _, value := range domain {
		allowed[value] = struct{}{}
	}
	seen := make(map[string]struct{}, len(candidates))
	total := 0.0
	selectedValue := canonicalValue(selected)
	selectedProbability := 0.0
	for _, candidate := range candidates {
		if math.IsNaN(candidate.Probability) || math.IsInf(candidate.Probability, 0) || candidate.Probability < 0 || candidate.Probability > 1 {
			return invalidBackendOutput("invalid candidate probability")
		}
		if _, ok := allowed[candidate.Value]; !ok {
			return invalidBackendOutput("candidate is outside the declared domain")
		}
		if _, ok := seen[candidate.Value]; ok {
			return invalidBackendOutput("duplicate candidate")
		}
		seen[candidate.Value] = struct{}{}
		total += candidate.Probability
		if candidate.Value == selectedValue {
			selectedProbability = candidate.Probability
		}
	}
	if math.Abs(total-1) > 1e-6 {
		return invalidBackendOutput("candidate probabilities do not sum to one")
	}
	if semantics != ConfidenceNone && math.Abs(selectedProbability-confidence) > 1e-6 {
		return invalidBackendOutput("confidence does not match selected candidate")
	}
	return nil
}

func decisionDomain(spec Spec) []string {
	switch spec.Kind {
	case KindChoice:
		values := make([]string, 0, len(spec.Options))
		for _, option := range spec.Options {
			values = append(values, option.ID)
		}
		return values
	case KindBoolean:
		return []string{"false", "true"}
	case KindIntegerRange:
		values := make([]string, 0, integerRangeWidth(spec.Range.Min, spec.Range.Max)+1)
		for offset := range integerRangeWidth(spec.Range.Min, spec.Range.Max) + 1 {
			values = append(values, strconv.FormatInt(spec.Range.Min+int64(offset), 10))
		}
		return values
	default:
		return nil
	}
}

func canonicalValue(value Value) string {
	switch {
	case value.Choice != "":
		return value.Choice
	case value.Boolean != nil:
		return strconv.FormatBool(*value.Boolean)
	case value.Integer != nil:
		return strconv.FormatInt(*value.Integer, 10)
	default:
		return ""
	}
}

func validateModel(model string) error {
	if model == "" {
		return nil
	}
	if !utf8.ValidString(model) || len(model) > maxModelBytes {
		return invalidBackendOutput("invalid model identity")
	}
	for _, r := range model {
		if unicode.IsControl(r) {
			return invalidBackendOutput("invalid model identity")
		}
	}
	return nil
}

func isZeroValue(value Value) bool {
	return value.Choice == "" && value.Boolean == nil && value.Integer == nil
}

func invalidBackendOutput(message string) error {
	return runtimeError(ErrorInvalidBackendOutput, "", fmt.Errorf("%s", message))
}
