//go:build linux || darwin
// +build linux darwin

package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/audit"
	"github.com/anomalyco/hufu/internal/hooks"
)

var askUserActive atomic.Int32
var interactiveAbortRequested atomic.Bool
var closeStdinOnce sync.Once

// denialReasonStdin is the reader used by promptDenialReason. It is a
// package-level variable so tests can inject a fake stdin.
var denialReasonStdin = func() *bufio.Reader { return bufio.NewReader(os.Stdin) }

// denialReasonStderr is the writer used by promptDenialReason. Tests may
// redirect this to capture output. Must be an unbuffered writer (os.Stderr
// or os.Pipe); a bufio.Writer would defeat capture-by-redirection since
// the buffered bytes would not be flushed before the test reads the pipe.
var denialReasonStderr = os.Stderr

type auditLoggerKeyType struct{}

var AuditLoggerKey = auditLoggerKeyType{}

var onAskUserStart func()
var onAskUserDone func()

func SetAuditLogger(ctx context.Context, logger *audit.AuditLogger) context.Context {
	return context.WithValue(ctx, AuditLoggerKey, logger)
}

func GetAuditLogger(ctx context.Context) *audit.AuditLogger {
	if v, ok := ctx.Value(AuditLoggerKey).(*audit.AuditLogger); ok {
		return v
	}
	return nil
}

func SetOnAskUserStart(fn func()) {
	onAskUserStart = fn
}

func SetOnAskUserDone(fn func()) {
	onAskUserDone = fn
}

// NotifyAskUserStart is called by the ask_user tool just before reading stdin.
// It invokes the registered start hook (e.g. to release the terminal in TUI mode).
func NotifyAskUserStart() {
	if onAskUserStart != nil {
		onAskUserStart()
	}
}

// NotifyAskUserDone is called by the ask_user tool after reading stdin.
// It invokes the registered done hook (e.g. to restore the terminal in TUI mode).
func NotifyAskUserDone() {
	if onAskUserDone != nil {
		onAskUserDone()
	}
}

func SetAskUserActive(active bool) {
	if active {
		askUserActive.Store(1)
		interactiveWaitStartNs.CompareAndSwap(0, time.Now().UnixNano())
	} else {
		askUserActive.Store(0)
		if started := interactiveWaitStartNs.Swap(0); started != 0 {
			interactiveWaitTotalNs.Add(time.Now().UnixNano() - started)
		}
		NotifyAskUserDone()
	}
}

func IsAskUserActive() bool {
	return askUserActive.Load() == 1
}

// interactiveWaitTotalNs accumulates time this process has spent blocked on
// interactive prompts (ask_user, path consent). interactiveWaitStartNs is
// the start of the in-flight prompt, or 0 when none is active; prompts are
// serialized on StdinMu so at most one runs at a time.
var (
	interactiveWaitTotalNs atomic.Int64
	interactiveWaitStartNs atomic.Int64
)

// InteractiveWaitTotal returns the cumulative time spent waiting on
// interactive prompts, including the currently active one. Task deadlines
// take the delta of this value so human response time does not count
// against an agent's time budget (see WithInteractiveAwareTimeout).
func InteractiveWaitTotal() time.Duration {
	total := interactiveWaitTotalNs.Load()
	if started := interactiveWaitStartNs.Load(); started != 0 {
		total += time.Now().UnixNano() - started
	}
	return time.Duration(total)
}

// RequestInteractiveAbort marks interactive input as aborted and closes stdin
// once so any in-flight prompt/consent read returns immediately.
func RequestInteractiveAbort() {
	interactiveAbortRequested.Store(true)
	closeStdinOnce.Do(func() {
		_ = os.Stdin.Close()
	})
}

func IsInteractiveAbortRequested() bool {
	return interactiveAbortRequested.Load()
}

// AskUserTUIOption is a choice for the ask_user TUI dialog.
// Mirrors tui.AskUserOption without a cross-package dependency.
var onAskUserTUI func(ctx context.Context, question, qtype string, opts []AskUserTUIOption, allowAny bool) (string, bool)

func SetOnAskUserTUI(fn func(ctx context.Context, question, qtype string, opts []AskUserTUIOption, allowAny bool) (string, bool)) {
	onAskUserTUI = fn
}

