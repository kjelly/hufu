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

// AutoApproveKey carries whether ask_user should auto-select clearly safe
// options when they exist.
type autoApproveKeyType struct{}

var AutoApproveKey = autoApproveKeyType{}

// UnattendedKey carries whether the run is unattended (no human available).
// When true, ask_user returns a safe default instead of blocking on stdin and
// tool permission falls back to deny-by-default for non-allowlisted tools.
type unattendedKeyType struct{}

var UnattendedKey = unattendedKeyType{}

type taskIDKeyType struct{}

var TaskIDKey = taskIDKeyType{}

// SSH session context key
type sshSessionKey struct{}

var SSHSessionKey = sshSessionKey{}

// SSHSession represents an active SSH connection context
type SSHSession struct {
	Host           string    // Remote host in [user@]hostname format
	User           string    // Username (extracted from host)
	Port           int       // SSH port (default 22)
	TaskID         string    // Task ID where this session was created
	CreatedAt      time.Time // Session creation timestamp
	LastUsedAt     time.Time // Last activity timestamp (for idle timeout)
	Password       string    // Cached password (if provided via ask_user)
	PasswordExpiry time.Time // Password expiration timestamp
}

type AskUserTUIOption struct {
	Label string
	Value string
}

// AskUserResponse is the normalized response shape used by ask_user in
// interactive, TUI, and unattended modes.
type AskUserResponse struct {
	Answers []string `json:"answers"`
	Free    string   `json:"free_text,omitempty"`
}

// AskUserChoiceSelector chooses an unattended answer for ask_user when the
// prompt includes options. It should return a normalized response.
type AskUserChoiceSelector func(ctx context.Context, question, qtype string, opts []AskUserTUIOption, allowAny bool) (AskUserResponse, error)

type askUserChoiceSelectorKeyType struct{}

var AskUserChoiceSelectorKey = askUserChoiceSelectorKeyType{}

type GuardReviewFn func(ctx context.Context, toolName string, args string, rules []string) (approved bool, reason string, err error)

type pathReviewerKeyType struct{}

var PathReviewerKey = pathReviewerKeyType{}

func GetPathReviewerFromContext(ctx context.Context) PathReviewer {
	if v, ok := ctx.Value(PathReviewerKey).(PathReviewer); ok {
		return v
	}
	return nil
}
