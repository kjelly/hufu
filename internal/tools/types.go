package tools

import (
	"context"
	"sync"
	"time"
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

// SSH session context key
type sshSessionKey struct{}

var SSHSessionKey = sshSessionKey{}

// SSHSession represents an active SSH connection context
type SSHSession struct {
	Host      string    // Remote host in [user@]hostname format
	User      string    // Username (extracted from host)
	Port      int       // SSH port (default 22)
	TaskID    string    // Task ID where this session was created
	CreatedAt time.Time // Session creation timestamp
}

type AskUserTUIOption struct {
	Label string
	Value string
}

type GuardReviewFn func(ctx context.Context, toolName string, args string, rules []string) (approved bool, reason string, err error)