// TryAskUserTUI attempts to handle ask_user via a TUI-native dialog.
// Returns (jsonResp, true) if handled; ("", false) if TUI is not active or ctx is cancelled.
func TryAskUserTUI(ctx context.Context, question, qtype string, opts []AskUserTUIOption, allowAny bool) (string, bool) {
	if onAskUserTUI == nil {
		return "", false
	}
	return onAskUserTUI(ctx, question, qtype, opts, allowAny)
}

// IsUnattended reports whether the context marks the run as unattended
// (no human available to answer prompts or grant permissions).
func IsUnattended(ctx context.Context) bool {
	v, _ := ctx.Value(UnattendedKey).(bool)
	return v
}

// IsAutoApprove reports whether ask_user should auto-select clearly safe
// options instead of prompting the user when possible.
func IsAutoApprove(ctx context.Context) bool {
	v, _ := ctx.Value(AutoApproveKey).(bool)
	return v
}

var interactiveEnvPinned atomic.Value // bool, set once at startup

// CaptureInteractiveEnvironment pins the interactivity decision for the rest
// of the process. Call it once at startup: stdin can stop looking like a
// terminal mid-run (TUI widgets or prompts taking over the fd), and a
// per-call check would silently flip every permission decision from allow to
// deny partway through a session.
func CaptureInteractiveEnvironment() {
	interactiveEnvPinned.Store(detectInteractiveEnvironment())
}

// IsInteractiveEnvironment reports whether stdin is a terminal and the process
// is not running in a known CI environment. Uses the value pinned by
// CaptureInteractiveEnvironment when available and probes live otherwise
// (tests swap os.Stdin and rely on the live probe).
func IsInteractiveEnvironment() bool {
	if v, ok := interactiveEnvPinned.Load().(bool); ok {
		return v
	}
	return detectInteractiveEnvironment()
}

