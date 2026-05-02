package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"charm.land/fantasy"
	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
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
			Description: "Execute Go code using the yaegi interpreter. Returns stdout output. Supports the Go standard library (no os/exec). Code must include package declaration and import statements.",
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
			return executeGolang(ctx, call, cfg)
		},
	}
}

type golangResult struct {
	stdout   string
	stderr   string
	err      error
	timedOut bool
}

func executeGolang(ctx context.Context, call fantasy.ToolCall, cfg ToolConfig) (fantasy.ToolResponse, error) {
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

	ch := make(chan golangResult, 1)

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		if cfg.WorkDir != "" {
			origDir, _ := os.Getwd()
			os.Chdir(cfg.WorkDir)
			defer os.Chdir(origDir)
		}

		var stdout, stderr bytes.Buffer

		i := interp.New(interp.Options{
			Stdout: &stdout,
			Stderr: &stderr,
		})

		if err := i.Use(stdlib.Symbols); err != nil {
			ch <- golangResult{err: err}
			return
		}
		if err := i.Use(interp.Symbols); err != nil {
			ch <- golangResult{err: err}
			return
		}

		_, evalErr := i.EvalWithContext(cmdCtx, args.Code)

		ch <- golangResult{
			stdout:   stdout.String(),
			stderr:   stderr.String(),
			err:      evalErr,
			timedOut: cmdCtx.Err() == context.DeadlineExceeded,
		}
	}()

	res := <-ch

	if res.timedOut {
		return fantasy.NewTextErrorResponse("go execution timed out"), nil
	}

	if res.err != nil && res.stderr == "" {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to run go code: %v", res.err)), nil
	}

	exitCode := 0
	if res.err != nil {
		exitCode = 1
	}

	return buildBashResponse(res.stdout, errToStderr(res.err, res.stderr), exitCode), nil
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
