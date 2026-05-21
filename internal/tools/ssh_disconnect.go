//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"charm.land/fantasy"
)

type sshDisconnectArgs struct {
	Host string `json:"host"`
}

func NewSSHDisconnectTool(opts ...ToolOption) fantasy.AgentTool {
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "ssh_disconnect",
			Description: "Close an active SSH session and clear cached credentials. Use this when finished with a remote host to free resources and clear cached passwords.",
			Parameters: map[string]any{
				"host": map[string]any{
					"type":        "string",
					"description": "Hostname to disconnect from (e.g., 'server.example.com'). Must match the exact host used in previous ssh tool calls.",
				},
			},
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			var args sshDisconnectArgs
			if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
				return fantasy.NewTextErrorResponse("failed to parse args: " + err.Error()), nil
			}

			sessionMgr := GetSSHSessionManager(ctx)
			if sessionMgr == nil {
				return fantasy.NewTextResponse("No active SSH sessions"), nil
			}

			// Clear password first
			sessionMgr.ClearPassword(args.Host)

			// Close session
			if sessionMgr.Close(args.Host) {
				return fantasy.NewTextResponse(fmt.Sprintf("Disconnected from %s and cleared cached credentials", args.Host)), nil
			}

			return fantasy.NewTextResponse(fmt.Sprintf("No active session for %s", args.Host)), nil
		},
	}
}