func detectInteractiveEnvironment() bool {
	for _, v := range CIEnvVars {
		if os.Getenv(v) != "" {
			return false
		}
	}
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

var onNeedsHuman func(question string)

// SetOnNeedsHuman registers a hook invoked when an agent requests human input
// but none is available (unattended / non-interactive). Used to push a
// notification so an operator can follow up out-of-band.
func SetOnNeedsHuman(fn func(question string)) { onNeedsHuman = fn }

// NotifyNeedsHuman fires the needs-human hook, if registered.
func NotifyNeedsHuman(question string) {
	if onNeedsHuman != nil {
		onNeedsHuman(question)
	}
}

// normalizeWorkspacePath rewrites a path that uses the workspace directory name
// as if it were at the filesystem root (e.g. /workspace/x) into a relative path
// (./workspace/x) so it resolves correctly under the workDir.
func normalizeWorkspacePath(path, workspaceName string) string {
	if workspaceName == "" || path == "" {
		return path
	}
	prefix := "/" + workspaceName
	if path == prefix || strings.HasPrefix(path, prefix+"/") {
		return "." + path
	}
	return path
}

// Tool risk levels for access control
const (
	ToolLevelLow    = "low"
	ToolLevelMedium = "medium"
	ToolLevelHigh   = "high"
)

// High-risk tools that require explicit allow in config
var highRiskTools = map[string]bool{
	"golang": true,
	"lua":    true,
	"bash":   true,
	"mcp":    true,
}

// Medium-risk tools that require user confirmation
var mediumRiskTools = map[string]bool{
	"download":      true,
	"fetch":         true,
	"agentic_fetch": true,
}

// ForceMCPBlockedTools are disabled when --force-mcp is enabled, forcing use of MCP servers
var ForceMCPBlockedTools = map[string]bool{
	"bash":           true,
	"sudo":           true,
	"ssh":            true,
	"scp":            true,
	"ssh_disconnect": true,
	"golang":         true,
	"lua":            true,
	"download":       true,
	"fetch":          true,
	"agentic_fetch":  true,
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
	if !IsUnattended(ctx) && !IsInteractiveEnvironment() {
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
	fmt.Fprint(denialReasonStderr, "Reason (optional, enter to skip): ")

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

var CIEnvVars = []string{
	"CI",
	"CI_SERVER",
	"GITHUB_ACTIONS",
	"GITLAB_CI",
	"CIRCLECI",
	"TRAVIS",
	"JENKINS_URL",
	"TF_BUILD",
	"APPVEYOR",
	"BUILDKITE",
	"DRONE",
}

func mergedAllowedPaths(cfg ToolConfig, ctx context.Context) []string {
	paths := cfg.AllowedPaths
	if extra, _ := ctx.Value(AgentAllowedPathsKey).([]string); len(extra) > 0 {
		seen := make(map[string]bool, len(paths))
		for _, p := range paths {
			seen[p] = true
		}
		for _, p := range extra {
			if !seen[p] {
				paths = append(paths, p)
				seen[p] = true
			}
		}
	}
	return paths
}

func cfgWithMergedPaths(cfg ToolConfig, ctx context.Context) ToolConfig {
	needMerge := false
	if _, ok := ctx.Value(AgentAllowedPathsKey).([]string); ok {
		needMerge = true
	}
	if rp, ok := ctx.Value(AgentRestrictedPathKey).(string); ok && rp != "" {
		needMerge = true
	}
	if nb, ok := ctx.Value(AgentNetworkBlockKey).(bool); ok && nb {
		needMerge = true
	}
	if fm, ok := ctx.Value(AgentForceMCPKey).(bool); ok && fm {
		needMerge = true
	}
	if !needMerge {
		return cfg
	}
	merged := ToolConfig{
		WorkDir:        cfg.WorkDir,
		AllowedPaths:   mergedAllowedPaths(cfg, ctx),
		PathConsent:    cfg.PathConsent,
		ToolName:       cfg.ToolName,
		WorkspaceName:  cfg.WorkspaceName,
		Hooks:          cfg.Hooks,
		RestrictedBash: cfg.RestrictedBash,
		RestrictedPath: mergedRestrictedPath(cfg, ctx),
		NetworkBlock:   mergedNetworkBlock(cfg, ctx),
		ForceMCP:       mergedForceMCP(cfg, ctx),
	}
	return merged
}

func mergedRestrictedPath(cfg ToolConfig, ctx context.Context) string {
	if rp, ok := ctx.Value(AgentRestrictedPathKey).(string); ok && rp != "" {
		return rp
	}
	return cfg.RestrictedPath
}

func mergedNetworkBlock(cfg ToolConfig, ctx context.Context) bool {
	if cfg.NetworkBlock {
		return true
	}
	if nb, ok := ctx.Value(AgentNetworkBlockKey).(bool); ok && nb {
		return true
	}
	return false
}

func mergedForceMCP(cfg ToolConfig, ctx context.Context) bool {
	if cfg.ForceMCP {
		return true
	}
	if fm, ok := ctx.Value(AgentForceMCPKey).(bool); ok && fm {
		return true
	}
	return false
}

type coreTool struct {
	info          fantasy.ToolInfo
	handler       func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error)
	pOpts         fantasy.ProviderOptions
	hooks         *hooks.HookRegistry
	guardReviewer GuardReviewFn
	pathReviewer  PathReviewer
}

func (t *coreTool) Info() fantasy.ToolInfo                          { return t.info }
func (t *coreTool) ProviderOptions() fantasy.ProviderOptions        { return t.pOpts }
func (t *coreTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.pOpts = opts }

func SetGuardReviewer(tools []fantasy.AgentTool, fn GuardReviewFn) {
	for _, t := range tools {
		if ct, ok := t.(*coreTool); ok {
			ct.guardReviewer = fn
		}
	}
}

func SetPathReviewer(tools []fantasy.AgentTool, pr PathReviewer) {
	for _, t := range tools {
		if ct, ok := t.(*coreTool); ok {
			ct.pathReviewer = pr
		}
	}
}

func extractHookContext(ctx context.Context) hooks.HookContext {
	teamName, _ := ctx.Value(hooks.TeamNameKey).(string)
	agentName, _ := ctx.Value(hooks.AgentNameKey).(string)
	if agentName == "" {
		agentName, _ = ctx.Value(AgentNameKey).(string)
	}
	taskDesc, _ := ctx.Value(hooks.TaskDescKey).(string)
	return hooks.MakeContext(teamName, agentName, "", taskDesc, "", "")
}

func (t *coreTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if t.pathReviewer != nil {
		ctx = context.WithValue(ctx, PathReviewerKey, t.pathReviewer)
	}

	if err := validateToolInput(call.Input, t.info); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	if t.hooks != nil && t.hooks.HasHooks("before_tool_call") {
		payload := hooks.HookPayload{
			HookPoint: "before_tool_call",
			Context:   extractHookContext(ctx),
			ToolName:  t.info.Name,
			Args:      json.RawMessage(call.Input),
		}
		resp := t.hooks.Dispatch(ctx, "before_tool_call", payload)
		switch resp.Result {
		case hooks.HookSkip:
			return fantasy.NewTextResponse("tool call skipped by hook"), nil
		case hooks.HookError:
			return fantasy.NewTextErrorResponse(resp.ErrorMessage), nil
		case hooks.HookReplace:
			if resp.Replacement != nil {
				call.Input = string(resp.Replacement)
			}
		}
	}

	// Check tool permission (after hooks, so hooks can influence tool name/args)
	allowed, askUser, denyReason, err := CheckToolPermissionDetail(ctx, t.info.Name)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("tool permission check failed: %v", err)), nil
	}
	if !allowed {
		if askUser {
			// Request user confirmation for medium-risk tools
			agentName, _ := ctx.Value(AgentNameKey).(string)
			question := fmt.Sprintf("Agent '%s' wants to use tool '%s'. Allow?", agentName, t.info.Name)

			// Try TUI first
			if jsonResp, ok := TryAskUserTUI(ctx, question, "single_choice", []AskUserTUIOption{
				{Label: "Yes", Value: "y"},
				{Label: "No", Value: "n"},
				{Label: "Always Allow", Value: "ay"},
				{Label: "Always Deny", Value: "an"},
			}, false); ok {
				var askResp askResponseType
				if err := json.Unmarshal([]byte(jsonResp), &askResp); err == nil && len(askResp.Answers) > 0 {
					ans := askResp.Answers[0]
					switch ans {
					case "y", "ay":
						allowed = true
						if ans == "ay" {
							if cb, ok := ctx.Value(ToolPermissionCallbackKey).(ToolPermissionCallback); ok {
								cb(t.info.Name, true)
							}
						}
					case "an":
						if cb, ok := ctx.Value(ToolPermissionCallbackKey).(ToolPermissionCallback); ok {
							cb(t.info.Name, false)
						}
					}
				}
			} else {
				// CLI fallback
				StdinMu.Lock()
				defer StdinMu.Unlock()
				fmt.Fprintf(os.Stderr, "\n%s %s\n", boldFmt("PERMISSION:"), question)
				fmt.Fprintf(os.Stderr, "  (y) Yes  (n) No  (ay) Always Yes  (an) Always No\n")
				fmt.Fprintf(os.Stderr, "  Choice [n]: ")
				reader := bufio.NewReader(os.Stdin)
				input, _ := reader.ReadString('\n')
				choice := strings.ToLower(strings.TrimSpace(input))
				switch choice {
				case "y", "ay":
					allowed = true
					if choice == "ay" {
						if cb, ok := ctx.Value(ToolPermissionCallbackKey).(ToolPermissionCallback); ok {
							cb(t.info.Name, true)
						}
					}
				case "an":
					if cb, ok := ctx.Value(ToolPermissionCallbackKey).(ToolPermissionCallback); ok {
						cb(t.info.Name, false)
					}
				}
			}

			if !allowed {
				reason := promptDenialReason(ctx)
				return fantasy.NewTextErrorResponse(formatDenialError(t.info.Name, reason)), nil
			}
		} else {
			msg := fmt.Sprintf("tool '%s' is not permitted (%s).", t.info.Name, denyReason)
			if strings.Contains(denyReason, "allowlist") {
				msg += fmt.Sprintf(" Add '%s' to tools.allowed in team.yaml to enable.", t.info.Name)
			}
			return fantasy.NewTextErrorResponse(msg), nil
		}
	}

	if t.guardReviewer != nil {
		if rules, _ := ctx.Value(GuardRulesKey).([]string); len(rules) > 0 {
			approved, reason, reviewErr := t.guardReviewer(ctx, t.info.Name, call.Input, rules)
			if reviewErr != nil {
				// Fail closed: when the guard reviewer cannot complete (timeout,
				// model error, etc.) we must not silently allow a call that was
				// supposed to be reviewed. Deny it so the agent retries or finds
				// an allowed alternative, rather than bypassing the guard.
				fmt.Fprintf(os.Stderr, "warning: guard review failed, denying tool call: %v\n", reviewErr)
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Guard review unavailable (%v); tool call denied. Retry or use a different approach that does not require review.", reviewErr)), nil
			} else if !approved {
				msg := fmt.Sprintf("Guard rule violation: %s", reason)
				return fantasy.NewTextErrorResponse(msg), nil
			}
		}
	}

	toolResp, err := t.handler(ctx, call)

	if t.hooks != nil && t.hooks.HasHooks("after_tool_call") {
		resultText := toolResp.Content
		isErr := toolResp.IsError
		if err != nil {
			resultText = err.Error()
			isErr = true
		}
		payload := hooks.HookPayload{
			HookPoint: "after_tool_call",
			Context:   extractHookContext(ctx),
			ToolName:  t.info.Name,
			Args:      json.RawMessage(call.Input),
			Result:    resultText,
			IsError:   isErr,
		}
		resp := t.hooks.Dispatch(ctx, "after_tool_call", payload)
		switch resp.Result {
		case hooks.HookReplace:
			if resp.Replacement != nil {
				if toolResp.IsError {
					toolResp = fantasy.NewTextErrorResponse(string(resp.Replacement))
				} else {
					toolResp = fantasy.NewTextResponse(string(resp.Replacement))
				}
			}
		case hooks.HookError:
			return fantasy.NewTextErrorResponse(resp.ErrorMessage), nil
		}
	}

	return toolResp, err
}

