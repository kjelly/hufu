//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/dop251/goja"
)

const (
	defaultJSTimeout = 10 * time.Second
	maxJSTimeout     = 60 * time.Second
	maxJSCodeBytes   = 256 * 1024
	maxJSInputBytes  = 2 * 1024 * 1024
	maxJSLogBytes    = 256 * 1024
	maxJSResultBytes = 2 * 1024 * 1024
)

const javascriptToolDescription = `Execute JavaScript for deterministic local computation and structured-data
transformation in an isolated sandbox. The runtime has no filesystem, network,
environment-variable, subprocess, package, Node.js, or timer access. Use this
tool for JSON/object/array/string transformations, sorting, grouping, filtering,
aggregation, regex processing, and small deterministic algorithms. Do not use it
for reading/editing files, running tests, building software, network calls,
system administration, or other side effects; use the appropriate tool for those.`

// jsBootstrapScript neutralizes ambient nondeterminism before user code runs.
// Date parsing/construction with explicit arguments keeps working; the
// zero-argument wall-clock forms and Math.random are disabled so the same
// code+input always produces the same result.
const jsBootstrapScript = `
(function() {
  var OriginalDate = Date;
  function SandboxedDate() {
    if (arguments.length === 0) {
      throw new Error("Date(): ambient wall-clock time is unavailable in the javascript tool; pass explicit arguments");
    }
    var bound = Function.prototype.bind.apply(OriginalDate, [null].concat(Array.prototype.slice.call(arguments)));
    return new bound();
  }
  SandboxedDate.prototype = OriginalDate.prototype;
  SandboxedDate.now = function() {
    throw new Error("Date.now() is unavailable in the javascript tool; the tool is deterministic");
  };
  SandboxedDate.parse = OriginalDate.parse;
  SandboxedDate.UTC = OriginalDate.UTC;
  Date = SandboxedDate;

  Math.random = function() {
    throw new Error("Math.random() is unavailable in the javascript tool; the tool is deterministic");
  };
})();
`

// jsForbiddenGlobals are deleted from the global object defensively. Goja does
// not implement Node.js/browser globals by default, but this keeps the
// contract explicit and stable regardless of engine version changes.
var jsForbiddenGlobals = []string{
	"require", "module", "exports", "process", "Buffer", "global",
	"fetch", "XMLHttpRequest", "WebSocket", "Deno", "Bun",
	"setTimeout", "setInterval", "setImmediate", "clearTimeout", "clearInterval",
	"queueMicrotask", "Worker", "SharedArrayBuffer",
}

type javascriptArgs struct {
	Code    string          `json:"code"`
	Input   json.RawMessage `json:"input,omitempty"`
	Timeout float64         `json:"timeout,omitempty"`
}

type jsErrorInfo struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type jsResultEnvelope struct {
	OK         bool         `json:"ok"`
	Result     interface{}  `json:"result,omitempty"`
	ResultKind string       `json:"result_kind,omitempty"`
	Logs       string       `json:"logs"`
	Error      *jsErrorInfo `json:"error,omitempty"`
}

// NewJavascriptTool builds the built-in `javascript` tool: deterministic,
// local, structured-data computation over a fresh goja.Runtime created per
// invocation. It has no filesystem, network, process, environment, or
// package-loading access; see spec.md for the full contract.
func NewJavascriptTool(opts ...ToolOption) fantasy.AgentTool {
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "javascript",
			Description: javascriptToolDescription,
			Parameters: map[string]any{
				"code": map[string]any{
					"type":        "string",
					"description": "JavaScript source to evaluate. The value of the final expression becomes the result.",
				},
				"input": map[string]any{
					"description": "JSON-compatible value exposed as the global `input` inside the script. Defaults to null.",
				},
				"timeout": map[string]any{
					"type":        "number",
					"description": "Timeout in seconds (optional, default 10s, max 60s).",
				},
			},
			Required: []string{"code"},
			Parallel: true,
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeJavascript(ctx, call)
		},
	}
}

