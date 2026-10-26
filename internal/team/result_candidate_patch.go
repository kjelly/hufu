package team

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"charm.land/fantasy"
)

type resultPatchBaseKey struct{}

type resultCandidateEdit struct {
	Operation string         `json:"operation"`
	Path      string         `json:"path"`
	From      string         `json:"from,omitempty"`
	Value     jsontext.Value `json:"value,omitempty"`
}

func resultPatchToolInfo(candidate *ResultCandidate) fantasy.ToolInfo {
	errors, _ := json.Marshal(candidate.Issues)
	return fantasy.ToolInfo{Name: submitResultToolName, Description: "Repair the runtime-pinned rejected candidate. Submit only edits, never regenerate the report. Move preserves an existing value; add fills a missing property; replace corrects an invalid scalar. No deletions or overwriting valid fields. Runtime revalidates the complete result and evidence before committing. Issues: " + string(errors), Required: []string{"edits"}, Parameters: map[string]any{
		"edits": map[string]any{"type": "array", "minItems": 1, "maxItems": 16, "items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"operation", "path"}, "properties": map[string]any{
			"operation": map[string]any{"type": "string", "enum": []string{"add", "replace", "move"}},
			"path":      map[string]any{"type": "string", "description": "Destination JSON Pointer in the COMPLETE submit_result arguments."},
			"from":      map[string]any{"type": "string", "description": "For move only: existing misplaced value's JSON Pointer."},
			"value":     map[string]any{"description": "For add/replace only: corrected value. Omit for move."},
		}}},
	}}
}

func (t *submitResultTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if t.candidate != nil {
		return t.runPatch(ctx, call)
	}
	response, err := t.runFull(ctx, call)
	if err != nil || !response.IsError || t.coordinator == nil {
		return response, err
	}
	item := t.coordinator.todoItemByID(t.todoID)
	if item == nil || item.ResultContract == nil || t.coordinator.artifactStoreRootPath() == "" || len(call.Input) > resultCandidateMaxBytes {
		return response, nil
	}
	// Capture only a structurally rejected object. Evidence/policy/verification
	// failures are never converted into an automatic formatting repair.
	issues, truncated, diagnosticErr := resultSubmissionIssues(call.Input, t.candidateValidationInfo())
	if diagnosticErr != nil || len(issues) == 0 {
		return response, nil
	}
	diagnostic, _ := json.Marshal(struct {
		Issues    []ResultSubmissionIssue `json:"issues"`
		Truncated bool                    `json:"truncated,omitzero"`
	}{issues, truncated})
	response.Content += "\nSubmission diagnostics: " + string(diagnostic)
	value, decodeErr := decodeResultContractJSON([]byte(call.Input))
	_, isObject := value.(map[string]any)
	if issues[0].Code == "invalid_json" || decodeErr != nil || !isObject {
		return response, nil
	}
	if _, identityErr := submitResultRuntimeIdentityFromContext(ctx, t.coordinator, t.todoID); identityErr != nil {
		return response, nil
	}
	if saveErr := t.coordinator.saveResultCandidate(ctx, t.todoID, call.Input, issues, truncated, nil); saveErr != nil {
		return fantasy.NewTextErrorResponse("result_candidate_persistence_failed: " + saveErr.Error()), saveErr
	}
	return response, nil
}