func parseArgs(input string, target any) error {
	if input == "" || input == "{}" {
		return nil
	}
	if err := json.Unmarshal([]byte(input), target); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func validateRequired(input string, required []string) error {
	if input == "" || input == "{}" {
		for _, field := range required {
			return fmt.Errorf("parameter %q is required", field)
		}
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(input), &raw); err != nil {
		return nil
	}
	for _, field := range required {
		val, ok := raw[field]
		if !ok || val == nil {
			return fmt.Errorf("parameter %q is required", field)
		}
		if s, ok := val.(string); ok && s == "" {
			return fmt.Errorf("parameter %q must not be empty", field)
		}
	}
	return nil
}

func validateParamType(input string, paramName string, expectedType string) error {
	if input == "" || input == "{}" {
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(input), &raw); err != nil {
		return nil
	}
	val, ok := raw[paramName]
	if !ok || val == nil {
		return nil
	}
	switch expectedType {
	case "string":
		if _, ok := val.(string); !ok {
			return fmt.Errorf("parameter %q must be a string", paramName)
		}
	case "number":
		if _, ok := val.(float64); !ok {
			if _, ok := val.(int); !ok {
				return fmt.Errorf("parameter %q must be a number", paramName)
			}
		}
	case "boolean":
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("parameter %q must be a boolean", paramName)
		}
	case "array":
		if _, ok := val.([]any); !ok {
			return fmt.Errorf("parameter %q must be an array", paramName)
		}
	}
	return nil
}

