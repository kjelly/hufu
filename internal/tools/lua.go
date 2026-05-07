package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"charm.land/fantasy"
	lua "github.com/yuin/gopher-lua"
)

const defaultLuaTimeout = 120 * time.Second
const maxLuaTimeout = 600 * time.Second

type luaArgs struct {
	Code    string  `json:"code"`
	Timeout float64 `json:"timeout,omitempty"`
}

func NewLuaTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	cfg.ToolName = "lua"
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "lua",
			Description: "Execute Lua code in a sandboxed environment. Returns stdout output. Supports string, math, table, coroutine, and restricted io/os libraries. The debug library is disabled. File I/O requires path consent for paths outside allowed directories.",
			Parameters: map[string]any{
				"code": map[string]any{
					"type":        "string",
					"description": "Lua code to execute",
				},
				"timeout": map[string]any{
					"type":        "number",
					"description": "Timeout in seconds (optional, default 120s, max 600s)",
				},
			},
			Required: []string{"code"},
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeLua(ctx, call, cfg)
		},
	}
}

type luaResult struct {
	output   string
	errMsg   string
	timedOut bool
}

func executeLua(ctx context.Context, call fantasy.ToolCall, cfg ToolConfig) (fantasy.ToolResponse, error) {
	var args luaArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse("code parameter is required"), nil
	}
	if args.Code == "" {
		return fantasy.NewTextErrorResponse("code parameter is required"), nil
	}

	timeout := defaultLuaTimeout
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout) * time.Second
		if timeout > maxLuaTimeout {
			timeout = maxLuaTimeout
		}
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	projectDir := cfg.WorkDir
	if projectDir == "" {
		projectDir, _ = os.Getwd()
	}
	projectDir = filepath.Clean(projectDir)

	ch := make(chan luaResult, 1)

	luaAllowedPaths := mergedAllowedPaths(cfg, ctx)
	luaPathConsent := cfg.PathConsent

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		if cfg.WorkDir != "" {
			origDir, _ := os.Getwd()
			os.Chdir(cfg.WorkDir)
			defer os.Chdir(origDir)
		}

		L := lua.NewState(lua.Options{
			SkipOpenLibs: true,
		})
		defer L.Close()

		L.SetContext(cmdCtx)

		lua.OpenBase(L)
		lua.OpenString(L)
		lua.OpenMath(L)
		lua.OpenTable(L)
		lua.OpenCoroutine(L)
		lua.OpenPackage(L)
		lua.OpenIo(L)
		lua.OpenOs(L)

		sandboxLua(L, projectDir, luaAllowedPaths, luaPathConsent)

		var buf bytes.Buffer
		overridePrint(L, &buf)

		if err := L.DoString(args.Code); err != nil {
			errMsg := err.Error()
			ch <- luaResult{
				output:   buf.String(),
				errMsg:   errMsg,
				timedOut: cmdCtx.Err() == context.DeadlineExceeded,
			}
			return
		}

		ch <- luaResult{output: buf.String()}
	}()

	res := <-ch

	if res.timedOut {
		return fantasy.NewTextErrorResponse("lua execution timed out"), nil
	}

	if res.errMsg != "" {
		return buildBashResponse(res.output, res.errMsg, 1), nil
	}

	output := res.output
	tr := TruncateTail(output, defaultMaxLines, defaultMaxBytes)
	return fantasy.NewTextResponse(tr.Content), nil
}

