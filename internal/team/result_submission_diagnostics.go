package team

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"charm.land/fantasy"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

const maxResultIssues = 16

// ResultSubmissionIssue carries machine-readable JSON Pointers. Recovery never
// parses the human-facing error string to decide which fields it may change.
type ResultSubmissionIssue struct {
	Code     string   `json:"code"`
	Pointer  string   `json:"pointer"`
	Expected []string `json:"expected,omitempty"`
}

func submissionSchema(info fantasy.ToolInfo) map[string]any {
	return map[string]any{"type": "object", "properties": info.Parameters, "required": info.Required, "additionalProperties": false}
}

func resultSubmissionIssues(raw string, info fantasy.ToolInfo) ([]ResultSubmissionIssue, bool, error) {
	if _, err := decodeUniqueJSON([]byte(raw)); err != nil {
		return []ResultSubmissionIssue{{Code: "invalid_json", Pointer: ""}}, false, nil
	}
	value, err := decodeResultContractJSON([]byte(raw))
	if err != nil {
		return nil, false, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(denyResultContractLoader{})
	const url = "hufu-submission:///arguments"
	encoded, err := json.Marshal(submissionSchema(info))
	if err != nil {
		return nil, false, err
	}
	document, err := decodeResultContractJSON(encoded)
	if err != nil {
		return nil, false, err
	}
	if err := compiler.AddResource(url, document); err != nil {
		return nil, false, err
	}
	schema, err := compiler.Compile(url)
	if err != nil {
		return nil, false, err
	}
	err = schema.Validate(value)
	if err == nil {
		return nil, false, nil
	}
	validation, ok := errors.AsType[*jsonschema.ValidationError](err)
	if !ok {
		return nil, false, err
	}
	var issues []ResultSubmissionIssue
	collectResultIssues(validation, &issues)
	slices.SortFunc(issues, func(a, b ResultSubmissionIssue) int {
		return strings.Compare(a.Pointer+"\x00"+a.Code, b.Pointer+"\x00"+b.Code)
	})
	truncated := len(issues) > maxResultIssues
	return issues[:min(len(issues), maxResultIssues)], truncated, nil
}

func collectResultIssues(e *jsonschema.ValidationError, issues *[]ResultSubmissionIssue) {
	// Bound traversal as well as presentation for hostile submissions.
	if len(*issues) > maxResultIssues {
		return
	}
	if len(e.Causes) > 0 {
		for _, cause := range e.Causes {
			collectResultIssues(cause, issues)
		}
		return
	}
	pointer := ""
	for _, token := range e.InstanceLocation {
		pointer += "/" + escapeJSONPointerToken(token)
	}
	appendIssue := func(code, path string, expected []string) {
		if len(*issues) <= maxResultIssues {
			*issues = append(*issues, ResultSubmissionIssue{Code: code, Pointer: path, Expected: expected})
		}
	}
	switch k := e.ErrorKind.(type) {
	case *kind.Required:
		for _, field := range slices.Sorted(slices.Values(k.Missing)) {
			appendIssue("missing_property", pointer+"/"+escapeJSONPointerToken(field), nil)
		}
	case *kind.AdditionalProperties:
		for _, field := range slices.Sorted(slices.Values(k.Properties)) {
			appendIssue("additional_property", pointer+"/"+escapeJSONPointerToken(field), nil)
		}
	case *kind.Type:
		appendIssue("invalid_type", pointer, k.Want)
	case *kind.Enum:
		var expected []string
		for _, value := range k.Want {
			if text, ok := value.(string); ok {
				expected = append(expected, text)
			} else if encoded, err := json.Marshal(value); err == nil {
				expected = append(expected, string(encoded))
			}
		}
		appendIssue("invalid_enum", pointer, expected)
	default:
		appendIssue("invalid_schema", pointer, []string{fmt.Sprint(e.ErrorKind.KeywordPath())})
	}
}
