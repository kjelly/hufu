//go:build linux || darwin
// +build linux darwin

package tools

import (
	"encoding/json"
	"fmt"

	"charm.land/fantasy"
)

func parseArgs(input string, target any) error {
	if input == "" || input == "{}" {
		return nil
	}
	if err := json.Unmarshal([]byte(input), target); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func validateRequired(input string, required []string) error {
	if input == "" || input == "{}" {
		for _, field := range required {
			return fmt.Errorf("parameter %q is required", field)
		}
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(input), &raw); err != nil {
		return nil
	}
	for _, field := range required {
		val, ok := raw[field]
		if !ok || val == nil {
			return fmt.Errorf("parameter %q is required", field)
		}
		if s, ok := val.(string); ok && s == "" {
			return fmt.Errorf("parameter %q must not be empty", field)
		}
	}
	return nil
}

func validateParamType(input string, paramName string, expectedType string) error {
	if input == "" || input == "{}" {
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(input), &raw); err != nil {
		return nil
	}
	val, ok := raw[paramName]
	if !ok || val == nil {
		return nil
	}
	switch expectedType {
	case "string":
		if _, ok := val.(string); !ok {
			return fmt.Errorf("parameter %q must be a string", paramName)
		}
	case "number":
		if _, ok := val.(float64); !ok {
			if _, ok := val.(int); !ok {
				return fmt.Errorf("parameter %q must be a number", paramName)
			}
		}
	case "boolean":
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("parameter %q must be a boolean", paramName)
		}
	case "array":
		if _, ok := val.([]any); !ok {
			return fmt.Errorf("parameter %q must be an array", paramName)
		}
	}
	return nil
}

func validateToolInput(input string, info fantasy.ToolInfo) error {
	if err := validateRequired(input, info.Required); err != nil {
		return err
	}
	if input == "" || input == "{}" {
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(input), &raw); err != nil {
		return nil
	}
	for paramName, paramDef := range info.Parameters {
		defMap, ok := paramDef.(map[string]any)
		if !ok {
			continue
		}
		typeVal, ok := defMap["type"].(string)
		if !ok {
			continue
		}
		if _, exists := raw[paramName]; exists {
			if err := validateParamType(input, paramName, typeVal); err != nil {
				return err
			}
		}
	}
	return nil
}
