package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/decisionrt"
)

const decisionRTMaximumInputBytes = 1 << 20

type decisionRTInputOptions struct {
	file  string
	stdin bool
}

type decisionRTContextOptions struct {
	values   []string
	jsonText string
	file     string
}

func readDecisionRTRequest(input decisionRTInputOptions, stdin io.Reader, stdinIsTerminal bool) (decisionrt.Request, error) {
	if input.file != "" && input.stdin {
		return decisionrt.Request{}, decisionRTInvalidRequest("--file and --stdin are mutually exclusive")
	}
	var reader io.Reader
	switch {
	case input.file != "":
		file, err := os.Open(input.file)
		if err != nil {
			return decisionrt.Request{}, decisionRTInvalidRequest("cannot read input file")
		}
		defer func() { _ = file.Close() }()
		reader = file
	case input.stdin || !stdinIsTerminal:
		reader = stdin
	default:
		return decisionrt.Request{}, decisionRTInvalidRequest("request input is required")
	}

	data, err := readDecisionRTLimited(reader)
	if err != nil {
		return decisionrt.Request{}, err
	}
	var request decisionrt.Request
	if err := decodeDecisionRTJSON(data, &request, true); err != nil {
		return decisionrt.Request{}, decisionRTInvalidRequest("invalid request JSON")
	}
	return request, nil
}

func buildDecisionRTContext(options decisionRTContextOptions) (map[string]any, error) {
	contextValues := make(map[string]any)
	if options.file != "" {
		file, err := os.Open(options.file)
		if err != nil {
			return nil, decisionRTInvalidRequest("cannot read context file")
		}
		data, readErr := readDecisionRTLimited(file)
		closeErr := file.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, decisionRTInvalidRequest("cannot read context file")
		}
		decoded, err := decodeDecisionRTContextObject(data)
		if err != nil {
			return nil, err
		}
		mergeDecisionRTContext(contextValues, decoded)
	}
	if options.jsonText != "" {
		decoded, err := decodeDecisionRTContextObject([]byte(options.jsonText))
		if err != nil {
			return nil, err
		}
		mergeDecisionRTContext(contextValues, decoded)
	}

	seen := make(map[string]struct{}, len(options.values))
	for _, assignment := range options.values {
		key, text, ok := strings.Cut(assignment, "=")
		if !ok || key == "" {
			return nil, decisionRTInvalidRequest("invalid --context value")
		}
		if _, duplicate := seen[key]; duplicate {
			return nil, decisionRTInvalidRequest("duplicate --context key")
		}
		seen[key] = struct{}{}
		value, err := parseDecisionRTContextScalar(text)
		if err != nil {
			return nil, err
		}
		contextValues[key] = value
	}
	return contextValues, nil
}

func parseDecisionRTContextScalar(text string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return text, nil
	}
	if err := requireDecisionRTJSONEOF(decoder); err != nil {
		return text, nil
	}
	switch value.(type) {
	case string, bool, json.Number:
		return value, nil
	case nil, []any, map[string]any:
		return nil, decisionRTInvalidRequest("--context value must be a JSON scalar")
	default:
		return nil, decisionRTInvalidRequest("unsupported --context value")
	}
}

func decodeDecisionRTContextObject(data []byte) (map[string]any, error) {
	var contextValues map[string]any
	if err := decodeDecisionRTJSON(data, &contextValues, false); err != nil || contextValues == nil {
		return nil, decisionRTInvalidRequest("context must be a JSON object")
	}
	return contextValues, nil
}

func decodeDecisionRTJSON(data []byte, destination any, disallowUnknown bool) error {
	if len(data) == 0 || !utf8.Valid(data) {
		return errors.New("invalid JSON input")
	}
	if err := rejectDecisionRTDuplicateJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if disallowUnknown {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	return requireDecisionRTJSONEOF(decoder)
}

func rejectDecisionRTDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanDecisionRTJSONValue(decoder); err != nil {
		return err
	}
	return requireDecisionRTJSONEOF(decoder)
}

func scanDecisionRTJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("invalid JSON object key")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("duplicate JSON object key")
			}
			seen[key] = struct{}{}
			if err := scanDecisionRTJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case '[':
		for decoder.More() {
			if err := scanDecisionRTJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}

func requireDecisionRTJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func readDecisionRTLimited(reader io.Reader) ([]byte, error) {
	limited := io.LimitReader(reader, decisionRTMaximumInputBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, decisionRTInvalidRequest("cannot read input")
	}
	if len(data) > decisionRTMaximumInputBytes {
		return nil, decisionRTInvalidRequest("input exceeds 1 MiB")
	}
	return data, nil
}

func mergeDecisionRTContext(target, source map[string]any) {
	for key, value := range source {
		target[key] = value
	}
}

func decisionRTInvalidRequest(message string) error {
	return &decisionrt.RuntimeError{Kind: decisionrt.ErrorInvalidRequest, Err: errors.New(message)}
}