func sandboxLua(L *lua.LState, projectDir string, allowedPaths []string, consent *PathConsent) {
	global := L.Get(lua.EnvironIndex).(*lua.LTable)

	L.SetGlobal(lua.DebugLibName, lua.LNil)

	osTbl := L.GetGlobal(lua.OsLibName)
	if tbl, ok := osTbl.(*lua.LTable); ok {
		safeOsFuncs := map[string]bool{
			"clock":    true,
			"time":     true,
			"date":     true,
			"difftime": true,
		}
		var remove []string
		tbl.ForEach(func(key, _ lua.LValue) {
			keyStr := key.String()
			if !safeOsFuncs[keyStr] {
				remove = append(remove, keyStr)
			}
		})
		for _, k := range remove {
			tbl.RawSetString(k, lua.LNil)
		}
	}

	ioTbl := L.GetGlobal(lua.IoLibName)
	if tbl, ok := ioTbl.(*lua.LTable); ok {
		tbl.RawSetString("popen", lua.LNil)
		tbl.RawSetString("tmpfile", lua.LNil)

		originalOpen := L.GetGlobal("io").(*lua.LTable).RawGetString("open")
		tbl.RawSetString("open", L.NewFunction(func(L *lua.LState) int {
			path := L.CheckString(1)
			top := L.GetTop()
			absPath, err := validateLuaPathWithConsent(path, projectDir, allowedPaths, consent)
			if err != nil {
				L.Push(lua.LNil)
				L.Push(lua.LString(err.Error()))
				return 2
			}
			L.Push(originalOpen)
			L.Push(lua.LString(absPath))
			if top >= 2 {
				L.Push(L.Get(2))
			}
			nargs := top
			if nargs < 1 {
				nargs = 1
			}
			L.Call(nargs, 2)
			return 2
		}))

		originalLines := L.GetGlobal("io").(*lua.LTable).RawGetString("lines")
		tbl.RawSetString("lines", L.NewFunction(func(L *lua.LState) int {
			path := L.CheckString(1)
			top := L.GetTop()
			absPath, err := validateLuaPathWithConsent(path, projectDir, allowedPaths, consent)
			if err != nil {
				L.RaiseError("path outside project directory: %s", path)
			}
			L.Push(originalLines)
			L.Push(lua.LString(absPath))
			if top >= 2 {
				for i := 2; i <= top; i++ {
					L.Push(L.Get(i))
				}
			}
			L.Call(top, lua.MultRet)
			return L.GetTop()
		}))
	}

	L.SetField(global, "dofile", lua.LNil)
	L.SetField(global, "loadfile", lua.LNil)

	pkgTbl := L.GetGlobal("package")
	if tbl, ok := pkgTbl.(*lua.LTable); ok {
		tbl.RawSetString("path", lua.LString(""))
		tbl.RawSetString("cpath", lua.LString(""))
		tbl.RawSetString("searchers", lua.LNil)
		tbl.RawSetString("loaders", lua.LNil)
	}
}

func validateLuaPath(path, projectDir string) (string, error) {
	absPath := path
	if !filepath.IsAbs(path) {
		absPath = filepath.Join(projectDir, path)
	}
	absPath = filepath.Clean(absPath)

	evaluatedProjDir, err := filepath.EvalSymlinks(projectDir)
	if err != nil {
		return "", fmt.Errorf("cannot resolve project directory: %w", err)
	}
	evaluatedProjDir = filepath.Clean(evaluatedProjDir)

	evaluatedPath, err := filepath.EvalSymlinks(absPath)
	if err == nil {
		if !strings.HasPrefix(evaluatedPath, evaluatedProjDir+string(filepath.Separator)) && evaluatedPath != evaluatedProjDir {
			return "", fmt.Errorf("path '%s' is outside the project directory", path)
		}
		return evaluatedPath, nil
	}

	parentDir := filepath.Dir(absPath)
	evaluatedDir, err := filepath.EvalSymlinks(parentDir)
	if err != nil {
		return "", fmt.Errorf("path '%s' is invalid or cannot be resolved: %w", path, err)
	}
	evaluatedPath = filepath.Join(evaluatedDir, filepath.Base(absPath))

	if !strings.HasPrefix(evaluatedDir, evaluatedProjDir+string(filepath.Separator)) && evaluatedDir != evaluatedProjDir {
		return "", fmt.Errorf("path '%s' is outside the project directory", path)
	}
	return evaluatedPath, nil
}

func validateLuaPathWithConsent(path, projectDir string, allowedPaths []string, consent *PathConsent) (string, error) {
	absPath := path
	if !filepath.IsAbs(path) {
		absPath = filepath.Join(projectDir, path)
	}
	absPath = filepath.Clean(absPath)

	if isPathAllowed(absPath, allowedPaths) {
		return validateLuaPath(path, projectDir)
	}

	if consent != nil {
		result, err := consent.AskConsent(absPath, "read", "lua", path)
		if err != nil {
			return "", fmt.Errorf("path '%s' is outside allowed paths and consent failed: %w", path, err)
		}
		switch result {
		case ConsentOnce, ConsentAlways:
			evalPath, evalErr := filepath.EvalSymlinks(absPath)
			if evalErr == nil {
				evalPath = filepath.Clean(evalPath)
				if evalPath != absPath && !isPathAllowed(evalPath, allowedPaths) {
					return "", fmt.Errorf("path '%s' resolves to '%s' which is outside allowed paths", path, evalPath)
				}
				return evalPath, nil
			}
			return absPath, nil
		default:
			return "", fmt.Errorf("path '%s' is outside allowed paths — access denied by user", path)
		}
	}

	return "", fmt.Errorf("path %q is outside allowed directories and no consent handler available", path)
}

func overridePrint(L *lua.LState, buf *bytes.Buffer) {
	global := L.Get(lua.EnvironIndex).(*lua.LTable)
	L.SetField(global, "print", L.NewFunction(func(L *lua.LState) int {
		top := L.GetTop()
		var parts []string
		for i := 1; i <= top; i++ {
			parts = append(parts, L.ToStringMeta(L.Get(i)).String())
		}
		buf.WriteString(strings.Join(parts, "\t"))
		buf.WriteString("\n")
		return 0
	}))
}
