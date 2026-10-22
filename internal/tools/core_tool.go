//go:build linux || darwin
// +build linux darwin

package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/hooks"
)

type coreTool struct {
	info                   fantasy.ToolInfo
	handler                func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error)
	pOpts                  fantasy.ProviderOptions
	hooks                  *hooks.HookRegistry
	guardReviewer          GuardReviewFn
	pathReviewer           PathReviewer
	artifactPathPolicySafe bool
	workspaceScope         ToolWorkspaceScopeDescriptor
}

func (t *coreTool) Info() fantasy.ToolInfo                          { return t.info }
func (t *coreTool) ProviderOptions() fantasy.ProviderOptions        { return t.pOpts }
func (t *coreTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.pOpts = opts }
func (t *coreTool) DescribeWorkspaceScope() ToolWorkspaceScopeDescriptor {
	return t.workspaceScope
}

// IsTrustedArtifactPathTool reports whether a concrete built-in tool has opted
// into the shared artifact-path policy. Being a coreTool is not sufficient.
func IsTrustedArtifactPathTool(tool fantasy.AgentTool) bool {
	core, ok := tool.(*coreTool)
	return ok && core.artifactPathPolicySafe
}

// IsBuiltInTool reports whether tool is an authentic Hufu built-in. This is a
// concrete identity check, not a name check, so an external or MCP tool cannot
// impersonate a built-in tool by choosing the same name.
func IsBuiltInTool(tool fantasy.AgentTool) bool {
	_, ok := tool.(*coreTool)
	return ok
}

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

	call.Input, _ = CanonicalToolArguments(call.Input, t.info)
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

	if guardError := t.guardError(ctx, call.Input); guardError != "" {
		ReportToolExecutionDisposition(ctx, ToolExecutionDisposition{
			Kind: "guard_denied", ReasonCode: "guard_rule", ToolName: t.info.Name, ToolCallID: call.ID, Executed: false,
		})
		return fantasy.NewTextErrorResponse(guardError), nil
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

// evaluateDeterministicGuard keeps the safety-critical rule check separate
// from the general tool lifecycle so the model-based reviewer cannot bypass
// it and the core dispatch path remains easy to audit.
func evaluateDeterministicGuard(toolName, input string, rules []string) (string, error) {
	return EvaluateDeterministicGuardRules(toolName, input, rules)
}

func (t *coreTool) guardError(ctx context.Context, input string) string {
	rules, _ := ctx.Value(GuardRulesKey).([]string)
	if len(rules) == 0 {
		return ""
	}
	if reason, ruleErr := evaluateDeterministicGuard(t.info.Name, input, rules); ruleErr != nil {
		return "deterministic guard configuration error; tool call denied: " + ruleErr.Error()
	} else if reason != "" {
		return "Deterministic guard violation: " + reason
	}

	if t.guardReviewer == nil {
		return ""
	}
	approved, reason, reviewErr := t.guardReviewer(ctx, t.info.Name, input, rules)
	if reviewErr != nil {
		// Fail closed: when the guard reviewer cannot complete (timeout, model
		// error, etc.) we must not silently allow a call that was supposed to be
		// reviewed. Deny it so the agent retries or finds an allowed alternative.
		fmt.Fprintf(os.Stderr, "warning: guard review failed, denying tool call: %v\n", reviewErr)
		return fmt.Sprintf("Guard review unavailable (%v); tool call denied. Retry or use a different approach that does not require review.", reviewErr)
	}
	if !approved {
		return fmt.Sprintf("Guard rule violation: %s", reason)
	}
	return ""
}