func validateToolInput(input string, info fantasy.ToolInfo) error {
	if err := validateRequired(input, info.Required); err != nil {
		return err
	}
	if input == "" || input == "{}" {
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(input), &raw); err != nil {
		return nil
	}
	for paramName, paramDef := range info.Parameters {
		defMap, ok := paramDef.(map[string]any)
		if !ok {
			continue
		}
		typeVal, ok := defMap["type"].(string)
		if !ok {
			continue
		}
		if _, exists := raw[paramName]; exists {
			if err := validateParamType(input, paramName, typeVal); err != nil {
				return err
			}
		}
	}
	return nil
}

func resolvePathWithWorkDir(path, workDir string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	baseDir := workDir
	if baseDir == "" {
		var err error
		baseDir, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("failed to get working directory: %w", err)
		}
	}
	return filepath.Clean(filepath.Join(baseDir, path)), nil
}

func resolveAndValidatePath(path, workDir string) (string, error) {
	absPath, err := resolvePathWithWorkDir(path, workDir)
	if err != nil {
		return "", err
	}
	projectDir := workDir
	if projectDir == "" {
		projectDir, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("failed to get working directory: %w", err)
		}
	}
	projectDir = filepath.Clean(projectDir)

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

	evaluatedDir, err := filepath.EvalSymlinks(filepath.Dir(absPath))
	if err != nil {
		return "", fmt.Errorf("path '%s' is invalid or cannot be resolved: %w", path, err)
	}

	if !strings.HasPrefix(evaluatedDir, evaluatedProjDir+string(filepath.Separator)) && evaluatedDir != evaluatedProjDir {
		return "", fmt.Errorf("path '%s' is outside the project directory", path)
	}
	return filepath.Join(evaluatedDir, filepath.Base(absPath)), nil
}

