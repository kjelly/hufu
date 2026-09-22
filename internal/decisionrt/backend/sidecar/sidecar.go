package sidecar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/decisionrt"
)

const (
	backendName        = "sidecar"
	maximumPromptRunes = 8000
	maximumResponseLen = 4096
)

type Generator interface {
	Execute(context.Context, string) (string, error)
	ModelID() string
}

type backend struct {
	generator Generator
	modelID   string
}

type promptCandidate struct {
	Token       string `json:"token"`
	Value       string `json:"value"`
	Description string `json:"description"`
}

type promptInput struct {
	Purpose     string            `json:"purpose"`
	SpecID      string            `json:"spec_id"`
	SpecVersion string            `json:"spec_version"`
	Question    string            `json:"question"`
	Context     map[string]any    `json:"context"`
	Candidates  []promptCandidate `json:"candidates"`
}

type responsePayload struct {
	Token string `json:"token"`
}

func New(generator Generator) (decisionrt.Backend, error) {
	if isNilGenerator(generator) {
		return nil, configurationError("sidecar generator is required")
	}
	modelID := generator.ModelID()
	if !validModelID(modelID) {
		return nil, configurationError("invalid sidecar model ID")
	}
	return &backend{generator: generator, modelID: modelID}, nil
}

func (b *backend) Name() string {
	return backendName
}

func (b *backend) Decide(ctx context.Context, request decisionrt.Request) (decisionrt.BackendResult, error) {
	prompt, mapping, err := buildPrompt(request)
	if err != nil {
		return decisionrt.BackendResult{}, err
	}
	if utf8.RuneCountInString(prompt) > maximumPromptRunes {
		return decisionrt.BackendResult{}, backendFailure("sidecar prompt exceeds 8000 runes")
	}

	response, err := b.generator.Execute(ctx, prompt)
	if err != nil {
		return decisionrt.BackendResult{}, &decisionrt.RuntimeError{
			Kind:    decisionrt.ErrorBackendFailure,
			Backend: backendName,
			Err:     err,
		}
	}
	token, err := parseResponse(response)
	if err != nil {
		return decisionrt.BackendResult{}, err
	}
	value, ok := mapping[token]
	if !ok {
		return decisionrt.BackendResult{}, invalidOutput("unknown sidecar candidate token")
	}
	return decisionrt.BackendResult{
		Status:              decisionrt.StatusDecided,
		Value:               value,
		ConfidenceSemantics: decisionrt.ConfidenceNone,
		Model:               b.modelID,
	}, nil
}

func buildPrompt(request decisionrt.Request) (string, map[string]decisionrt.Value, error) {
	if err := request.Validate(); err != nil {
		return "", nil, err
	}
	candidates, mapping := candidateMapping(request.Spec)
	contextValues := canonicalPromptContext(request.Context)
	input := promptInput{
		Purpose:     request.Purpose,
		SpecID:      request.Spec.ID,
		SpecVersion: request.Spec.Version,
		Question:    request.Spec.Question,
		Context:     contextValues,
		Candidates:  candidates,
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", nil, backendFailure("sidecar prompt encoding failed")
	}
	prompt := "Choose exactly one candidate token from this JSON input.\nInput:\n" + string(encoded) +
		"\n\nReturn only one JSON object. TOKEN must be replaced by exactly one candidate token:\n{\"token\":\"TOKEN\"}"
	return prompt, mapping, nil
}

func canonicalPromptContext(values map[string]any) map[string]any {
	canonical := make(map[string]any, len(values))
	for key, value := range values {
		if number, ok := value.(json.Number); ok {
			canonical[key] = canonicalPromptJSONNumber(number)
			continue
		}
		reflected := reflect.ValueOf(value)
		switch reflected.Kind() {
		case reflect.String:
			canonical[key] = reflected.String()
		case reflect.Bool:
			canonical[key] = reflected.Bool()
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			canonical[key] = json.Number(strconv.FormatInt(reflected.Int(), 10))
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			canonical[key] = json.Number(strconv.FormatUint(reflected.Uint(), 10))
		case reflect.Float32, reflect.Float64:
			canonical[key] = canonicalPromptFloat(reflected.Float())
		}
	}
	return canonical
}

