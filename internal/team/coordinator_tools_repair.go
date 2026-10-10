package team

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

// protocolRepairState is shared by every coordinator tool exposed in one
// model invocation. A rejected call can therefore be repaired once even when
// the replacement is dispatched through a newly generated tool-call ID.
type protocolRepairState struct {
	mu      sync.Mutex
	pending *protocolRepairAttempt
}

type protocolRepairAttempt struct {
	tool           string
	originalCallID string
	original       *toolArgumentSchemaError
	redirects      int
}

const maxProtocolRepairRedirects = 1

// protocolRepairExecutionKey marks the one regenerated tool call after a
// schema violation. Ingress handlers use it to prevent a repair turn from
// creating new evidence that was not accepted by the original invocation.
type protocolRepairExecutionKey struct{}

func protocolRepairExecution(ctx context.Context) bool {
	value, _ := ctx.Value(protocolRepairExecutionKey{}).(bool)
	return value
}

type protocolRepairWrapper struct {
	base  fantasy.AgentTool
	c     *Coordinator
	state *protocolRepairState
}

type toolArgumentSchemaError struct {
	Path     string
	Expected string
	Actual   string
	Detail   string
}

func (e *toolArgumentSchemaError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("%s: expected %s, got %s", e.Path, e.Expected, e.Actual)
	}
	return fmt.Sprintf("%s: expected %s, got %s (%s)", e.Path, e.Expected, e.Actual, e.Detail)
}

func (t *protocolRepairWrapper) Info() fantasy.ToolInfo { return t.base.Info() }

func (t *protocolRepairWrapper) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if err := ctx.Err(); err != nil {
		return fantasy.ToolResponse{}, err
	}

	call.Input = canonicalToolArgumentObjects(call.Input, t.base.Info())
	call.Input, _ = tools.CanonicalToolArgumentCase(call.Input, t.base.Info())
	validationErr := validateToolArguments(call.Input, t.base.Info())
	t.state.mu.Lock()
	pending := t.state.pending

	if pending != nil {
		if call.Name != pending.tool && t.base.Info().Name != pending.tool {
			mismatch := &toolArgumentSchemaError{Path: "$", Expected: "complete arguments for tool " + strconv.Quote(pending.tool), Actual: "tool " + strconv.Quote(t.base.Info().Name)}
			if pending.redirects < maxProtocolRepairRedirects {
				pending.redirects++
				t.state.mu.Unlock()
				t.auditViolation(ctx, call, mismatch, true, "repair_redirected")
				return fantasy.NewTextErrorResponse(buildProtocolRepairRedirectPrompt(pending, call)), nil
			}
			t.state.pending = nil
			t.state.mu.Unlock()
			t.auditViolation(ctx, call, mismatch, true, "terminal_failure")
			return fantasy.ToolResponse{}, t.terminalRepairError(ctx, pending, call, mismatch)
		}
		if validationErr != nil {
			t.state.pending = nil
			t.state.mu.Unlock()
			t.auditViolation(ctx, call, validationErr, true, "terminal_failure")
			return fantasy.ToolResponse{}, t.terminalRepairError(ctx, pending, call, validationErr)
		}
		// A schema-valid regeneration consumes the pending repair before the
		// policy/tool boundary. Any later policy or execution error is terminal
		// and can never be mistaken for another protocol-repair opportunity.
		t.state.pending = nil
		t.state.mu.Unlock()
		if err := ctx.Err(); err != nil {
			t.auditViolation(ctx, call, pending.original, true, "cancelled")
			return fantasy.ToolResponse{}, err
		}
		repairCtx := context.WithValue(ctx, protocolRepairExecutionKey{}, true)
		resp, err := t.base.Run(repairCtx, call)
		if err == nil && !resp.IsError {
			t.c.coordinatorProtocolRepairsSuccess.Add(1)
			t.auditViolation(ctx, call, pending.original, true, "repaired_and_executed")
		}
		return resp, err
	}

	if validationErr != nil {
		t.state.pending = &protocolRepairAttempt{
			tool:           t.base.Info().Name,
			originalCallID: call.ID,
			original:       validationErr,
		}
		t.state.mu.Unlock()
		t.c.coordinatorProtocolRepairsAttempt.Add(1)
		t.auditViolation(ctx, call, validationErr, false, "repair_requested")
		return fantasy.NewTextErrorResponse(buildProtocolRepairPrompt(t.base.Info().Name, validationErr, t.base.Info())), nil
	}
	t.state.mu.Unlock()

	// Valid arguments pass through exactly once. Authorization, capability,
	// sequence, execution, and non-zero tool results are intentionally outside
	// the repair protocol.
	return t.base.Run(ctx, call)
}

