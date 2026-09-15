package team

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/audit"
	internalmcp "github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/utils"
)

const (
	dynamicToolGatewayName          = "use_dynamic_tool"
	dynamicToolGatewaySchemaVersion = 1
	maxDynamicGatewayInputBytes     = 256 * 1024
	maxDynamicGatewayQueryRunes     = 512
	maxDynamicGatewayTargetBytes    = 256
	maxDynamicGatewayOutputBytes    = 256 * 1024
	maxDynamicInspectOutputBytes    = 32 * 1024
)

type dynamicToolExecutor interface {
	ExecuteAuthorizedTool(context.Context, string, string, string) (string, bool, error)
}

type DynamicToolInvocationEvent struct {
	Phase            string
	GatewayTool      string
	LogicalTool      string
	ToolCallID       string
	DescriptorSHA256 string
	IsError          bool
	ReasonCode       string
	Truncated        bool
	OriginalBytes    int
}

type DynamicToolInvocationReporter func(DynamicToolInvocationEvent)

type dynamicToolInvocationReporterKey struct{}
type dynamicToolInvocationDiagnostics func(DynamicToolInvocationEvent, string, string)

type dynamicToolInvocationSink struct {
	reporter    DynamicToolInvocationReporter
	diagnostics dynamicToolInvocationDiagnostics
}

func withDynamicToolInvocationSink(ctx context.Context, reporter DynamicToolInvocationReporter, diagnostics dynamicToolInvocationDiagnostics) context.Context {
	return context.WithValue(ctx, dynamicToolInvocationReporterKey{}, dynamicToolInvocationSink{reporter: reporter, diagnostics: diagnostics})
}

func reportDynamicToolInvocation(ctx context.Context, event DynamicToolInvocationEvent, input, result string) {
	if sink, ok := ctx.Value(dynamicToolInvocationReporterKey{}).(dynamicToolInvocationSink); ok {
		if sink.reporter != nil {
			sink.reporter(event)
		}
		if sink.diagnostics != nil {
			sink.diagnostics(event, input, result)
		}
	}
}

type dynamicToolGateway struct {
	coordinator *Coordinator
	executor    dynamicToolExecutor
	targets     map[string]DynamicToolTarget
	ordered     []DynamicToolTarget
	pOpts       fantasy.ProviderOptions
}

func newDynamicToolGateway(c *Coordinator, executor dynamicToolExecutor, targets []DynamicToolTarget) *dynamicToolGateway {
	cloned := cloneDynamicToolTargets(targets)
	slices.SortFunc(cloned, func(a, b DynamicToolTarget) int { return strings.Compare(a.Name, b.Name) })
	byName := make(map[string]DynamicToolTarget, len(cloned))
	for _, target := range cloned {
		byName[target.Name] = target
	}
	return &dynamicToolGateway{coordinator: c, executor: executor, targets: byName, ordered: cloned}
}

func cloneDynamicToolTargets(targets []DynamicToolTarget) []DynamicToolTarget {
	cloned := slices.Clone(targets)
	for i := range cloned {
		cloned[i].InputSchema = cloneJSONMap(targets[i].InputSchema)
		cloned[i].Parameters = cloneJSONMap(targets[i].Parameters)
		cloned[i].Required = slices.Clone(targets[i].Required)
	}
	return cloned
}

func (g *dynamicToolGateway) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name: dynamicToolGatewayName,
		Description: "Search, inspect, or call an authorized dynamic MCP tool. Search " +
			"first when the exact target or arguments are unknown.",
		Parameters: map[string]any{
			"action":    map[string]any{"type": "string", "enum": []string{"search", "inspect", "call"}},
			"query":     map[string]any{"type": "string"},
			"target":    map[string]any{"type": "string"},
			"arguments": map[string]any{"type": "object", "additionalProperties": true},
		},
		Required: []string{"action"},
	}
}

