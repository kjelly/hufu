package providerintrospection

import (
	"bufio"
	"errors"
	"strings"
)

// ParseOllamaParameters parses Ollama's line-oriented parameters field. It
// accepts optional PARAMETER prefixes, comments, blank lines, and preserves
// parameters unknown to Hufu.
func ParseOllamaParameters(raw string) map[string]string {
	parameters := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		line = stripComment(line)
		if line == "" {
			continue
		}
		fields := strings.FieldsSeq(line)
		parts := make([]string, 0, 3)
		for field := range fields {
			parts = append(parts, field)
		}
		if len(parts) == 0 {
			continue
		}
		if parts[0] == "PARAMETER" {
			parts = parts[1:]
		}
		if len(parts) == 0 || parts[0] == "" {
			continue
		}
		if len(parts) == 1 {
			key, value, found := strings.Cut(parts[0], "=")
			if found && key != "" {
				parameters[key] = value
			}
			continue
		}
		parameters[parts[0]] = strings.TrimSpace(strings.TrimPrefix(lineAfterKey(line, parts[0]), "="))
	}
	return parameters
}

func lineAfterKey(line, key string) string {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "PARAMETER") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "PARAMETER"))
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, key))
	return line
}

func stripComment(line string) string {
	inQuotes := false
	escaped := false
	for index, character := range line {
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' && inQuotes {
			escaped = true
			continue
		}
		if character == '"' {
			inQuotes = !inQuotes
			continue
		}
		if character == '#' && !inQuotes {
			return strings.TrimSpace(line[:index])
		}
	}
	return strings.TrimSpace(line)
}

// FindOllamaContextLength returns a context_length field only when all
// matching architecture fields agree. This intentionally does not know any
// model family or architecture name.
func FindOllamaContextLength(modelInfo map[string]any) (int, bool) {
	const suffix = ".context_length"
	var found int
	for key, value := range modelInfo {
		if !strings.HasSuffix(key, suffix) || len(key) == len(suffix) {
			continue
		}
		candidate, ok := intValue(value)
		if !ok {
			continue
		}
		if found == 0 {
			found = candidate
			continue
		}
		if found != candidate {
			return 0, false
		}
	}
	return found, found > 0
}

// ParseOllamaShow parses one decoded /api/show response.
func ParseOllamaShow(raw map[string]any) (RuntimeModelInfo, error) {
	info := RuntimeModelInfo{Raw: cloneMap(raw)}
	if modelID, ok := raw["model"].(string); ok {
		info.ModelID = modelID
	}
	if parameters, present := raw["parameters"]; present {
		parameterText, ok := parameters.(string)
		if !ok {
			return RuntimeModelInfo{}, errors.New("parameters is not text")
		}
		info.Parameters = ParseOllamaParameters(parameterText)
		if value, ok := intValue(info.Parameters["num_ctx"]); ok {
			info.ConfiguredContext = value
		}
		if value, ok := intValue(info.Parameters["num_predict"]); ok {
			info.MaxOutputTokens = value
		}
	}
	if modelInfo, present := raw["model_info"]; present {
		var ok bool
		info.ModelInfo, ok = modelInfo.(map[string]any)
		if !ok {
			return RuntimeModelInfo{}, errors.New("model_info is not an object")
		}
		info.ModelInfo = cloneMap(info.ModelInfo)
		if contextLength, ok := FindOllamaContextLength(info.ModelInfo); ok {
			info.ModelMaxContext = contextLength
		}
	}
	if capabilities, present := raw["capabilities"]; present {
		parsed, err := parseCapabilities(capabilities)
		if err != nil {
			return RuntimeModelInfo{}, err
		}
		info.Capabilities = parsed
	}
	if details, present := raw["details"]; present {
		parsed, ok := details.(map[string]any)
		if !ok {
			return RuntimeModelInfo{}, errors.New("details is not an object")
		}
		info.Family, _ = parsed["family"].(string)
		info.ParameterSize, _ = parsed["parameter_size"].(string)
		info.Quantization, _ = parsed["quantization_level"].(string)
	}
	return info, nil
}

