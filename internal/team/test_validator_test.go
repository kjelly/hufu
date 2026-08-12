package team

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestValidationErrIsNil(t *testing.T) {
	input := "{\"tasks\":\"[{\\\"agent\\\": \\\"config-applier\\\", \\\"goal\\\": \\\"do it\\\"}]\"}"

	// Create tasksSchema exactly as in coordinator_tools.go
	taskSchema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"required":             []string{"agent", "goal"},
		"additionalProperties": false,
	}
	tasksSchema := map[string]any{
		"type":  "array",
		"items": taskSchema,
	}

	infoParameters := map[string]any{
		"tasks": tasksSchema,
	}

	top := map[string]any{
		"type":                 "object",
		"properties":           infoParameters,
		"required":             []string{"tasks"},
		"additionalProperties": false,
	}

	decoder := json.NewDecoder(strings.NewReader(input))
	decoder.UseNumber()
	var value any
	err := decoder.Decode(&value)
	if err != nil {
		t.Fatal(err)
	}

	valErr := validateSchemaValue(value, top, "$")
	if valErr == nil {
		fmt.Println("validationErr IS NIL!!!")
	} else {
		fmt.Printf("validationErr: %v\n", valErr)
	}
}