func (g *dynamicToolGateway) ProviderOptions() fantasy.ProviderOptions { return g.pOpts }
func (g *dynamicToolGateway) SetProviderOptions(opts fantasy.ProviderOptions) {
	g.pOpts = opts
}

func (*dynamicToolGateway) DescribeWorkspaceScope() tools.ToolWorkspaceScopeDescriptor {
	return tools.ToolWorkspaceScopeDescriptor{
		MayReadWorkspace: true, ReadBehavior: tools.PathScopeUnsupported,
		MayWriteWorkspace: true, WriteBehavior: tools.PathScopeUnsupported,
	}
}

type dynamicGatewayRequest struct {
	Action    string          `json:"action"`
	Query     string          `json:"query"`
	Target    string          `json:"target"`
	Arguments json.RawMessage `json:"arguments"`
}

func (g *dynamicToolGateway) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	request, code, err := parseDynamicGatewayRequest(call.Input)
	if err != nil {
		return dynamicGatewayError(code, err.Error()), nil
	}
	switch request.Action {
	case "search":
		return fantasy.NewTextResponse(g.search(request.Query)), nil
	case "inspect":
		target, ok := g.targets[request.Target]
		if !ok {
			return dynamicGatewayError("dynamic_target_not_authorized", "target is not authorized for this attempt"), nil
		}
		return fantasy.NewTextResponse(renderDynamicTargetInspection(target)), nil
	case "call":
		return g.call(ctx, call.ID, request)
	default:
		return dynamicGatewayError("dynamic_invalid_request", "unsupported action"), nil
	}
}

func parseDynamicGatewayRequest(input string) (dynamicGatewayRequest, string, error) {
	if len(input) > maxDynamicGatewayInputBytes {
		return dynamicGatewayRequest{}, "dynamic_input_too_large", fmt.Errorf("gateway input exceeds %d bytes", maxDynamicGatewayInputBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(input)))
	decoder.DisallowUnknownFields()
	var request dynamicGatewayRequest
	if err := decoder.Decode(&request); err != nil {
		return dynamicGatewayRequest{}, "dynamic_invalid_request", fmt.Errorf("input must be one valid JSON object")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return dynamicGatewayRequest{}, "dynamic_invalid_request", err
	}
	if utf8.RuneCountInString(request.Query) > maxDynamicGatewayQueryRunes {
		return dynamicGatewayRequest{}, "dynamic_invalid_request", fmt.Errorf("query exceeds %d Unicode code points", maxDynamicGatewayQueryRunes)
	}
	if len(request.Target) > maxDynamicGatewayTargetBytes {
		return dynamicGatewayRequest{}, "dynamic_invalid_request", fmt.Errorf("target exceeds %d bytes", maxDynamicGatewayTargetBytes)
	}
	switch request.Action {
	case "search":
		if request.Target != "" || request.Arguments != nil {
			return dynamicGatewayRequest{}, "dynamic_invalid_request", fmt.Errorf("search forbids target and arguments")
		}
	case "inspect":
		if request.Target == "" || request.Query != "" || request.Arguments != nil {
			return dynamicGatewayRequest{}, "dynamic_invalid_request", fmt.Errorf("inspect requires target and forbids query and arguments")
		}
	case "call":
		if request.Target == "" || request.Query != "" || request.Arguments == nil {
			return dynamicGatewayRequest{}, "dynamic_invalid_request", fmt.Errorf("call requires target and arguments and forbids query")
		}
	default:
		return dynamicGatewayRequest{}, "dynamic_invalid_request", fmt.Errorf("action must be search, inspect, or call")
	}
	return request, "", nil
}

func dynamicGatewayError(code, message string) fantasy.ToolResponse {
	message = utils.TruncateRunes(utils.RedactSecrets(strings.TrimSpace(message)), 512)
	payload, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": message}})
	return fantasy.NewTextErrorResponse(string(payload))
}

type dynamicSearchResult struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
	score       int
}