func isPathAllowed(absPath string, allowedPaths []string) bool {
	evalPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		evalPath = absPath
	}
	evalPath = filepath.Clean(evalPath)

	if cwd, err := os.Getwd(); err == nil && isPathWithinDir(evalPath, cwd) {
		return true
	}

	checkPath := evalPath

	for _, allowed := range allowedPaths {
		evalAllowed, err := filepath.EvalSymlinks(allowed)
		if err != nil {
			evalAllowed = allowed
		}
		evalAllowed = filepath.Clean(evalAllowed)

		if checkPath == evalAllowed || strings.HasPrefix(checkPath, evalAllowed+string(filepath.Separator)) {
			return true
		}

		parentDir := filepath.Dir(absPath)
		evalParent, err := filepath.EvalSymlinks(parentDir)
		if err == nil {
			evalParent = filepath.Clean(evalParent)
			if evalParent == evalAllowed || strings.HasPrefix(evalParent, evalAllowed+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

func isPathWithinDir(path, dir string) bool {
	if path == "" || dir == "" {
		return false
	}
	evalPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		evalPath = path
	}
	evalPath = filepath.Clean(evalPath)

	evalDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		evalDir = dir
	}
	evalDir = filepath.Clean(evalDir)

	if evalPath == evalDir {
		return true
	}
	rel, err := filepath.Rel(evalDir, evalPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func checkPathOrConsent(path, workDir, operation string, cfg ToolConfig) (string, error) {
	path = normalizeWorkspacePath(path, cfg.WorkspaceName)
	absPath, err := resolvePathWithWorkDir(path, workDir)
	if err != nil {
		return "", err
	}

	if isPathAllowed(absPath, cfg.AllowedPaths) {
		return absPath, nil
	}

	if cfg.PathConsent != nil {
		result, suggestion, err := cfg.PathConsent.AskConsent(absPath, operation, cfg.ToolName, path)
		if err != nil {
			return "", fmt.Errorf("path '%s' is outside allowed paths and consent failed: %w", path, err)
		}
		switch result {
		case ConsentOnce, ConsentAlways:
			evalPath, evalErr := filepath.EvalSymlinks(absPath)
			if evalErr == nil {
				evalPath = filepath.Clean(evalPath)
				if evalPath != absPath && !isPathAllowed(evalPath, cfg.AllowedPaths) {
					return "", fmt.Errorf("path '%s' resolves to '%s' which is outside allowed paths", path, evalPath)
				}
			}
			return absPath, nil
		default:
			return "", formatPathConsentDenied(path, suggestion)
		}
	}

	return "", fmt.Errorf("path '%s' is outside allowed paths", path)
}

func resolveAndValidatePathWithConsent(path string, cfg ToolConfig) (string, error) {
	path = normalizeWorkspacePath(path, cfg.WorkspaceName)
	absPath, err := resolvePathWithWorkDir(path, cfg.WorkDir)
	if err != nil {
		return "", err
	}

	if isPathAllowed(absPath, cfg.AllowedPaths) {
		// resolveAndValidatePath only knows about cfg.WorkDir (the project
		// directory) — calling it here silently discarded the AllowedPaths
		// check that just passed, rejecting any agent-level allowed-paths
		// entry outside the project dir (e.g. a /tmp scratch directory) with
		// a misleading "outside the project directory" from write/edit even
		// though bash/sudo happily use the same path. Validate against the
		// allowlist that actually matched, with the same symlink-aware logic.
		return resolveAndValidatePathForAllowedPath(absPath, cfg.WorkDir, cfg.AllowedPaths)
	}

	if cfg.PathConsent != nil {
		result, suggestion, err := cfg.PathConsent.AskConsent(absPath, "edit", cfg.ToolName, path)
		if err != nil {
			return "", fmt.Errorf("path '%s' is outside allowed paths and consent failed: %w", path, err)
		}
		switch result {
		case ConsentOnce, ConsentAlways:
			projectDir := cfg.WorkDir
			if projectDir == "" {
				projectDir, _ = os.Getwd()
			}
			// The consent applies to the directory that was approved. Add that
			// directory only for this validation pass so edit/write tools retain
			// their symlink-aware validation without rejecting a path the user
			// explicitly approved (including a persisted approval).
			consentedPaths := append([]string{}, cfg.AllowedPaths...)
			consentedPaths = append(consentedPaths, dirOfPath(absPath))
			return resolveAndValidatePathForAllowedPath(absPath, projectDir, consentedPaths)
		default:
			return "", formatPathConsentDenied(path, suggestion)
		}
	}

	return "", fmt.Errorf("path '%s' is outside allowed paths", path)
}

func formatPathConsentDenied(path, suggestion string) error {
	if suggestion == "" {
		return fmt.Errorf("path '%s' is outside allowed paths — access denied by user", path)
	}
	return fmt.Errorf("path '%s' is outside allowed paths; user suggested '%s', retry the command using that path instead", path, suggestion)
}

func resolveAndValidatePathForAllowedPath(absPath, projectDir string, allowedPaths []string) (string, error) {
	evaluatedAbsPath, err := filepath.EvalSymlinks(absPath)
	if err == nil {
		return validatedPathInAllowedDirs(evaluatedAbsPath, allowedPaths)
	}

	parentDir := filepath.Dir(absPath)
	evaluatedParent, err := filepath.EvalSymlinks(parentDir)
	if err != nil {
		return "", fmt.Errorf("path '%s' is invalid or cannot be resolved: %w", absPath, err)
	}

	result, err := validatedPathInAllowedDirs(evaluatedParent, allowedPaths)
	if err != nil {
		return "", err
	}
	return filepath.Join(result, filepath.Base(absPath)), nil
}

func validatedPathInAllowedDirs(evaluatedPath string, allowedPaths []string) (string, error) {
	evaluatedPath = filepath.Clean(evaluatedPath)
	for _, allowed := range allowedPaths {
		evalAllowed, err := filepath.EvalSymlinks(allowed)
		if err != nil {
			evalAllowed = allowed
		}
		evalAllowed = filepath.Clean(evalAllowed)

		if evaluatedPath == evalAllowed || strings.HasPrefix(evaluatedPath, evalAllowed+string(filepath.Separator)) {
			return evaluatedPath, nil
		}
	}
	return "", fmt.Errorf("path is outside allowed directories")
}

func AllTools(opts ...ToolOption) []fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	tools := []fantasy.AgentTool{
		NewBashTool(opts...),
		NewSudoTool(opts...),
		NewWaitForTool(opts...),
		NewSshTool(opts...),
		NewScpTool(opts...),
		NewSSHDisconnectTool(opts...),
		NewViewTool(opts...),
		NewWriteTool(opts...),
		NewEditTool(opts...),
		NewMultiEditTool(opts...),
		NewGrepTool(opts...),
		NewGlobTool(opts...),
		NewLsTool(opts...),
		NewLuaTool(opts...),
		NewGolangTool(opts...),
		NewAskUserTool(opts...),
		NewDownloadTool(opts...),
		NewFetchTool(opts...),
		NewAgenticFetchTool(opts...),
		NewRandomTool(opts...),
		NewMathTool(opts...),
		NewCreateSkillTool(opts...),
	}
	if cfg.NetworkBlock {
		netTools := map[string]bool{"fetch": true, "download": true, "agentic_fetch": true}
		filtered := make([]fantasy.AgentTool, 0, len(tools))
		for _, t := range tools {
			if !netTools[t.Info().Name] {
				filtered = append(filtered, t)
			}
		}
		tools = filtered
	}
	if cfg.ForceMCP {
		filtered := make([]fantasy.AgentTool, 0, len(tools))
		for _, t := range tools {
			if !ForceMCPBlockedTools[t.Info().Name] {
				filtered = append(filtered, t)
			}
		}
		tools = filtered
	}
	if cfg.Hooks != nil {
		for _, t := range tools {
			if ct, ok := t.(*coreTool); ok {
				ct.hooks = cfg.Hooks
			}
		}
	}
	return tools
}

func FilterTools(all []fantasy.AgentTool, allowed map[string]bool) []fantasy.AgentTool {
	if len(allowed) == 0 {
		return all
	}
	var filtered []fantasy.AgentTool
	for _, t := range all {
		if allowed[t.Info().Name] {
			filtered = append(filtered, t)
		}
	}
	return filtered
}
