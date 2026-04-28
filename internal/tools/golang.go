package tools

import (
	"bytes"
	"context"
	"os"
	"time"

	"charm.land/fantasy"
	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
	"github.com/traefik/yaegi/stdlib/unrestricted"
)

const defaultGolangTimeout = 120 * time.Second
const maxGolangTimeout = 600 * time.Second

type golangArgs struct {
	Code    string  `json:"code"`
	Timeout float64 `json:"timeout,omitempty"`
}

func NewGolangTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "golang",
			Description: "Execute Go code using the yaegi interpreter. Returns stdout output. Supports the full Go standard library with explicit import statements. Code must include package declaration and import statements.",
			Parameters: map[string]any{
				"code": map[string]any{
					"type":        "string",
					"description": "Go source code to execute (must include package declaration and imports)",
				},
				"timeout": map[string]any{
					"type":        "number",
					"description": "Timeout in seconds (optional, default 120s, max 600s)",
				},
			},
			Required: []string{"code"},
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeGolang(ctx, call, cfg.WorkDir)
		},
	}
}

func executeGolang(ctx context.Context, call fantasy.ToolCall, workDir string) (fantasy.ToolResponse, error) {
	var args golangArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse("code parameter is required"), nil
	}
	if args.Code == "" {
		return fantasy.NewTextErrorResponse("code parameter is required"), nil
	}

	timeout := defaultGolangTimeout
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout) * time.Second
		if timeout > maxGolangTimeout {
			timeout = maxGolangTimeout
		}
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer

	i := interp.New(interp.Options{
		Stdout:      &stdout,
		Stderr:      &stderr,
		Unrestricted: true,
	})

	if err := i.Use(stdlib.Symbols); err != nil {
		return fantasy.NewTextErrorResponse("failed to load stdlib symbols: " + err.Error()), nil
	}
	if err := i.Use(unrestricted.Symbols); err != nil {
		return fantasy.NewTextErrorResponse("failed to load unrestricted symbols: " + err.Error()), nil
	}
	if err := i.Use(interp.Symbols); err != nil {
		return fantasy.NewTextErrorResponse("failed to load interpreter symbols: " + err.Error()), nil
	}

	if workDir != "" {
		origDir, _ := os.Getwd()
		os.Chdir(workDir)
		defer os.Chdir(origDir)
	}

	_, err := i.EvalWithContext(cmdCtx, args.Code)

	if cmdCtx.Err() == context.DeadlineExceeded {
		return fantasy.NewTextErrorResponse("go execution timed out"), nil
	}

	exitCode := 0
	if err != nil {
		exitCode = 1
	}

	return buildBashResponse(stdout.String(), errToStderr(err, stderr.String()), exitCode), nil
}

func errToStderr(err error, stderr string) string {
	if err == nil {
		return stderr
	}
	var result string
	if stderr != "" {
		result = stderr + "\n"
	}
	result += err.Error()
	return result
}