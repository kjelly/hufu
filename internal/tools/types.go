package tools

import (
	"context"
	"sync"
)

var StdinMu sync.Mutex

type agentToolsAllowedKeyType struct{}

var AgentToolsAllowedKey = agentToolsAllowedKeyType{}

type agentToolsSessionPermissionsKeyType struct{}

var AgentToolsSessionPermissionsKey = agentToolsSessionPermissionsKeyType{}

type toolPermissionCallbackKeyType struct{}

var ToolPermissionCallbackKey = toolPermissionCallbackKeyType{}

type ToolPermissionCallback func(toolName string, allowed bool)

func SetToolsAllowed(ctx context.Context, allowed []string) context.Context {
	return context.WithValue(ctx, AgentToolsAllowedKey, allowed)
}

func GetToolsAllowed(ctx context.Context) []string {
	if v, ok := ctx.Value(AgentToolsAllowedKey).([]string); ok {
		return v
	}
	return nil
}

type agentNameKeyType struct{}

var AgentNameKey = agentNameKeyType{}

type guardRulesKeyType struct{}

var GuardRulesKey = guardRulesKeyType{}

type agentAllowedPathsKeyType struct{}

var AgentAllowedPathsKey = agentAllowedPathsKeyType{}

type agentRestrictedPathKeyType struct{}

var AgentRestrictedPathKey = agentRestrictedPathKeyType{}

type agentNetworkBlockKeyType struct{}

var AgentNetworkBlockKey = agentNetworkBlockKeyType{}

type agentForceMCPKeyType struct{}

var AgentForceMCPKey = agentForceMCPKeyType{}

type AskUserTUIOption struct {
	Label string
	Value string
}

type GuardReviewFn func(ctx context.Context, toolName string, args string, rules []string) (approved bool, reason string, err error)