func canonicalPromptJSONNumber(number json.Number) json.Number {
	if integer, err := number.Int64(); err == nil {
		return json.Number(strconv.FormatInt(integer, 10))
	}
	value, _ := strconv.ParseFloat(number.String(), 64)
	return canonicalPromptFloat(value)
}

func canonicalPromptFloat(value float64) json.Number {
	const upperInt64Bound = float64(uint64(1) << 63)
	if math.Trunc(value) == value && value >= math.MinInt64 && value < upperInt64Bound {
		return json.Number(strconv.FormatInt(int64(value), 10))
	}
	return json.Number(strconv.FormatFloat(value, 'g', -1, 64))
}

func candidateMapping(spec decisionrt.Spec) ([]promptCandidate, map[string]decisionrt.Value) {
	var candidates []promptCandidate
	mapping := make(map[string]decisionrt.Value)
	appendCandidate := func(value, description string, typed decisionrt.Value) {
		token := "A" + strconv.Itoa(len(candidates))
		candidates = append(candidates, promptCandidate{Token: token, Value: value, Description: description})
		mapping[token] = typed
	}

	switch spec.Kind {
	case decisionrt.KindChoice:
		for _, option := range spec.Options {
			appendCandidate(option.ID, option.Description, decisionrt.Value{Choice: option.ID})
		}
	case decisionrt.KindBoolean:
		appendCandidate("false", "", decisionrt.Value{Boolean: new(false)})
		appendCandidate("true", "", decisionrt.Value{Boolean: new(true)})
	case decisionrt.KindIntegerRange:
		width := uint64(spec.Range.Max) - uint64(spec.Range.Min)
		for offset := range width + 1 {
			value := spec.Range.Min + int64(offset)
			appendCandidate(strconv.FormatInt(value, 10), "", decisionrt.Value{Integer: new(value)})
		}
	}
	return candidates, mapping
}

func parseResponse(response string) (string, error) {
	if len(response) == 0 || len(response) > maximumResponseLen || !utf8.ValidString(response) {
		return "", invalidOutput("invalid sidecar response")
	}
	if err := rejectDuplicateOrUnknownKeys(response); err != nil {
		return "", err
	}

	decoder := json.NewDecoder(strings.NewReader(response))
	decoder.DisallowUnknownFields()
	var payload responsePayload
	if err := decoder.Decode(&payload); err != nil {
		return "", invalidOutput("invalid sidecar response")
	}
	if err := requireEOF(decoder); err != nil {
		return "", err
	}
	if payload.Token == "" {
		return "", invalidOutput("invalid sidecar response")
	}
	return payload.Token, nil
}

func rejectDuplicateOrUnknownKeys(response string) error {
	decoder := json.NewDecoder(strings.NewReader(response))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return invalidOutput("invalid sidecar response")
	}
	seenToken := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return invalidOutput("invalid sidecar response")
		}
		key, ok := keyToken.(string)
		if !ok || key != "token" || seenToken {
			return invalidOutput("invalid sidecar response")
		}
		seenToken = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return invalidOutput("invalid sidecar response")
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || !seenToken {
		return invalidOutput("invalid sidecar response")
	}
	return requireEOF(decoder)
}

func requireEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return invalidOutput("invalid sidecar response")
	}
	return nil
}

func validModelID(modelID string) bool {
	if modelID == "" || strings.TrimSpace(modelID) != modelID || len(modelID) > 256 || !utf8.ValidString(modelID) {
		return false
	}
	for _, character := range modelID {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func isNilGenerator(generator Generator) bool {
	if generator == nil {
		return true
	}
	value := reflect.ValueOf(generator)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func configurationError(message string) error {
	return &decisionrt.RuntimeError{Kind: decisionrt.ErrorConfiguration, Backend: backendName, Err: fmt.Errorf("%s", message)}
}

func backendFailure(message string) error {
	return &decisionrt.RuntimeError{Kind: decisionrt.ErrorBackendFailure, Backend: backendName, Err: fmt.Errorf("%s", message)}
}

func invalidOutput(message string) error {
	return &decisionrt.RuntimeError{Kind: decisionrt.ErrorInvalidBackendOutput, Backend: backendName, Err: fmt.Errorf("%s", message)}
}
