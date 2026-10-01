package systemone

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/decisionrt"
)

const (
	maximumJSONDepth      = 32
	probabilityTolerance  = 1e-6
	invalidResponseReason = "invalid response"
)

func decodeResult(data []byte, kind decisionrt.Kind, candidates []candidate) (decisionrt.BackendResult, error) {
	if !utf8.Valid(data) {
		return decisionrt.BackendResult{}, invalidOutput("response is not valid UTF-8")
	}
	if err := requireStrictJSON(data); err != nil {
		return decisionrt.BackendResult{}, err
	}
	answer, err := decodeAnswer(data)
	if err != nil {
		return decisionrt.BackendResult{}, err
	}
	if kind == decisionrt.KindBoolean {
		return decodeNoul(answer)
	}
	return decodeChoice(answer, candidates)
}

// decodeAnswer returns the fields of answers.decision, which must be the only
// answer.
func decodeAnswer(data []byte) (map[string]json.RawMessage, error) {
	top, ok := decodeObject(data)
	if !ok {
		return nil, invalidOutput(invalidResponseReason)
	}
	rawAnswers, ok := requiredField(top, "answers")
	if !ok {
		return nil, invalidOutput("missing answers")
	}
	answers, ok := decodeObject(rawAnswers)
	if !ok || len(answers) != 1 {
		return nil, invalidOutput("answers must contain exactly one decision")
	}
	rawAnswer, ok := requiredField(answers, questionKey)
	if !ok {
		return nil, invalidOutput("missing decision answer")
	}
	answer, ok := decodeObject(rawAnswer)
	if !ok {
		return nil, invalidOutput("invalid decision answer")
	}
	return answer, nil
}

func decodeChoice(answer map[string]json.RawMessage, candidates []candidate) (decisionrt.BackendResult, error) {
	if !hasStringField(answer, "type", nativeChoice) {
		return decisionrt.BackendResult{}, invalidOutput("answer type is not choice")
	}
	selectedKey, ok := stringField(answer, "choice")
	if !ok {
		return decisionrt.BackendResult{}, invalidOutput("missing choice")
	}
	if _, ok := probabilityField(answer, "confidence"); !ok {
		return decisionrt.BackendResult{}, invalidOutput("missing or invalid confidence")
	}
	rawProbabilities, ok := requiredField(answer, "probabilities")
	if !ok {
		return decisionrt.BackendResult{}, invalidOutput("missing probabilities")
	}
	probabilities, ok := decodeObject(rawProbabilities)
	if !ok || len(probabilities) != len(candidates) {
		return decisionrt.BackendResult{}, invalidOutput("probabilities do not match the declared candidates")
	}

	result := decisionrt.BackendResult{
		Status:              decisionrt.StatusDecided,
		Candidates:          make([]decisionrt.Candidate, 0, len(candidates)),
		ConfidenceSemantics: decisionrt.ConfidenceRaw,
	}
	selected := false
	total := 0.0
	for _, item := range candidates {
		probability, ok := probabilityField(probabilities, item.key)
		if !ok {
			return decisionrt.BackendResult{}, invalidOutput("missing or invalid candidate probability")
		}
		total += probability
		result.Candidates = append(result.Candidates, decisionrt.Candidate{Value: item.canonical, Probability: probability})
		if item.key == selectedKey {
			selected = true
			result.Value = item.value
			result.Confidence = probability
		}
	}
	if !selected {
		return decisionrt.BackendResult{}, invalidOutput("choice is not a declared candidate")
	}
	if math.Abs(total-1) > probabilityTolerance {
		return decisionrt.BackendResult{}, invalidOutput("candidate probabilities do not sum to one")
	}
	return result, nil
}

func decodeNoul(answer map[string]json.RawMessage) (decisionrt.BackendResult, error) {
	if !hasStringField(answer, "type", nativeNoul) {
		return decisionrt.BackendResult{}, invalidOutput("answer type is not noul")
	}
	probabilityTrue, ok := probabilityField(answer, "noul")
	if !ok {
		return decisionrt.BackendResult{}, invalidOutput("missing or invalid noul")
	}
	value := probabilityTrue > 0.5
	confidence := probabilityTrue
	if !value {
		confidence = 1 - probabilityTrue
	}
	return decisionrt.BackendResult{
		Status: decisionrt.StatusDecided,
		Value:  decisionrt.Value{Boolean: new(value)},
		Candidates: []decisionrt.Candidate{
			{Value: "false", Probability: 1 - probabilityTrue},
			{Value: "true", Probability: probabilityTrue},
		},
		Confidence:          confidence,
		ConfidenceSemantics: decisionrt.ConfidenceRaw,
	}, nil
}

// requiredField reports a field that is present and not JSON null.
func requiredField(object map[string]json.RawMessage, name string) (json.RawMessage, bool) {
	raw, ok := object[name]
	raw = bytes.TrimSpace(raw)
	if !ok || len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, false
	}
	return raw, true
}

func decodeObject(raw []byte) (map[string]json.RawMessage, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, false
	}
	return object, true
}

func stringField(object map[string]json.RawMessage, name string) (string, bool) {
	raw, ok := requiredField(object, name)
	if !ok || raw[0] != '"' {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func hasStringField(object map[string]json.RawMessage, name, want string) bool {
	value, ok := stringField(object, name)
	return ok && value == want
}

// probabilityField reports a present JSON number that is finite and in [0,1].
func probabilityField(object map[string]json.RawMessage, name string) (float64, bool) {
	raw, ok := requiredField(object, name)
	if !ok || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return 0, false
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
		return 0, false
	}
	return value, true
}

// requireStrictJSON accepts exactly one JSON document with no duplicate object
// names at any depth. encoding/json alone keeps the last duplicate silently.
func requireStrictJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := walkJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return invalidOutput("trailing data after the response document")
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maximumJSONDepth {
		return invalidOutput("response nesting is too deep")
	}
	token, err := decoder.Token()
	if err != nil {
		return invalidOutput("malformed JSON")
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			nameToken, err := decoder.Token()
			name, ok := nameToken.(string)
			if err != nil || !ok {
				return invalidOutput("malformed JSON")
			}
			if _, duplicate := seen[name]; duplicate {
				return invalidOutput("duplicate JSON object name")
			}
			seen[name] = struct{}{}
			if err := walkJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return invalidOutput("malformed JSON")
	}
	if _, err := decoder.Token(); err != nil {
		return invalidOutput("malformed JSON")
	}
	return nil
}
