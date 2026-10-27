package team

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"

	"charm.land/fantasy"
)

func validateSubmissionFieldPresence(raw string, info fantasy.ToolInfo) *toolArgumentSchemaError {
	var fields map[string]jsontext.Value
	if json.Unmarshal([]byte(raw), &fields) != nil {
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		if _, allowed := info.Parameters[name]; !allowed {
			return &toolArgumentSchemaError{Path: "$." + name, Expected: "declared property", Actual: "additional property", schema: submissionSchema(info), schemaPath: "$"}
		}
	}
	return nil
}

// Submission fields are a versioned contract choice, not a heuristic based on
// the agent name or its prose. Required runtime assertions always remain visible.
func compileResultSubmissionFields(root map[string]any) ([]string, error) {
	raw, ok := root["x-hufu-submission-fields"]
	if !ok {
		return nil, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var fields []string
	if err := json.Unmarshal(data, &fields); err != nil || len(fields) == 0 {
		return nil, fmt.Errorf("x-hufu-submission-fields must be a nonempty array of field names")
	}
	allowed := submitResultToolInfo(taskResultSubmissionContract{AllowArtifacts: true, AllowEvidence: true}).Parameters
	for i, field := range fields {
		if field != "structured_payload" && allowed[field] == nil || slices.Contains(fields[:i], field) {
			return nil, fmt.Errorf("x-hufu-submission-fields: unknown or duplicate field %q", field)
		}
	}
	for _, field := range []string{"status", "summary", "structured_payload"} {
		if !slices.Contains(fields, field) {
			return nil, fmt.Errorf("x-hufu-submission-fields must include %q", field)
		}
	}
	return fields, nil
}

func projectResultSubmissionFields(info fantasy.ToolInfo, fields []string) fantasy.ToolInfo {
	if len(fields) != 0 {
		for name := range info.Parameters {
			if !slices.Contains(fields, name) && !slices.Contains(info.Required, name) {
				delete(info.Parameters, name)
			}
		}
	}
	return info
}

// Candidate validation retains all issued collection keys even when a provider's
// portable tool schema must omit them because of its schema-size limit.
func (t *submitResultTool) candidateValidationInfo() fantasy.ToolInfo {
	return localResultCandidateValidationInfo(t.coordinator, t.todoID)
}

func localResultCandidateValidationInfo(c *Coordinator, taskID string) fantasy.ToolInfo {
	info := localResultSubmissionInfo(c, taskID)
	if c == nil {
		return info
	}
	ref, compiled, err := c.boundResultContract(taskID)
	if err != nil || compiled == nil || len(compiled.fixedCollections) == 0 {
		return info
	}
	plans, err := c.fixedCollectionPlans(taskID)
	if err != nil {
		return info
	}
	schema := resultFixedCollectionPayloadSchema(*ref, compiled, plans, false)
	advertiseResultEvidenceKeys(schema, compiled.evidenceKeys)
	info.Parameters["structured_payload"] = providerResultIntegerStrings(schema, 0)
	return info
}

func (t *submitResultTool) usesSubmissionProjection() bool {
	if t.coordinator == nil {
		return false
	}
	_, compiled, err := t.coordinator.boundResultContract(t.todoID)
	return err == nil && compiled != nil && len(compiled.submissionFields) > 0
}
