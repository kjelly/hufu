//go:build linux || darwin
// +build linux darwin

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
	cfg.ToolName = "golang"
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "golang",
			Description: "Execute Go code using the yaegi interpreter. Returns stdout output. Dangerous packages (os, os/exec, net, net/http, syscall, unsafe, plugin, reflect, runtime, debug) are blocked. Code must include package declaration and import statements. Prefer this over a multi-line bash pipeline (grep/awk/sed chains) for parsing, counting, or aggregating command output into a report: it has no pipefail/exit-code pitfalls and gives real typed data structures.",
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

		// Filter stdlib symbols to remove dangerous packages.
		// - os/exec: subprocess execution
		// - net, net/http: network access
		// - syscall: low-level system calls
		// - unsafe: pointer arithmetic and memory access
		// - plugin: dynamic code loading
		// - reflect: access to unexported fields via Interface()
		// - runtime: memory stats, SetFinalizer abuse
		// - debug/*: arbitrary memory reading (elf, macho, pe)
		// - io/ioutil: file I/O utilities
		safeSymbols := make(interp.Exports)
		dangerousPkgs := map[string]bool{
			"os":        true,
			"os/exec":   true,
			"net":       true,
			"net/http":  true,
			"syscall":   true,
			"unsafe":    true,
			"plugin":    true,
			"reflect":   true,
			"runtime":   true,
			"debug":     true,
			"io/ioutil": true,
		}
		for pkg, symbols := range stdlib.Symbols {
			if !dangerousPkgs[pkg] {
				safeSymbols[pkg] = symbols
			}
		}

		if err := i.Use(safeSymbols); err != nil {
			ch <- golangResult{err: err}
			return
		}
		// Filter interp.Symbols the same way — yaegi's built-in exports
		// may still expose dangerous packages like unsafe or reflect.
		safeInterpSymbols := make(interp.Exports)
		for pkg, symbols := range interp.Symbols {
			if !dangerousPkgs[pkg] {
				safeInterpSymbols[pkg] = symbols
			}
		}
		if err := i.Use(safeInterpSymbols); err != nil {
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