// buildProtocolRepairRedirectPrompt rejects a stray tool call without running
// it and leaves the original repair pending. This is bounded by
// maxProtocolRepairRedirects; it gives tool-calling models one clear chance to
// return to the schema repair without turning an invalid coordinator request
// into a terminal run failure.
func buildProtocolRepairRedirectPrompt(pending *protocolRepairAttempt, call fantasy.ToolCall) string {
	if pending == nil {
		return "A tool argument repair is pending. Call the required tool with corrected complete arguments."
	}
	return fmt.Sprintf("A repair is pending for tool %q. Do not call %q or any other tool. Your only permitted next call is %q with a complete corrected argument object. The original schema error was: %s", pending.tool, call.Name, pending.tool, pending.original.Error())
}

func (t *protocolRepairWrapper) terminalRepairError(ctx context.Context, original *protocolRepairAttempt, repaired fantasy.ToolCall, repairErr *toolArgumentSchemaError) error {
	model, provider := protocolRepairIdentity(ctx)
	return fmt.Errorf("coordinator tool argument repair failed closed: tool=%q model=%q provider=%q original_call_id=%q repair_call_id=%q original_error=%q repair_error=%q",
		original.tool, model, provider, original.originalCallID, repaired.ID, original.original.Error(), repairErr.Error())
}

func (t *protocolRepairWrapper) auditViolation(ctx context.Context, call fantasy.ToolCall, violation *toolArgumentSchemaError, repair bool, disposition string) {
	if t == nil || t.c == nil || t.c.auditLogger == nil || violation == nil {
		return
	}
	model, provider := protocolRepairIdentity(ctx)
	t.c.auditLogger.LogToolArgumentSchemaViolation("coordinator", t.base.Info().Name, call.ID, model, provider, violation.Path, violation.Expected, violation.Actual, repair, disposition)
}

func protocolRepairIdentity(ctx context.Context) (model, provider string) {
	model, _ = ctx.Value(modelKey{}).(string)
	provider, _ = agent.ParseModelProvider(model)
	if model == "" {
		model = "unknown"
	}
	if provider == "" {
		provider = "default"
	}
	return model, provider
}

func buildProtocolRepairPrompt(toolName string, violation *toolArgumentSchemaError, info fantasy.ToolInfo) string {
	return buildToolSchemaValidationPrompt(toolName, violation, info) + "\nRegenerate the complete argument object without commentary."
}

func buildToolSchemaValidationPrompt(toolName string, violation *toolArgumentSchemaError, info fantasy.ToolInfo) string {
	return fmt.Sprintf("%s%s: expected %s, got %s. Valid example: %s",
		toolSchemaValidationPromptPrefix(toolName), violation.Path, violation.Expected, violation.Actual, compactToolExampleJSON(info))
}

// Keep diagnostic production and the submit_result loop classifier coupled;
// changing a field-specific message must not bypass the rejection budget.
func toolSchemaValidationPromptPrefix(toolName string) string {
	return fmt.Sprintf("Tool %q arguments are invalid at ", toolName)
}

