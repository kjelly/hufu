//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// Tool risk levels for access control
const (
	ToolLevelLow    = "low"
	ToolLevelMedium = "medium"
	ToolLevelHigh   = "high"
)

// High-risk tools that require explicit allow in config
var highRiskTools = map[string]bool{
	"golang":         true,
	"lua":            true,
	"javascript":     true,
	"bash":           true,
	"mcp":            true,
	"terminal":       true,
	"terminal_start": true,
}

// Medium-risk tools that require user confirmation
var mediumRiskTools = map[string]bool{
	"download":      true,
	"fetch":         true,
	"agentic_fetch": true,
	"web_search":    true,
	"web_fetch":     true,
}

// ForceMCPBlockedTools are disabled when --force-mcp is enabled, forcing use of MCP servers
var ForceMCPBlockedTools = map[string]bool{
	"bash":               true,
	"sudo":               true,
	"ssh":                true,
	"scp":                true,
	"ssh_disconnect":     true,
	"golang":             true,
	"lua":                true,
	"javascript":         true,
	"download":           true,
	"fetch":              true,
	"agentic_fetch":      true,
	"web_search":         true,
	"web_fetch":          true,
	"terminal":           true,
	"terminal_start":     true,
	"terminal_write":     true,
	"terminal_read":      true,
	"terminal_close":     true,
	"terminal_list":      true,
	"terminal_reconcile": true,
}

// GetToolLevel returns the risk level of a tool
func GetToolLevel(toolName string) string {
	if highRiskTools[toolName] {
		return ToolLevelHigh
	}
	if mediumRiskTools[toolName] {
		return ToolLevelMedium
	}
	return ToolLevelLow
}

// CheckToolPermission checks if a tool is allowed to be used.
// Returns (allowed, askUser, error)
func CheckToolPermission(ctx context.Context, toolName string) (bool, bool, error) {
	allowed, askUser, _, err := CheckToolPermissionDetail(ctx, toolName)
	return allowed, askUser, err
}

// CheckToolPermissionDetail is CheckToolPermission with a denial reason.
// The reason distinguishes the deny branches, which otherwise all surface to
// the agent as the same generic message and make denials undiagnosable.
func CheckToolPermissionDetail(ctx context.Context, toolName string) (bool, bool, string, error) {
	// Special case: ask_user is always allowed (used for clarification)
	if toolName == "ask_user" {
		return true, false, "", nil
	}

	// 0. ForceMCP mode: deny blocked tools immediately (defense in depth)
	if fm, ok := ctx.Value(AgentForceMCPKey).(bool); ok && fm {
		if ForceMCPBlockedTools[toolName] {
			return false, false, "", fmt.Errorf("tool '%s' is blocked by --force-mcp. Use an MCP server instead", toolName)
		}
	}

	// 1. In CI/non-interactive environments, never ask the user — deny
	// all tools except ask_user. Exception: in unattended mode the allowlist is
	// trusted to run without a human, so fall through to the allowlist check
	// (step 3) instead of blanket-denying — otherwise no tool could ever run.
	if !IsUnattended(ctx) && !processUnattended.Load() && !IsInteractiveEnvironment() {
		return false, false, "non-interactive environment: stdin is not a terminal and unattended mode is off", nil
	}

	// 2. Check permanent session-level permissions first
	if sessionPerms, ok := ctx.Value(AgentToolsSessionPermissionsKey).(map[string]bool); ok {
		if allowed, decided := sessionPerms[toolName]; decided {
			if allowed {
				return true, false, "", nil
			}
			return false, false, "denied earlier in this session (permanent session-level permission)", nil
		}
	}

	// 3. Check explicitly allowed tools from team.yaml (context)
	val := ctx.Value(AgentToolsAllowedKey)
	if val == nil {
		// Not configured: deny all tools except ask_user
		return false, false, "no tools allowlist is configured for this agent", nil
	}

	allowed, _ := val.([]string)
	allowedMap := make(map[string]bool, len(allowed))
	for _, t := range allowed {
		allowedMap[t] = true
	}

	// Check if tool is in allowlist
	if !allowedMap[toolName] {
		return false, false, "not in the tools allowlist for this agent", nil
	}

	// Tool is in allowlist - allow it
	return true, false, "", nil
}

// formatDenialError builds the error string returned to the LLM when the
// user denies a tool permission. When reason is empty, the result is
// byte-identical to the pre-feature format so existing agents see no change.
// When reason is non-empty, it is trimmed of surrounding whitespace, its
// first non-empty line is used, and the result is appended to the standard
// denial prefix.
func formatDenialError(toolName, reason string) string {
	cleaned := strings.TrimSpace(reason)
	if cleaned == "" {
		return fmt.Sprintf("user denied permission for tool '%s'", toolName)
	}
	// Take the first non-empty line (skip leading blank lines so that
	// padding or stray newlines from the user don't silently drop their
	// intent). Each line is also stripped of a trailing CR for Windows
	// line endings.
	for _, line := range strings.Split(cleaned, "\n") {
		if trimmed := strings.TrimSpace(strings.TrimRight(line, "\r")); trimmed != "" {
			return fmt.Sprintf("user denied permission for tool '%s'. Reason: %s", toolName, trimmed)
		}
	}
	return fmt.Sprintf("user denied permission for tool '%s'", toolName)
}

// promptDenialReason reads an optional one-line free-text reason from the
// user after they have denied a tool permission. The reason is returned to
// the LLM as part of the tool-error result. It is optional: an empty input
// (or a cancelled context) yields an empty string and the caller falls back
// to the standard "user denied" error string with no Reason suffix.
//
// The function is safe to call in TUI mode: it invokes NotifyAskUserStart
// to release the TUI altscreen (a no-op in CLI mode) and NotifyAskUserDone
// to restore it. It also serializes on StdinMu so it does not race with
// ask_user or the promptInjector.
func promptDenialReason(ctx context.Context) string {
	StdinMu.Lock()
	defer StdinMu.Unlock()

	SetAskUserActive(true)
	defer SetAskUserActive(false)

	// NotifyAskUserStart is called *after* the lock and active flag so
	// that the deferred SetAskUserActive(false) → NotifyAskUserDone pair
	// is still balanced even on early return paths.
	NotifyAskUserStart()

	// Re-check the context after acquiring StdinMu: another goroutine may
	// have been waiting for the lock while the user (or the system)
	// cancelled the operation.
	if err := ctx.Err(); err != nil {
		return ""
	}

	reader := denialReasonStdin()
	_, _ = fmt.Fprint(denialReasonStderr, "Reason (optional, enter to skip): ")

	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return ""
	}
	// If the user pressed Ctrl-D (EOF) without typing, line will be "".
	// If they typed text without a trailing newline, we still get the text.
	// Trim trailing CR/LF.
	line = strings.TrimRight(line, "\r\n")
	return strings.TrimSpace(line)
}