func (g *dynamicToolGateway) search(query string) string {
	tokens := dynamicSearchTokens(query)
	results := make([]dynamicSearchResult, 0, len(g.ordered))
	for _, target := range g.ordered {
		score := dynamicTargetScore(target, strings.ToLower(strings.TrimSpace(query)), tokens)
		if len(tokens) > 0 && score == 0 {
			continue
		}
		results = append(results, dynamicSearchResult{
			Name: target.Name, Description: utils.TruncateRunes(target.Description, 512), Kind: target.Kind, score: score,
		})
	}
	slices.SortFunc(results, func(a, b dynamicSearchResult) int {
		if a.score != b.score {
			return b.score - a.score
		}
		return strings.Compare(a.Name, b.Name)
	})
	if len(results) > 12 {
		results = results[:12]
	}
	payload, _ := json.Marshal(struct {
		Results []dynamicSearchResult `json:"results"`
	}{Results: results})
	return string(payload)
}

func dynamicSearchTokens(value string) []string {
	fields := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	slices.Sort(fields)
	return slices.Compact(fields)
}

func dynamicTargetScore(target DynamicToolTarget, normalizedQuery string, queryTokens []string) int {
	name := strings.ToLower(target.Name)
	nameScore := 0
	switch {
	case normalizedQuery != "" && name == normalizedQuery:
		nameScore = 100
	case normalizedQuery != "" && strings.HasPrefix(name, normalizedQuery):
		nameScore = 60
	default:
		nameTokens := dynamicSearchTokens(name)
		for _, token := range queryTokens {
			if strings.Contains(name, token) || slices.Contains(nameTokens, token) {
				nameScore = 30
				break
			}
		}
	}
	description := strings.ToLower(target.Description)
	descriptionScore := 0
	for _, token := range queryTokens {
		if strings.Contains(description, token) {
			descriptionScore += 10
		}
	}
	return nameScore + min(descriptionScore, 40)
}

func renderDynamicTargetInspection(target DynamicToolTarget) string {
	description := utils.TruncateRunes(target.Description, 4096)
	full := map[string]any{
		"name": target.Name, "description": description, "required": target.Required,
		"input_schema": target.InputSchema, "descriptor_sha256": target.DescriptorSHA256, "schema_truncated": false,
	}
	if raw, err := json.Marshal(full); err == nil && len(raw) <= maxDynamicInspectOutputBytes {
		return string(raw)
	}
	required := slices.Clone(target.Required)
	truncatedRequired := false
	for {
		fallback := map[string]any{
			"name": target.Name, "description": description, "required": required,
			"descriptor_sha256": target.DescriptorSHA256, "schema_truncated": true,
		}
		if truncatedRequired {
			fallback["required_truncated"] = true
		}
		raw, _ := json.Marshal(fallback)
		if len(raw) <= maxDynamicInspectOutputBytes || len(required) == 0 {
			return string(raw)
		}
		required = required[:len(required)-1]
		truncatedRequired = true
	}
}

