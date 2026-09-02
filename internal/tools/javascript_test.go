//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
)

func ctxWithJSAllowed(ctx context.Context) context.Context {
	return SetToolsAllowed(ctx, []string{"javascript"})
}

// runJS drives the tool through the full coreTool.Run path (permission
// checks, hooks, validation) like production use, and parses the response as
// the tool's JSON envelope.
func runJS(t *testing.T, code string, input interface{}, timeout float64) (fantasy.ToolResponse, jsResultEnvelope) {
	t.Helper()
	fields := map[string]interface{}{"code": code}
	if input != nil {
		fields["input"] = input
	}
	if timeout > 0 {
		fields["timeout"] = timeout
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal call input: %v", err)
	}
	tool := NewJavascriptTool()
	ctx := ctxWithJSAllowed(t.Context())
	resp, err := tool.Run(ctx, fantasy.ToolCall{Name: "javascript", Input: string(payload)})
	if err != nil {
		t.Fatalf("Run() returned unexpected error: %v", err)
	}
	var env jsResultEnvelope
	if uerr := json.Unmarshal([]byte(resp.Content), &env); uerr != nil {
		t.Fatalf("response content is not a valid JSON envelope: %v\ncontent: %s", uerr, resp.Content)
	}
	return resp, env
}

func TestJavascriptToolInfo(t *testing.T) {
	tool := NewJavascriptTool()
	info := tool.Info()
	if info.Name != "javascript" {
		t.Errorf("Name = %q, want %q", info.Name, "javascript")
	}
	if len(info.Required) != 1 || info.Required[0] != "code" {
		t.Errorf("Required = %v, want [code]", info.Required)
	}
}

// ============== Basic behavior ==============