func compactToolExampleJSON(info fantasy.ToolInfo) string {
	exampleJSON, err := json.Marshal(generateCompactToolExample(info))
	if err != nil {
		return "{}"
	}
	return string(exampleJSON)
}

func generateCompactToolExample(info fantasy.ToolInfo) map[string]any {
	result := make(map[string]any)
	required := make(map[string]bool, len(info.Required))
	for _, name := range info.Required {
		required[name] = true
	}
	keys := make([]string, 0, len(info.Parameters))
	for name := range info.Parameters {
		if required[name] {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	for _, name := range keys {
		// Domain payload examples should not fabricate optional claims.
		// Other tool arrays retain an item to illustrate their input shape.
		result[name] = generateCompactExampleWithArrayDefaults(info.Parameters[name], name == "structured_payload")
	}
	return result
}

func generateCompactExample(raw any) any {
	return generateCompactExampleWithArrayDefaults(raw, false)
}

func generateCompactExampleWithArrayDefaults(raw any, emptyOptionalArrays bool) any {
	schema, _ := raw.(map[string]any)
	if schema == nil {
		return nil
	}
	if value, ok := schema["const"]; ok {
		return value
	}
	if alternatives, ok := schema["oneOf"].([]any); ok && len(alternatives) > 0 {
		return generateCompactExampleWithArrayDefaults(alternatives[0], emptyOptionalArrays)
	}
	if enum, ok := schema["enum"].([]string); ok && len(enum) > 0 {
		return enum[0]
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		return enum[0]
	}
	switch schema["type"] {
	case "object":
		obj := make(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		for _, name := range schemaStringSlice(schema["required"]) {
			if child, exists := props[name]; exists {
				obj[name] = generateCompactExampleWithArrayDefaults(child, emptyOptionalArrays)
			}
		}
		return obj
	case "array":
		minimum, specified := schemaInt(schema["minItems"])
		if (specified && minimum == 0) || (!specified && emptyOptionalArrays) {
			return []any{}
		}
		return []any{generateCompactExampleWithArrayDefaults(schema["items"], emptyOptionalArrays)}
	case "string":
		return "value"
	case "boolean":
		return false
	case "number", "integer":
		return 0
	default:
		return nil
	}
}

// coordinatorToolDecodesUndeclaredArguments reports whether a coordinator
// tool's own decoder, not its model-visible schema, owns undeclared argument
// keys. The agent tool's provider schema is a deliberately small projection
// (portableProviderTaskProperties): decodeModelTaskDefs accepts or rejects the
// full task vocabulary, including fields the coordinator prompt documents but
// the projection omits (for example verify_spec).
func coordinatorToolDecodesUndeclaredArguments(name string) bool {
	return name == "agent"
}

func validateToolArguments(input string, info fantasy.ToolInfo) *toolArgumentSchemaError {
	decoder := json.NewDecoder(bytes.NewBufferString(input))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return &toolArgumentSchemaError{Path: "$", Expected: "object", Actual: "invalid JSON", Detail: err.Error()}
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return &toolArgumentSchemaError{Path: "$", Expected: "single object", Actual: "trailing JSON value", Detail: err.Error()}
	}
	top := map[string]any{
		"type":                 "object",
		"properties":           info.Parameters,
		"required":             info.Required,
		"additionalProperties": false,
	}
	if coordinatorToolDecodesUndeclaredArguments(info.Name) {
		top = withoutClosedObjects(top).(map[string]any)
	}
	return validateSchemaValue(value, top, "$")
}

// withoutClosedObjects returns a copy of schema with every
// additionalProperties:false constraint removed, so declared properties keep
// their type, enum, and required checks while undeclared keys pass through.
func withoutClosedObjects(schema any) any {
	switch node := schema.(type) {
	case map[string]any:
		copied := make(map[string]any, len(node))
		for key, value := range node {
			if key == "additionalProperties" {
				if closed, ok := value.(bool); ok && !closed {
					continue
				}
			}
			copied[key] = withoutClosedObjects(value)
		}
		return copied
	case []any:
		copied := make([]any, len(node))
		for i, value := range node {
			copied[i] = withoutClosedObjects(value)
		}
		return copied
	default:
		return schema
	}
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return fmt.Errorf("multiple JSON values")
	} else if err != io.EOF {
		return err
	}
	return nil
}

func validateSchemaValue(value any, schema map[string]any, path string) *toolArgumentSchemaError {
	if alternatives, ok := schema["oneOf"].([]any); ok {
		for _, alternative := range alternatives {
			if candidate, ok := alternative.(map[string]any); ok && validateSchemaValue(value, candidate, path) == nil {
				return nil
			}
		}
		return &toolArgumentSchemaError{Path: path, Expected: "exactly one declared schema", Actual: jsonValueType(value)}
	}
	expected, _ := schema["type"].(string)
	if expected != "" && !matchesJSONType(value, expected) {
		return &toolArgumentSchemaError{Path: path, Expected: expected, Actual: jsonValueType(value)}
	}
	if enum := schemaEnum(schema["enum"]); len(enum) > 0 && !containsJSONValue(enum, value) {
		return &toolArgumentSchemaError{Path: path, Expected: "one of " + compactJSON(enum), Actual: compactJSON(value)}
	}
	switch expected {
	case "object":
		obj := value.(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		for _, name := range schemaStringSlice(schema["required"]) {
			if _, ok := obj[name]; !ok {
				return &toolArgumentSchemaError{Path: joinJSONPath(path, name), Expected: "required property", Actual: "missing"}
			}
		}
		if additional, ok := schema["additionalProperties"].(bool); ok && !additional {
			keys := make([]string, 0, len(obj))
			for name := range obj {
				keys = append(keys, name)
			}
			sort.Strings(keys)
			for _, name := range keys {
				if _, ok := props[name]; !ok {
					return &toolArgumentSchemaError{Path: joinJSONPath(path, name), Expected: "declared property", Actual: "additional property"}
				}
			}
		}
		keys := make([]string, 0, len(props))
		for name := range props {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		for _, name := range keys {
			childValue, ok := obj[name]
			if !ok {
				continue
			}
			childSchema, ok := props[name].(map[string]any)
			if ok {
				if err := validateSchemaValue(childValue, childSchema, joinJSONPath(path, name)); err != nil {
					return err
				}
			}
		}
	case "array":
		items := value.([]any)
		if min, ok := schemaInt(schema["minItems"]); ok && len(items) < min {
			return &toolArgumentSchemaError{Path: path, Expected: fmt.Sprintf("array with at least %d items", min), Actual: fmt.Sprintf("array with %d items", len(items))}
		}
		if max, ok := schemaInt(schema["maxItems"]); ok && len(items) > max {
			return &toolArgumentSchemaError{Path: path, Expected: fmt.Sprintf("array with at most %d items", max), Actual: fmt.Sprintf("array with %d items", len(items))}
		}
		itemSchema, _ := schema["items"].(map[string]any)
		prefixSchemas := schemaMapSlice(schema["prefixItems"])
		for i, item := range items {
			selectedSchema := itemSchema
			if i < len(prefixSchemas) {
				selectedSchema = prefixSchemas[i]
			}
			if selectedSchema != nil {
				if err := validateSchemaValue(item, selectedSchema, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case "integer", "number":
		number, _ := value.(json.Number)
		parsed, err := number.Float64()
		if err != nil {
			return &toolArgumentSchemaError{Path: path, Expected: expected, Actual: "invalid number"}
		}
		if minimum, ok := schemaFloat(schema["minimum"]); ok && parsed < minimum {
			return &toolArgumentSchemaError{Path: path, Expected: fmt.Sprintf("%s >= %v", expected, minimum), Actual: number.String()}
		}
		if maximum, ok := schemaFloat(schema["maximum"]); ok && parsed > maximum {
			return &toolArgumentSchemaError{Path: path, Expected: fmt.Sprintf("%s <= %v", expected, maximum), Actual: number.String()}
		}
	case "string":
		return validateStringSchema(value.(string), schema, path)
	}
	return nil
}

func validateStringSchema(value string, schema map[string]any, path string) *toolArgumentSchemaError {
	length := utf8.RuneCountInString(value)
	if minimum, ok := schemaInt(schema["minLength"]); ok && length < minimum {
		return &toolArgumentSchemaError{Path: path, Expected: fmt.Sprintf("string with at least %d runes", minimum), Actual: fmt.Sprintf("string with %d runes", length)}
	}
	if maximum, ok := schemaInt(schema["maxLength"]); ok && length > maximum {
		return &toolArgumentSchemaError{Path: path, Expected: fmt.Sprintf("string with at most %d runes", maximum), Actual: fmt.Sprintf("string with %d runes", length)}
	}
	return nil
}

func matchesJSONType(value any, expected string) bool {
	switch expected {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "integer":
		n, ok := value.(json.Number)
		return ok && !strings.ContainsAny(n.String(), ".eE")
	default:
		return true
	}
}

func jsonValueType(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number:
		if strings.ContainsAny(v.String(), ".eE") {
			return "number"
		}
		return "integer"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func schemaStringSlice(value any) []string {
	switch values := value.(type) {
	case []string:
		return values
	case []any:
		result := make([]string, 0, len(values))
		for _, item := range values {
			if text, ok := item.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func schemaEnum(value any) []any {
	switch values := value.(type) {
	case []any:
		return values
	case []string:
		result := make([]any, len(values))
		for i, item := range values {
			result[i] = item
		}
		return result
	default:
		return nil
	}
}

func schemaMapSlice(value any) []map[string]any {
	switch values := value.(type) {
	case []map[string]any:
		return values
	case []any:
		result := make([]map[string]any, 0, len(values))
		for _, item := range values {
			if schema, ok := item.(map[string]any); ok {
				result = append(result, schema)
			}
		}
		return result
	default:
		return nil
	}
}

func containsJSONValue(values []any, target any) bool {
	targetJSON := compactJSON(target)
	for _, value := range values {
		if compactJSON(value) == targetJSON {
			return true
		}
	}
	return false
}

func compactJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
}

func schemaInt(value any) (int, bool) {
	switch n := value.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case json.Number:
		value, err := strconv.Atoi(n.String())
		return value, err == nil
	default:
		return 0, false
	}
}

func schemaFloat(value any) (float64, bool) {
	switch n := value.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case json.Number:
		value, err := n.Float64()
		return value, err == nil
	default:
		return 0, false
	}
}

func joinJSONPath(parent, property string) string {
	if property != "" && (property[0] == '_' || property[0] >= 'A' && property[0] <= 'Z' || property[0] >= 'a' && property[0] <= 'z') {
		for _, r := range property[1:] {
			if r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
				continue
			}
			return parent + "[" + strconv.Quote(property) + "]"
		}
		return parent + "." + property
	}
	return parent + "[" + strconv.Quote(property) + "]"
}

func (c *Coordinator) wrapWithProtocolRepair(tools []fantasy.AgentTool) []fantasy.AgentTool {
	state := &protocolRepairState{}
	wrapped := make([]fantasy.AgentTool, 0, len(tools))
	for _, tool := range tools {
		if tool != nil {
			wrapped = append(wrapped, &protocolRepairWrapper{base: tool, c: c, state: state})
		}
	}
	return wrapped
}

func (t *protocolRepairWrapper) ProviderOptions() fantasy.ProviderOptions {
	return t.base.ProviderOptions()
}

func (t *protocolRepairWrapper) SetProviderOptions(opts fantasy.ProviderOptions) {
	t.base.SetProviderOptions(opts)
}