func (g *dynamicToolGateway) call(ctx context.Context, callID string, request dynamicGatewayRequest) (fantasy.ToolResponse, error) {
	target, ok := g.targets[request.Target]
	if !ok {
		target.Name = request.Target
		reportDynamicToolInvocation(ctx, dynamicInvocation(target, callID, "denied", "dynamic_target_not_authorized"), "", "target is not authorized for this attempt")
		return dynamicGatewayError("dynamic_target_not_authorized", "target is not authorized for this attempt"), nil
	}
	input, err := validateDynamicArguments(target.InputSchema, request.Arguments)
	if err != nil {
		reportDynamicToolInvocation(ctx, dynamicInvocation(target, callID, "denied", "dynamic_schema_invalid"), "", err.Error())
		return dynamicGatewayError("dynamic_schema_invalid", err.Error()), nil
	}
	agentName, _ := ctx.Value(tools.AgentNameKey).(string)
	if denial, reason := g.coordinator.authorizeDynamicLogicalInvocation(ctx, agentName, target.Name, input); denial != "" {
		event := dynamicInvocation(target, callID, "denied", reason)
		reportDynamicToolInvocation(ctx, event, input, denial)
		return dynamicGatewayError("dynamic_policy_denied", denial), nil
	}
	reportDynamicToolInvocation(ctx, dynamicInvocation(target, callID, "authorized", ""), input, "")
	reportDynamicToolInvocation(ctx, dynamicInvocation(target, callID, "started", ""), "", "")
	content, isError, err := g.executor.ExecuteAuthorizedTool(ctx, target.Name, target.DescriptorSHA256, input)
	if err != nil {
		code := "dynamic_transport_error"
		if internalmcp.IsToolAuthorizationError(err) {
			code = "dynamic_policy_denied"
		} else if internalmcp.IsToolDescriptorMismatchError(err) {
			code = "dynamic_target_unavailable"
		}
		event := dynamicInvocation(target, callID, "finished", code)
		event.IsError = true
		reportDynamicToolInvocation(ctx, event, "", err.Error())
		return dynamicGatewayError(code, err.Error()), nil
	}
	bounded, truncated, originalBytes := boundDynamicToolOutput(content)
	event := dynamicInvocation(target, callID, "finished", "")
	event.IsError, event.Truncated, event.OriginalBytes = isError, truncated, originalBytes
	reportDynamicToolInvocation(ctx, event, "", bounded)
	if isError {
		return fantasy.NewTextErrorResponse(bounded), nil
	}
	return fantasy.NewTextResponse(bounded), nil
}

func dynamicInvocation(target DynamicToolTarget, callID, phase, reason string) DynamicToolInvocationEvent {
	return DynamicToolInvocationEvent{
		Phase: phase, GatewayTool: dynamicToolGatewayName, LogicalTool: target.Name,
		ToolCallID: callID, DescriptorSHA256: target.DescriptorSHA256, ReasonCode: reason,
	}
}

func boundDynamicToolOutput(content string) (string, bool, int) {
	originalBytes := len(content)
	if originalBytes <= maxDynamicGatewayOutputBytes {
		return content, false, originalBytes
	}
	marker := fmt.Sprintf("\n...[truncated by hufu: original_bytes=%d, limit_bytes=%d]", originalBytes, maxDynamicGatewayOutputBytes)
	limit := maxDynamicGatewayOutputBytes - len(marker)
	if limit < 0 {
		limit = 0
	}
	prefix := []byte(content)
	if len(prefix) > limit {
		prefix = prefix[:limit]
	}
	for !utf8.Valid(prefix) && len(prefix) > 0 {
		prefix = prefix[:len(prefix)-1]
	}
	return string(prefix) + marker, true, originalBytes
}

type dynamicInvocationAccumulator struct {
	mu        sync.Mutex
	limit     int
	receipts  []ToolInvocationReceipt
	truncated int
}

func newDynamicInvocationAccumulator(stepBudget int) *dynamicInvocationAccumulator {
	limit := stepBudget
	if limit <= 0 || limit > 256 {
		limit = 256
	}
	return &dynamicInvocationAccumulator{limit: limit}
}

func (a *dynamicInvocationAccumulator) record(event DynamicToolInvocationEvent) {
	if a == nil || (event.Phase != "denied" && event.Phase != "finished") {
		return
	}
	outcome := "success"
	if event.Phase == "denied" {
		outcome = "denied"
	} else if event.IsError || event.ReasonCode != "" {
		outcome = "error"
	}
	receipt := ToolInvocationReceipt{
		GatewayTool: event.GatewayTool, LogicalTool: event.LogicalTool, ToolCallID: event.ToolCallID,
		DescriptorSHA256: event.DescriptorSHA256, Outcome: outcome, ReasonCode: event.ReasonCode,
		Truncated: event.Truncated, OriginalBytes: event.OriginalBytes,
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.receipts) >= a.limit {
		a.truncated++
		return
	}
	a.receipts = append(a.receipts, receipt)
}