func TestJavascript_BasicBehavior(t *testing.T) {
	tests := []struct {
		name  string
		code  string
		input interface{}
		want  interface{}
	}{
		{"literal number", "42", nil, float64(42)},
		{"literal string", "'hello'", nil, "hello"},
		{"plain object", "({a: 1, b: 'x'})", nil, map[string]interface{}{"a": float64(1), "b": "x"}},
		{"array", "[1,2,3]", nil, []interface{}{float64(1), float64(2), float64(3)}},
		{"nested object", "({a: {b: [1,2]}})", nil, map[string]interface{}{"a": map[string]interface{}{"b": []interface{}{float64(1), float64(2)}}}},
		{"input injection", "input.value * 2", map[string]interface{}{"value": 21}, float64(42)},
		{"map", "[1,2,3].map(x => x * 2)", nil, []interface{}{float64(2), float64(4), float64(6)}},
		{"filter", "[1,2,3,4].filter(x => x % 2 === 0)", nil, []interface{}{float64(2), float64(4)}},
		{"reduce", "[1,2,3,4].reduce((a,b) => a+b, 0)", nil, float64(10)},
		{"sort", "[3,1,2].sort()", nil, []interface{}{float64(1), float64(2), float64(3)}},
		{"regex", `'hello world'.match(/w\w+/)[0]`, nil, "world"},
		{"JSON.parse", `JSON.parse('{"a":1}').a`, nil, float64(1)},
		{"JSON.stringify", "JSON.stringify({a:1})", nil, `{"a":1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, env := runJS(t, tt.code, tt.input, 0)
			if !env.OK {
				t.Fatalf("expected ok=true, got error: %+v", env.Error)
			}
			if !reflect.DeepEqual(env.Result, tt.want) {
				t.Errorf("result = %#v, want %#v", env.Result, tt.want)
			}
		})
	}
}

func TestJavascript_ConsoleLogCapture(t *testing.T) {
	_, env := runJS(t, "console.log('processing', 3, 'items'); 'done'", nil, 0)
	if !env.OK {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	if env.Result != "done" {
		t.Errorf("result = %v, want done", env.Result)
	}
	if !strings.Contains(env.Logs, "processing 3 items") {
		t.Errorf("logs = %q, want to contain %q", env.Logs, "processing 3 items")
	}
}

func TestJavascript_LogsDefaultEmpty(t *testing.T) {
	_, env := runJS(t, "1+1", nil, 0)
	if env.Logs != "" {
		t.Errorf("logs = %q, want empty", env.Logs)
	}
}

func TestJavascript_UndefinedResultKind(t *testing.T) {
	_, env := runJS(t, "var x = 5;", nil, 0)
	if !env.OK {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	if env.Result != nil {
		t.Errorf("result = %v, want nil", env.Result)
	}
	if env.ResultKind != "undefined" {
		t.Errorf("result_kind = %q, want undefined", env.ResultKind)
	}
}

// ============== Isolation ==============

func TestJavascript_PrototypeMutationDoesNotLeak(t *testing.T) {
	_, env1 := runJS(t, "Object.prototype.polluted = true; ({}).polluted", nil, 0)
	if !env1.OK || env1.Result != true {
		t.Fatalf("setup call failed: %+v", env1)
	}
	_, env2 := runJS(t, "typeof ({}).polluted", nil, 0)
	if !env2.OK || env2.Result != "undefined" {
		t.Errorf("prototype pollution leaked across invocations: %+v", env2)
	}
}

func TestJavascript_GlobalVariableDoesNotLeak(t *testing.T) {
	_, env1 := runJS(t, "globalThis.leaked = 42; leaked", nil, 0)
	if !env1.OK || env1.Result != float64(42) {
		t.Fatalf("setup call failed: %+v", env1)
	}
	_, env2 := runJS(t, "typeof leaked", nil, 0)
	if !env2.OK || env2.Result != "undefined" {
		t.Errorf("global variable leaked across invocations: %+v", env2)
	}
}

// TestJavascript_ParallelInvocationsDoNotShareState calls the same tool
// instance concurrently from many goroutines. It intentionally avoids
// calling any t.* failure method from inside the goroutines (only the
// spawning goroutine may call FailNow/Fatalf) and instead collects errors to
// report after Wait.
func TestJavascript_ParallelInvocationsDoNotShareState(t *testing.T) {
	const n = 20
	tool := NewJavascriptTool()
	ctx := ctxWithJSAllowed(t.Context())

	var wg sync.WaitGroup
	errs := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload, _ := json.Marshal(map[string]interface{}{"code": "input * 2", "input": i})
			resp, err := tool.Run(ctx, fantasy.ToolCall{Input: string(payload)})
			if err != nil {
				errs[i] = fmt.Sprintf("Run error: %v", err)
				return
			}
			var env jsResultEnvelope
			if uerr := json.Unmarshal([]byte(resp.Content), &env); uerr != nil {
				errs[i] = fmt.Sprintf("bad envelope: %v (%s)", uerr, resp.Content)
				return
			}
			if !env.OK {
				errs[i] = fmt.Sprintf("execution error: %+v", env.Error)
				return
			}
			if want := float64(i * 2); env.Result != want {
				errs[i] = fmt.Sprintf("result = %v, want %v", env.Result, want)
			}
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != "" {
			t.Errorf("goroutine %d: %s", i, e)
		}
	}
}

func TestJavascript_NoChdirSideEffects(t *testing.T) {
	before, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	_, env := runJS(t, "1+1", nil, 0)
	if !env.OK {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	after, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if before != after {
		t.Errorf("working directory changed: %q -> %q", before, after)
	}
}

// ============== Forbidden capabilities ==============

func TestJavascript_ForbiddenGlobalsAreAbsent(t *testing.T) {
	for _, name := range jsForbiddenGlobals {
		t.Run(name, func(t *testing.T) {
			_, env := runJS(t, "typeof "+name, nil, 0)
			if !env.OK {
				t.Fatalf("unexpected error checking %q: %+v", name, env.Error)
			}
			if env.Result != "undefined" {
				t.Errorf("typeof %s = %v, want undefined", name, env.Result)
			}
		})
	}
}

func TestJavascript_GlobalThisHasNoPrivilegedCapabilities(t *testing.T) {
	_, env := runJS(t, "typeof globalThis.process", nil, 0)
	if !env.OK || env.Result != "undefined" {
		t.Errorf("globalThis.process should be undefined, got %+v", env)
	}
}

// ============== Error handling ==============

func TestJavascript_ErrorHandling(t *testing.T) {
	tests := []struct {
		name        string
		code        string
		wantErrType string
		wantSubstr  string
	}{
		{"syntax error", "const x = ;", "execution_error", ""},
		{"reference error", "undefinedVar123", "execution_error", "ReferenceError"},
		{"thrown error", "throw new Error('boom')", "execution_error", "boom"},
		{"cyclic result", "const x = {}; x.self = x; x", "result_serialization_error", ""},
		{"function result", "(function(){})", "result_serialization_error", ""},
		{"NaN result", "NaN", "result_serialization_error", ""},
		{"Infinity result", "Infinity", "result_serialization_error", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, env := runJS(t, tt.code, nil, 0)
			if env.OK {
				t.Fatalf("expected error, got ok result: %+v", env.Result)
			}
			if !resp.IsError {
				t.Error("expected ToolResponse.IsError = true")
			}
			if env.Error == nil || env.Error.Type != tt.wantErrType {
				t.Errorf("error = %+v, want type %q", env.Error, tt.wantErrType)
			}
			if tt.wantSubstr != "" && (env.Error == nil || !strings.Contains(env.Error.Message, tt.wantSubstr)) {
				t.Errorf("error message = %+v, want substring %q", env.Error, tt.wantSubstr)
			}
		})
	}
}

func TestJavascript_InvalidArguments(t *testing.T) {
	tool := NewJavascriptTool()
	ctx := ctxWithJSAllowed(t.Context())

	resp, err := tool.Run(ctx, fantasy.ToolCall{Input: `{"code": "1+1", "input": }`})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !resp.IsError {
		t.Fatalf("expected error for malformed JSON args, got %s", resp.Content)
	}
	var env jsResultEnvelope
	if uerr := json.Unmarshal([]byte(resp.Content), &env); uerr != nil {
		t.Fatalf("expected JSON envelope for malformed args, got %q", resp.Content)
	}
	if env.Error == nil || env.Error.Type != "invalid_arguments" {
		t.Errorf("error = %+v, want type invalid_arguments", env.Error)
	}
}

func TestJavascript_WhitespaceOnlyCodeIsInvalid(t *testing.T) {
	_, env := runJS(t, "   ", nil, 0)
	if env.OK {
		t.Fatalf("expected error for whitespace-only code, got %+v", env)
	}
	if env.Error == nil || env.Error.Type != "invalid_arguments" {
		t.Errorf("error = %+v, want type invalid_arguments", env.Error)
	}
}

// TestJavascript_MissingCodeIsRejectedUpstream documents that a wholly
// missing "code" field never reaches the tool's own handler: coreTool.Run's
// generic required-field pre-check rejects it first. Only IsError is
// guaranteed at that layer, not our JSON envelope.
func TestJavascript_MissingCodeIsRejectedUpstream(t *testing.T) {
	tool := NewJavascriptTool()
	ctx := ctxWithJSAllowed(t.Context())
	resp, err := tool.Run(ctx, fantasy.ToolCall{Input: `{"input": 1}`})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !resp.IsError {
		t.Errorf("expected error for missing code, got %s", resp.Content)
	}
}

func TestJavascript_OversizedCode(t *testing.T) {
	code := strings.Repeat("a", maxJSCodeBytes+1)
	_, env := runJS(t, code, nil, 0)
	if env.OK || env.Error == nil || env.Error.Type != "code_too_large" {
		t.Errorf("error = %+v, want type code_too_large", env.Error)
	}
}

func TestJavascript_OversizedInput(t *testing.T) {
	big := strings.Repeat("a", maxJSInputBytes+1)
	_, env := runJS(t, "input.length", big, 0)
	if env.OK || env.Error == nil || env.Error.Type != "input_too_large" {
		t.Errorf("error = %+v, want type input_too_large", env.Error)
	}
}

func TestJavascript_OversizedResult(t *testing.T) {
	_, env := runJS(t, "'a'.repeat(3*1024*1024)", nil, 0)
	if env.OK || env.Error == nil || env.Error.Type != "result_limit_exceeded" {
		t.Errorf("error = %+v, want type result_limit_exceeded", env.Error)
	}
}

func TestJavascript_OversizedConsoleOutputIsTruncatedNotFailed(t *testing.T) {
	code := `
		var chunk = 'x'.repeat(1000);
		for (var i = 0; i < 2000; i++) { console.log(chunk); }
		'done';
	`
	_, env := runJS(t, code, nil, 20)
	if !env.OK {
		t.Fatalf("expected success despite oversized logs, got error: %+v", env.Error)
	}
	if env.Result != "done" {
		t.Errorf("result = %v, want done", env.Result)
	}
	if !strings.Contains(env.Logs, "[log output truncated") {
		t.Errorf("expected logs to be marked truncated, got length %d", len(env.Logs))
	}
	if len(env.Logs) > maxJSLogBytes+100 {
		t.Errorf("logs length %d exceeds bound", len(env.Logs))
	}
}

func TestJavascript_ExecutionTimeout(t *testing.T) {
	start := time.Now()
	_, env := runJS(t, "while(true){}", nil, 0.2)
	elapsed := time.Since(start)
	if env.OK || env.Error == nil || env.Error.Type != "execution_timeout" {
		t.Errorf("error = %+v, want type execution_timeout", env.Error)
	}
	if elapsed > 5*time.Second {
		t.Errorf("timeout took too long to be enforced: %v", elapsed)
	}
}

func TestJavascript_ParentContextCancellation(t *testing.T) {
	tool := NewJavascriptTool()
	baseCtx := ctxWithJSAllowed(t.Context())
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	payload, _ := json.Marshal(map[string]interface{}{"code": "while(true){}"})
	resp, err := tool.Run(ctx, fantasy.ToolCall{Input: string(payload)})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	var env jsResultEnvelope
	if uerr := json.Unmarshal([]byte(resp.Content), &env); uerr != nil {
		t.Fatalf("bad envelope: %v (%s)", uerr, resp.Content)
	}
	if env.OK || env.Error == nil || env.Error.Type != "execution_cancelled" {
		t.Errorf("error = %+v, want type execution_cancelled", env.Error)
	}
}

// ============== Determinism ==============

func TestJavascript_DeterministicRepeatedExecution(t *testing.T) {
	code := "input.items.filter(x => x.score >= 80).sort((a,b) => b.score - a.score)"
	input := map[string]interface{}{
		"items": []map[string]interface{}{
			{"name": "a", "score": 81},
			{"name": "b", "score": 62},
			{"name": "c", "score": 95},
		},
	}
	var first string
	for i := 0; i < 5; i++ {
		resp, env := runJS(t, code, input, 0)
		if !env.OK {
			t.Fatalf("run %d failed: %+v", i, env.Error)
		}
		if i == 0 {
			first = resp.Content
		} else if resp.Content != first {
			t.Errorf("run %d produced different output:\n%s\nvs\n%s", i, resp.Content, first)
		}
	}
}

func TestJavascript_MathRandomDisabled(t *testing.T) {
	_, env := runJS(t, "Math.random()", nil, 0)
	if env.OK {
		t.Fatalf("expected Math.random() to be disabled, got result %v", env.Result)
	}
	if env.Error == nil || !strings.Contains(env.Error.Message, "Math.random") {
		t.Errorf("error = %+v, want mention of Math.random", env.Error)
	}
}

func TestJavascript_AmbientDateDisabled(t *testing.T) {
	for _, code := range []string{"Date.now()", "new Date()"} {
		t.Run(code, func(t *testing.T) {
			_, env := runJS(t, code, nil, 0)
			if env.OK {
				t.Fatalf("expected %q to be disabled, got result %v", code, env.Result)
			}
		})
	}
}

func TestJavascript_ExplicitDateStillWorks(t *testing.T) {
	_, env := runJS(t, "new Date(2020, 0, 1).getFullYear()", nil, 0)
	if !env.OK || env.Result != float64(2020) {
		t.Errorf("explicit Date construction should work, got %+v", env)
	}

	_, env2 := runJS(t, "Date.parse('2020-01-01T00:00:00.000Z')", nil, 0)
	if !env2.OK || env2.Result != float64(1577836800000) {
		t.Errorf("Date.parse should work, got %+v", env2)
	}
}

// ============== Policy integration ==============

func TestJavascript_PolicySurfaces(t *testing.T) {
	if !highRiskTools["javascript"] {
		t.Error("javascript should be in highRiskTools")
	}
	if !ForceMCPBlockedTools["javascript"] {
		t.Error("javascript should be in ForceMCPBlockedTools")
	}
}

func TestJavascript_RegisteredInAllTools(t *testing.T) {
	for _, tool := range AllTools() {
		if tool.Info().Name == "javascript" {
			return
		}
	}
	t.Error("javascript tool not registered in AllTools()")
}

func TestJavascript_ForceMCPBlocksDirectExecution(t *testing.T) {
	tool := NewJavascriptTool()
	ctx := context.WithValue(context.Background(), AgentForceMCPKey, true)
	resp, err := tool.Run(ctx, fantasy.ToolCall{Input: `{"code": "1+1"}`})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !resp.IsError {
		t.Error("expected error when --force-mcp is enabled")
	}
	if !strings.Contains(resp.Content, "blocked by --force-mcp") {
		t.Errorf("expected force-mcp error message, got %q", resp.Content)
	}
}

func TestJavascript_ForceMCPExcludesFromAllTools(t *testing.T) {
	for _, tool := range AllTools(WithForceMCP(true)) {
		if tool.Info().Name == "javascript" {
			t.Error("javascript should be excluded from AllTools when ForceMCP is enabled")
		}
	}
}
