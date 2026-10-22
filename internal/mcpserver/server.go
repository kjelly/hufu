// Package mcpserver exposes fixed-scope, read-only inspection over MCP stdio.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/team"
)

const (
	RequestTimeout   = 15 * time.Second
	MaxConcurrent    = 4
	MaxResponseBytes = 1 << 20
	MaxRequestBytes  = 64 << 10
)

type observationReader interface {
	Inspect(context.Context, inspect.Kind, string) (*inspect.ScopedObservation, error)
}

type Server struct {
	sdk     *server.MCPServer
	reader  observationReader
	slots   chan struct{}
	timeout time.Duration
	tools   []mcp.Tool
}

func New(ctx context.Context, query inspect.InspectQuery) (*Server, error) {
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	reader, err := inspect.NewScopedReader(ctx, query, inspect.ReadLimits{
		MaxEvents: 100000, MaxPayloadBytes: 32 << 20, MaxArtifactBytes: 32 << 20,
	})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return newServer(reader), nil
}

func newServer(reader observationReader) *Server {
	s := &Server{reader: reader, slots: make(chan struct{}, MaxConcurrent), timeout: RequestTimeout}
	s.sdk = server.NewMCPServer("hufu-readonly", "1", server.WithToolCapabilities(false))
	for _, definition := range []struct {
		name string
		kind inspect.Kind
	}{
		{"hufu_run_status", inspect.KindRun},
		{"hufu_run_routes", inspect.KindRoutes},
		{"hufu_run_economics", inspect.KindEconomics},
		{"hufu_capabilities", ""},
	} {
		options := []mcp.ToolOption{mcp.WithSchemaAdditionalProperties(false), mcp.WithReadOnlyHintAnnotation(true)}
		if definition.kind == inspect.KindRoutes {
			options = append(options, mcp.WithString("task_id", mcp.Description("Optional opaque task ID within the fixed scope.")))
		}
		tool := mcp.NewTool(definition.name, options...)
		s.tools = append(s.tools, tool)
		s.sdk.AddTool(tool, s.handler(definition.kind))
	}
	return s
}

func (s *Server) handler(kind inspect.Kind) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (result *mcp.CallToolResult, err error) {
		defer func() {
			if recover() != nil {
				result, err = failure("internal_error"), nil
			}
		}()
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
		default:
			return failure("resource_limit"), nil
		}
		ctx, cancel := context.WithTimeout(ctx, s.timeout)
		defer cancel()
		taskID, err := arguments(request, kind == inspect.KindRoutes)
		if err != nil {
			return failure("invalid_argument"), nil
		}
		var value any
		if kind == "" {
			value = struct {
				SchemaVersion int        `json:"schema_version"`
				Scope         string     `json:"scope"`
				Tools         []mcp.Tool `json:"tools"`
			}{1, "fixed_workspace_session_branch_run", s.tools}
		} else {
			value, err = s.reader.Inspect(ctx, kind, taskID)
		}
		if ctx.Err() != nil {
			return failure("cancelled"), nil
		}
		if err != nil {
			return failure(ErrorCode(err)), nil
		}
		var output limitedBuffer
		if err := json.MarshalWrite(&output, value); err != nil {
			return failure(ErrorCode(err)), nil
		}
		if ctx.Err() != nil {
			return failure("cancelled"), nil
		}
		return mcp.NewToolResultText(output.String()), nil
	}
}

func arguments(request mcp.CallToolRequest, routes bool) (string, error) {
	raw := request.Params.RawArguments
	if len(raw) == 0 {
		if request.Params.Arguments == nil {
			raw = []byte("{}")
		} else {
			var err error
			raw, err = json.Marshal(request.Params.Arguments)
			if err != nil {
				return "", err
			}
		}
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return "", inspect.ErrInvalidQuery
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", err
	}
	taskID := ""
	for name, value := range fields {
		if !routes || name != "task_id" || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return "", inspect.ErrInvalidQuery
		}
		if err := json.Unmarshal(value, &taskID); err != nil || taskID == "" {
			return "", inspect.ErrInvalidQuery
		}
	}
	return taskID, nil
}

func failure(code string) *mcp.CallToolResult {
	// Only a fixed vocabulary is returned; never raw errors, paths or payloads.
	return mcp.NewToolResultError(`{"code":"` + code + `"}`)
}

func ErrorCode(err error) string {
	switch {
	case errors.Is(err, team.ErrReadLimit):
		return "resource_limit"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	case errors.Is(err, inspect.ErrScopeConflict), errors.Is(err, inspect.ErrAmbiguous):
		return "scope_conflict"
	case errors.Is(err, inspect.ErrNotFound):
		return "not_found"
	case errors.Is(err, inspect.ErrInvalidQuery), errors.Is(err, inspect.ErrUnauthorized):
		return "invalid_argument"
	case errors.Is(err, inspect.ErrIntegrity):
		return "integrity_error"
	default:
		return "internal_error"
	}
}

type limitedBuffer struct{ buffer bytes.Buffer }

func (b *limitedBuffer) String() string { return b.buffer.String() }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(p) > MaxResponseBytes-b.buffer.Len() {
		return 0, team.ErrReadLimit
	}
	return b.buffer.Write(p)
}