func (a *dynamicInvocationAccumulator) snapshot() ([]ToolInvocationReceipt, int) {
	if a == nil {
		return nil, 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.receipts), a.truncated
}

func dynamicInvocationDiagnosticsReporter(c *Coordinator, transcript *taskTranscript, evidence *toolCallEvidence, agentName, todoID string) dynamicToolInvocationDiagnostics {
	return func(event DynamicToolInvocationEvent, input, result string) {
		if event.Phase == "authorized" || event.Phase == "denied" {
			boundedInput := utils.TruncateRunes(utils.RedactSecrets(input), 500)
			if transcript != nil {
				_ = transcript.RecordToolCall(event.ToolCallID, event.LogicalTool, boundedInput)
			}
			audit.LogToolCall(agentName, event.LogicalTool, boundedInput, event.ToolCallID)
			c.report(c.newEvent("tool_call").withAgent(agentName).withTodoID(todoID).withTool(event.LogicalTool, boundedInput))
			if evidence != nil {
				evidence.toolName = event.LogicalTool
				evidence.toolInput = boundedInput
			}
			if event.Phase == "authorized" {
				c.taskTracker.TodoList().SetLastOperation(todoID, event.LogicalTool)
				if c.skillDetector != nil {
					c.skillDetector.RecordToolCall(agentName, event.LogicalTool, boundedInput, "dynamic MCP invocation")
				}
			}
		}
		if event.Phase == "denied" || event.Phase == "finished" {
			isError := event.Phase == "denied" || event.IsError || event.ReasonCode != ""
			boundedResult := utils.TruncateRunes(utils.RedactSecrets(result), 500)
			if transcript != nil {
				_ = transcript.RecordToolResult(event.ToolCallID, event.LogicalTool, boundedResult, isError)
			}
			audit.LogToolResult(agentName, event.LogicalTool, boundedResult, isError, event.ToolCallID)
			c.report(c.newEvent("tool_result").withAgent(agentName).withTodoID(todoID).withToolResult(event.LogicalTool, boundedResult))
			if evidence != nil {
				evidence.resultText = boundedResult
				evidence.resultErr = isError
			}
		}
	}
}

func (c *Coordinator) authorizeDynamicLogicalInvocation(ctx context.Context, agentName, toolName, input string) (string, string) {
	if c == nil {
		return "coordinator authorization is unavailable", "tool_authorization_error"
	}
	denial, err := c.authorizeToolInvocation(ctx, agentName, toolName)
	if err != nil {
		return err.Error(), "tool_authorization_error"
	}
	if denial != "" {
		return denial, "tool_authorization_denied"
	}
	allowed, _, reason, err := tools.CheckToolPermissionDetail(ctx, toolName)
	if err != nil {
		return err.Error(), "tool_permission_error"
	}
	if !allowed {
		return reason, "tool_permission_denied"
	}
	if denial := artifactScopeToolDenial(ctx, toolName, nil); denial != "" {
		return denial, "artifact_scope_unsupported"
	}
	if readOnly, _ := ctx.Value(tools.AgentReadOnlyExecutionKey).(bool); readOnly && readOnlyToolMutation(toolName, input) {
		return fmt.Sprintf("tool %q is denied for side_effect:none tasks", toolName), "read_only_tool_denied"
	}
	todoID, _ := ctx.Value(todoIDKey{}).(string)
	if denial := c.checkpointDenial(todoID); denial != "" {
		return denial, ReasonKillCriterionReached
	}
	if denial := c.commitGateDenial(ctx, todoID, toolName, input); denial != "" {
		return denial, "commit_gate_blocked"
	}
	return "", ""
}

var _ dynamicToolExecutor = (*internalmcp.MCPToolManager)(nil)