func (t *submitResultTool) runPatch(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	c := t.coordinator
	if c == nil {
		return fantasy.NewTextErrorResponse("candidate_unavailable"), nil
	}
	identity, err := submitResultRuntimeIdentityFromContext(ctx, c, t.todoID)
	if err != nil || identity.Attempt != t.candidate.Attempt {
		return fantasy.NewTextErrorResponse("candidate_stale"), nil
	}
	item := c.todoItemByID(t.todoID)
	value, err := c.loadResultCandidate(ctx, item, t.candidate)
	if err != nil {
		return fantasy.NewTextErrorResponse("candidate_stale: " + err.Error()), nil
	}
	if t.candidate.Truncated {
		return fantasy.NewTextErrorResponse("candidate_requires_review: diagnostic limit exceeded"), nil
	}
	if issue := validateToolArguments(call.Input, t.Info()); issue != nil {
		return fantasy.NewTextErrorResponse(buildToolSchemaValidationPrompt(submitResultToolName, issue, t.Info())), nil
	}
	if _, err := decodeUniqueJSON([]byte(call.Input)); err != nil {
		return fantasy.NewTextErrorResponse("invalid_json: " + err.Error()), nil
	}
	var patch struct {
		Edits []resultCandidateEdit `json:"edits"`
	}
	if err := json.Unmarshal([]byte(call.Input), &patch, json.RejectUnknownMembers(true)); err != nil {
		return fantasy.NewTextErrorResponse("invalid_patch: " + err.Error()), nil
	}
	if err := validateCandidateEditSet(patch.Edits); err != nil {
		return fantasy.NewTextErrorResponse("invalid_patch: " + err.Error()), nil
	}
	for _, edit := range patch.Edits {
		if err := applyResultCandidateEdit(value, edit, t.candidate.Issues, submissionSchema(t.candidateValidationInfo())); err != nil {
			return fantasy.NewTextErrorResponse("invalid_patch: " + err.Error()), nil
		}
	}
	data, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	issues, truncated, err := resultSubmissionIssues(string(data), t.candidateValidationInfo())
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if len(issues) > 0 {
		if err := c.saveResultCandidate(ctx, t.todoID, string(data), issues, truncated, t.candidate); err != nil {
			return fantasy.NewTextErrorResponse("candidate_stale: " + err.Error()), err
		}
		encoded, _ := json.Marshal(issues)
		return fantasy.NewTextErrorResponse("invalid_schema: candidate preserved; remaining issues: " + string(encoded)), nil
	}
	call.Input = string(data)
	// This is the same complete validation/receipt/evidence/transaction path
	// used by ordinary submissions. A patch is never a success shortcut.
	return t.runFull(context.WithValue(ctx, resultPatchBaseKey{}, t.candidate.SHA256), call)
}

func applyResultCandidateEdit(document map[string]any, edit resultCandidateEdit, issues []ResultSubmissionIssue, schema map[string]any) error {
	if edit.Path == "" || validateJSONPointer(edit.Path) != nil {
		return fmt.Errorf("invalid destination pointer")
	}
	if edit.Operation == "move" {
		return moveResultCandidateValue(document, edit, issues, schema)
	}
	if edit.Operation != "add" && edit.Operation != "replace" {
		return fmt.Errorf("unknown edit operation")
	}
	return correctResultCandidateValue(document, edit, issues, schema)
}

func moveResultCandidateValue(document map[string]any, edit resultCandidateEdit, issues []ResultSubmissionIssue, schema map[string]any) error {
	parent, key, err := candidateMoveParent(document, edit.Path, issues, schema)
	if err != nil {
		return err
	}
	_, present := parent[key]
	if edit.From == "" || len(edit.Value) != 0 || resultPointersOverlap(edit.From, edit.Path) || !candidateMoveSourceAllowed(edit.From, issues) {
		return fmt.Errorf("move must preserve a rejected value at disjoint pointers")
	}
	_, sourceName, _ := strings.CutLast(edit.From, "/")
	_, targetName, _ := strings.CutLast(edit.Path, "/")
	if sourceName != targetName {
		return fmt.Errorf("move cannot rename a field or collection key")
	}
	if candidateSchemaAt(schema, edit.Path) == nil {
		return fmt.Errorf("move destination is not a declared model field")
	}
	source, sourceKey, err := candidateObjectParent(document, edit.From)
	if err != nil {
		return err
	}
	v, exists := source[sourceKey]
	if !exists || present && !reflect.DeepEqual(parent[key], v) {
		return fmt.Errorf("move source missing or destination would overwrite data")
	}
	parent[key] = v
	delete(source, sourceKey)
	return nil
}