func parseCapabilities(value any) ([]string, error) {
	switch typed := value.(type) {
	case []any:
		capabilities := make([]string, 0, len(typed))
		for _, item := range typed {
			capability, ok := item.(string)
			if !ok {
				return nil, errors.New("capabilities contains a non-string")
			}
			capabilities = append(capabilities, capability)
		}
		return capabilities, nil
	case []string:
		return append([]string(nil), typed...), nil
	case string:
		if typed == "" {
			return nil, nil
		}
		return strings.Fields(typed), nil
	default:
		return nil, errors.New("capabilities is not a list")
	}
}

// ParseOllamaPS finds the requested loaded model in a decoded /api/ps
// response. A missing model is a normal result, not an error.
func ParseOllamaPS(raw map[string]any, requestedModel string) (RuntimeModelInfo, bool, error) {
	value, present := raw["models"]
	if !present {
		return RuntimeModelInfo{}, false, nil
	}
	models, ok := value.([]any)
	if !ok {
		return RuntimeModelInfo{}, false, errors.New("models is not a list")
	}
	for _, item := range models {
		model, ok := item.(map[string]any)
		if !ok {
			return RuntimeModelInfo{}, false, errors.New("models contains a non-object")
		}
		if !matchesModel(model, requestedModel) {
			continue
		}
		info := RuntimeModelInfo{ModelID: requestedModel, Raw: cloneMap(model)}
		if modelName, ok := model["name"].(string); ok && modelName != "" {
			info.ModelID = modelName
		} else if modelName, ok := model["model"].(string); ok && modelName != "" {
			info.ModelID = modelName
		}
		if value, ok := intValue(model["context_length"]); ok {
			info.RuntimeContext = value
		}
		info.Size, _ = int64Value(model["size"])
		info.SizeVRAM, _ = int64Value(model["size_vram"])
		info.ExpiresAt, _ = model["expires_at"].(string)
		return info, true, nil
	}
	return RuntimeModelInfo{}, false, nil
}

// ParseOpenAIModels parses a common /v1/models response and returns the
// matching model entry.
func ParseOpenAIModels(raw map[string]any, requestedModel string) (RuntimeModelInfo, error) {
	value, present := raw["data"]
	if !present {
		return RuntimeModelInfo{}, errors.New("data is missing")
	}
	models, ok := value.([]any)
	if !ok {
		return RuntimeModelInfo{}, errors.New("data is not a list")
	}
	for _, item := range models {
		model, ok := item.(map[string]any)
		if !ok {
			return RuntimeModelInfo{}, errors.New("data contains a non-object")
		}
		modelID, _ := model["id"].(string)
		if !matchesRequestedModel(modelID, requestedModel) {
			continue
		}
		info := RuntimeModelInfo{ModelID: modelID, Raw: cloneMap(model)}
		for _, key := range []string{"context_length", "max_context_window", "max_input_tokens"} {
			if candidate, ok := intValue(model[key]); ok {
				info.ModelMaxContext = candidate
				break
			}
		}
		for _, key := range []string{"max_output_tokens", "max_completion_tokens"} {
			if candidate, ok := intValue(model[key]); ok {
				info.MaxOutputTokens = candidate
				break
			}
		}
		return info, nil
	}
	return RuntimeModelInfo{}, errors.New("requested model not found")
}

func matchesModel(model map[string]any, requested string) bool {
	for _, key := range []string{"name", "model"} {
		if value, ok := model[key].(string); ok && matchesRequestedModel(value, requested) {
			return true
		}
	}
	return false
}

func int64Value(value any) (int64, bool) {
	integer, ok := intValue(value)
	return int64(integer), ok
}

func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	return redactSensitive(value, "").(map[string]any)
}
