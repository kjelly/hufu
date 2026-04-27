package tools

import (
	"bytes"
	"context"
	"os"
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
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "lua",
			Description: "Execute Lua code in a sandboxed environment. Returns stdout output. Supports string, math, table, coroutine, and io (without io.popen). The os library is restricted: only os.clock, os.time, os.date, and os.difftime are available. The debug library is disabled.",
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
			return executeLua(ctx, call, cfg.WorkDir)
		},
	}
}

func executeLua(ctx context.Context, call fantasy.ToolCall, workDir string) (fantasy.ToolResponse, error) {
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

	sandboxLua(L)

	var buf bytes.Buffer
	overridePrint(L, &buf)

	if workDir != "" {
		origDir, _ := os.Getwd()
		os.Chdir(workDir)
		defer os.Chdir(origDir)
	}

	if err := L.DoString(args.Code); err != nil {
		errMsg := err.Error()
		if cmdCtx.Err() == context.DeadlineExceeded {
			return fantasy.NewTextErrorResponse("lua execution timed out"), nil
		}
		return buildBashResponse(buf.String(), errMsg, 1), nil
	}

	output := buf.String()
	tr := TruncateTail(output, defaultMaxLines, defaultMaxBytes)
	return fantasy.NewTextResponse(tr.Content), nil
}

func sandboxLua(L *lua.LState) {
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
	}

	L.SetField(global, "dofile", lua.LNil)
	L.SetField(global, "loadfile", lua.LNil)
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