func executeJavascript(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args javascriptArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return jsErrorResponse("invalid_arguments", err.Error(), ""), nil
	}
	if strings.TrimSpace(args.Code) == "" {
		return jsErrorResponse("invalid_arguments", "code parameter is required", ""), nil
	}
	if len(args.Code) > maxJSCodeBytes {
		return jsErrorResponse("code_too_large", fmt.Sprintf("code exceeds maximum size of %d bytes", maxJSCodeBytes), ""), nil
	}
	if len(args.Input) > maxJSInputBytes {
		return jsErrorResponse("input_too_large", fmt.Sprintf("input exceeds maximum size of %d bytes", maxJSInputBytes), ""), nil
	}

	var inputValue interface{}
	if len(args.Input) > 0 {
		if err := json.Unmarshal(args.Input, &inputValue); err != nil {
			return jsErrorResponse("invalid_arguments", fmt.Sprintf("input is not valid JSON: %v", err), ""), nil
		}
	}

	timeout := defaultJSTimeout
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout * float64(time.Second))
		if timeout > maxJSTimeout {
			timeout = maxJSTimeout
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	vm := goja.New()
	logs := newBoundedLog(maxJSLogBytes)

	if err := installJSSandbox(vm); err != nil {
		return jsErrorResponse("internal_error", fmt.Sprintf("failed to initialize sandbox: %v", err), ""), nil
	}
	installBoundedConsole(vm, logs)
	if err := vm.Set("input", inputValue); err != nil {
		return jsErrorResponse("internal_error", fmt.Sprintf("failed to inject input: %v", err), logs.String()), nil
	}

	// A fresh goja.Runtime is used exactly once, by exactly one goroutine
	// (this one plus the watcher below which only ever calls Interrupt),
	// and is discarded after this call returns. Never share it.
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-runCtx.Done():
			vm.Interrupt("execution cancelled")
		case <-watchDone:
		}
	}()

	value, err := vm.RunString(args.Code)
	close(watchDone)

	if err != nil {
		var interrupted *goja.InterruptedError
		if errors.As(err, &interrupted) {
			if runCtx.Err() == context.DeadlineExceeded {
				return jsErrorResponse("execution_timeout", fmt.Sprintf("javascript execution exceeded %s", timeout), logs.String()), nil
			}
			return jsErrorResponse("execution_cancelled", "javascript execution was cancelled", logs.String()), nil
		}
		return jsErrorResponse("execution_error", err.Error(), logs.String()), nil
	}

	if value == nil || goja.IsUndefined(value) {
		return jsSuccessResponse(nil, "undefined", logs.String()), nil
	}

	result, err := exportJSONCompatible(value)
	if err != nil {
		return jsErrorResponse("result_serialization_error", err.Error(), logs.String()), nil
	}

	resultBytes, err := json.Marshal(result)
	if err != nil {
		return jsErrorResponse("result_serialization_error", err.Error(), logs.String()), nil
	}
	if len(resultBytes) > maxJSResultBytes {
		return jsErrorResponse("result_limit_exceeded", fmt.Sprintf("result exceeds maximum size of %d bytes", maxJSResultBytes), logs.String()), nil
	}

	return jsSuccessResponse(result, "", logs.String()), nil
}

// installJSSandbox removes ambient/privileged globals and neutralizes
// nondeterministic sources. Goja does not implement Node/browser globals by
// default, so the explicit deletes are defense in depth rather than undoing
// engine-provided capabilities.
func installJSSandbox(vm *goja.Runtime) error {
	global := vm.GlobalObject()
	for _, name := range jsForbiddenGlobals {
		_ = global.Delete(name)
	}
	if _, err := vm.RunString(jsBootstrapScript); err != nil {
		return fmt.Errorf("running sandbox bootstrap: %w", err)
	}
	return nil
}

type boundedLog struct {
	buf       strings.Builder
	limit     int
	truncated bool
}

func newBoundedLog(limit int) *boundedLog {
	return &boundedLog{limit: limit}
}

func (b *boundedLog) Append(s string) {
	if b.truncated {
		return
	}
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return
	}
	if len(s) > remaining {
		b.buf.WriteString(safeTruncateHeadBytes(s, remaining))
		b.truncated = true
		return
	}
	b.buf.WriteString(s)
}

func (b *boundedLog) String() string {
	if !b.truncated {
		return b.buf.String()
	}
	return b.buf.String() + "\n[log output truncated at limit]"
}

func installBoundedConsole(vm *goja.Runtime, logs *boundedLog) {
	logFn := func(call goja.FunctionCall) goja.Value {
		parts := make([]string, len(call.Arguments))
		for i, arg := range call.Arguments {
			parts[i] = formatConsoleArg(vm, arg)
		}
		logs.Append(strings.Join(parts, " ") + "\n")
		return goja.Undefined()
	}
	console := vm.NewObject()
	_ = console.Set("log", logFn)
	_ = console.Set("warn", logFn)
	_ = console.Set("error", logFn)
	_ = vm.Set("console", console)
}

func formatConsoleArg(vm *goja.Runtime, v goja.Value) string {
	if v == nil || goja.IsUndefined(v) {
		return "undefined"
	}
	if goja.IsNull(v) {
		return "null"
	}
	obj, ok := v.(*goja.Object)
	if !ok {
		return v.String()
	}
	if _, isFn := goja.AssertFunction(v); isFn {
		return "[Function]"
	}
	if jsonObj, ok := vm.GlobalObject().Get("JSON").(*goja.Object); ok {
		if stringify, ok := goja.AssertFunction(jsonObj.Get("stringify")); ok {
			if res, err := stringify(goja.Undefined(), obj); err == nil && res != nil && !goja.IsUndefined(res) {
				return res.String()
			}
		}
	}
	return v.String()
}

func jsErrorResponse(errType, message, logs string) fantasy.ToolResponse {
	env := jsResultEnvelope{
		OK:    false,
		Logs:  logs,
		Error: &jsErrorInfo{Type: errType, Message: message},
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf(`{"ok":false,"error":{"type":"internal_error","message":%q}}`, err.Error()))
	}
	return fantasy.NewTextErrorResponse(string(data))
}

func jsSuccessResponse(result interface{}, resultKind, logs string) fantasy.ToolResponse {
	env := jsResultEnvelope{
		OK:         true,
		Result:     result,
		ResultKind: resultKind,
		Logs:       logs,
	}
	data, err := json.Marshal(env)
	if err != nil {
		return jsErrorResponse("internal_error", fmt.Sprintf("failed to marshal result envelope: %v", err), logs)
	}
	return fantasy.NewTextResponse(string(data))
}
