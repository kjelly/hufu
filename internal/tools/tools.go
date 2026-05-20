//go:build linux || darwin
// +build linux darwin

package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/hooks"
)

var askUserActive atomic.Int32

var onAskUserStart func()
var onAskUserDone func()

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
	} else {
		askUserActive.Store(0)
		NotifyAskUserDone()
	}
}

func IsAskUserActive() bool {
	return askUserActive.Load() == 1
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
	"download":       true,
	"fetch":          true,
	"agentic_fetch":  true,
}

// ForceMCPBlockedTools are disabled when --force-mcp is enabled, forcing use of MCP servers
var ForceMCPBlockedTools = map[string]bool{
	"bash":          true,
	"sudo":          true,
	"ssh":           true,
	"golang":        true,
	"lua":           true,
	"download":      true,
	"fetch":         true,
	"agentic_fetch": true,
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
	// Special case: ask_user is always allowed (used for clarification)
	if toolName == "ask_user" {
		return true, false, nil
	}

	// 0. ForceMCP mode: deny blocked tools immediately (defense in depth)
	if fm, ok := ctx.Value(AgentForceMCPKey).(bool); ok && fm {
		if ForceMCPBlockedTools[toolName] {
			return false, false, fmt.Errorf("tool '%s' is blocked by --force-mcp. Use an MCP server instead", toolName)
		}
	}

	// 1. In CI/non-interactive environments, never ask the user — deny
	// all tools except ask_user.
	if !isInteractiveEnvironment() {
		return false, false, nil
	}

	// 2. Check permanent session-level permissions first
	if sessionPerms, ok := ctx.Value(AgentToolsSessionPermissionsKey).(map[string]bool); ok {
		if allowed, decided := sessionPerms[toolName]; decided {
			if allowed {
				return true, false, nil
			}
			return false, false, nil
		}
	}

	// 3. Check explicitly allowed tools from team.yaml (context)
	val := ctx.Value(AgentToolsAllowedKey)
	if val == nil {
		// Not configured: deny all tools except ask_user
		return false, false, nil
	}

	allowed, _ := val.([]string)
	allowedMap := make(map[string]bool, len(allowed))
	for _, t := range allowed {
		allowedMap[t] = true
	}

	// Check if tool is in allowlist
	if !allowedMap[toolName] {
		return false, false, nil
	}

	// Tool is in allowlist - allow it
	return true, false, nil
}

var ciEnvVars = []string{
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

func isInteractiveEnvironment() bool {
	for _, v := range ciEnvVars {
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
		WorkDir:         cfg.WorkDir,
		AllowedPaths:   mergedAllowedPaths(cfg, ctx),
		PathConsent:    cfg.PathConsent,
		ToolName:        cfg.ToolName,
		WorkspaceName:   cfg.WorkspaceName,
		Hooks:           cfg.Hooks,
		RestrictedBash:  cfg.RestrictedBash,
		RestrictedPath:  mergedRestrictedPath(cfg, ctx),
		NetworkBlock:    mergedNetworkBlock(cfg, ctx),
		ForceMCP:        mergedForceMCP(cfg, ctx),
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
	allowed, askUser, err := CheckToolPermission(ctx, t.info.Name)
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
					if ans == "y" || ans == "ay" {
						allowed = true
						if ans == "ay" {
							if cb, ok := ctx.Value(ToolPermissionCallbackKey).(ToolPermissionCallback); ok {
								cb(t.info.Name, true)
							}
						}
					} else if ans == "an" {
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
				if choice == "y" || choice == "ay" {
					allowed = true
					if choice == "ay" {
						if cb, ok := ctx.Value(ToolPermissionCallbackKey).(ToolPermissionCallback); ok {
							cb(t.info.Name, true)
						}
					}
				} else if choice == "an" {
					if cb, ok := ctx.Value(ToolPermissionCallbackKey).(ToolPermissionCallback); ok {
						cb(t.info.Name, false)
					}
				}
			}

			if !allowed {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("user denied permission for tool '%s'", t.info.Name)), nil
			}
		} else {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("tool '%s' is not permitted. Add '%s' to tools.allowed in team.yaml to enable.", t.info.Name, t.info.Name)), nil
		}
	}

	if t.guardReviewer != nil {
		if rules, _ := ctx.Value(GuardRulesKey).([]string); len(rules) > 0 {
			approved, reason, reviewErr := t.guardReviewer(ctx, t.info.Name, call.Input, rules)
			if reviewErr != nil {
				fmt.Fprintf(os.Stderr, "warning: guard review failed: %v\n", reviewErr)
				// fail open: allow tool call on reviewer error
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
		result, err := cfg.PathConsent.AskConsent(absPath, operation, cfg.ToolName, path)
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
			return "", fmt.Errorf("path '%s' is outside allowed paths — access denied by user", path)
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
		return resolveAndValidatePath(path, cfg.WorkDir)
	}

	if cfg.PathConsent != nil {
		result, err := cfg.PathConsent.AskConsent(absPath, "edit", cfg.ToolName, path)
		if err != nil {
			return "", fmt.Errorf("path '%s' is outside allowed paths and consent failed: %w", path, err)
		}
		switch result {
		case ConsentOnce, ConsentAlways:
			projectDir := cfg.WorkDir
			if projectDir == "" {
				projectDir, _ = os.Getwd()
			}
			return resolveAndValidatePathForAllowedPath(absPath, projectDir, cfg.AllowedPaths)
		default:
			return "", fmt.Errorf("path '%s' is outside allowed paths — access denied by user", path)
		}
	}

	return "", fmt.Errorf("path '%s' is outside allowed paths", path)
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
		NewSshTool(opts...),
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