func correctResultCandidateValue(document map[string]any, edit resultCandidateEdit, issues []ResultSubmissionIssue, schema map[string]any) error {
	parent, key, err := candidateObjectParent(document, edit.Path)
	if err != nil {
		return err
	}
	_, present := parent[key]
	code := "missing_property"
	if edit.Operation == "replace" {
		code = "invalid_type"
	}
	allowed := false
	for _, issue := range issues {
		if issue.Pointer == edit.Path && (issue.Code == code || edit.Operation == "replace" && issue.Code == "invalid_enum") {
			allowed = true
		}
	}
	if !allowed || edit.From != "" || len(edit.Value) == 0 || edit.Operation == "add" && present || edit.Operation == "replace" && !present {
		return fmt.Errorf("edit is outside the rejected fields")
	}
	if _, ok := parent[key].(map[string]any); ok {
		return fmt.Errorf("cannot replace an object")
	}
	if _, ok := parent[key].([]any); ok {
		return fmt.Errorf("cannot replace an array")
	}
	if candidateSchemaAt(schema, edit.Path) == nil {
		return fmt.Errorf("destination is not a declared model field")
	}
	v, err := decodeResultContractJSON(edit.Value)
	if err != nil {
		return err
	}
	parent[key] = v
	return nil
}

func candidateMoveSourceAllowed(pointer string, issues []ResultSubmissionIssue) bool {
	if pointer == "" || validateJSONPointer(pointer) != nil {
		return false
	}
	for _, issue := range issues {
		if issue.Code != "missing_property" && (issue.Pointer == pointer || strings.HasPrefix(issue.Pointer, pointer+"/")) {
			return true
		}
	}
	return false
}

func candidateObjectParent(document any, pointer string) (map[string]any, string, error) {
	parent, key, ok := strings.CutLast(pointer, "/")
	if !ok {
		return nil, "", fmt.Errorf("invalid pointer")
	}
	v := document
	var err error
	if parent != "" {
		v, err = resolveJSONPointer(document, parent)
	}
	object, ok := v.(map[string]any)
	if err != nil || !ok {
		return nil, "", fmt.Errorf("destination parent must be an existing object")
	}
	key = strings.ReplaceAll(strings.ReplaceAll(key, "~1", "/"), "~0", "~")
	return object, key, nil
}

// A misplaced collection may have no destination container yet. Materialize
// only schema-declared object parents below a diagnosed missing property; no
// values, array membership or required sibling fields are synthesized.
func candidateMoveParent(document map[string]any, pointer string, issues []ResultSubmissionIssue, schema map[string]any) (map[string]any, string, error) {
	if parent, key, err := candidateObjectParent(document, pointer); err == nil {
		return parent, key, nil
	}
	tokens := fixedCollectionPointerTokens(pointer)
	parent := document
	path := ""
	for _, token := range tokens[:len(tokens)-1] {
		path += "/" + escapeJSONPointerToken(token)
		value, exists := parent[token]
		if !exists {
			node := candidateSchemaAt(schema, path)
			allowed := false
			for _, issue := range issues {
				if issue.Code == "missing_property" && (issue.Pointer == path || strings.HasPrefix(path, issue.Pointer+"/")) {
					allowed = true
				}
			}
			if !allowed || node["type"] != "object" {
				return nil, "", fmt.Errorf("move parent is not a declared missing object")
			}
			value = make(map[string]any)
			parent[token] = value
		}
		next, ok := value.(map[string]any)
		if !ok {
			return nil, "", fmt.Errorf("move parent must be an object")
		}
		parent = next
	}
	return parent, tokens[len(tokens)-1], nil
}

func candidateSchemaAt(schema map[string]any, pointer string) map[string]any {
	node := schema
	for _, token := range fixedCollectionPointerTokens(pointer) {
		if node["type"] == "array" {
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || strconv.Itoa(index) != token {
				return nil
			}
			node, _ = node["items"].(map[string]any)
		} else {
			properties, _ := node["properties"].(map[string]any)
			node, _ = properties[token].(map[string]any)
		}
		if node == nil {
			return nil
		}
	}
	return node
}

func validateCandidateEditSet(edits []resultCandidateEdit) error {
	var changed []string
	for _, edit := range edits {
		paths := []string{edit.Path}
		if edit.Operation == "move" {
			paths = append(paths, edit.From)
		}
		for _, path := range paths {
			for _, prior := range changed {
				if resultPointersOverlap(prior, path) {
					return fmt.Errorf("overlapping edits require a new validated candidate version")
				}
			}
		}
		changed = append(changed, paths...)
	}
	return nil
}
