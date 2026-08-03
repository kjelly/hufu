package team

import (
	"context"
	"fmt"
	"strings"
)

type PolicyDecisionCode string

const (
	DecisionAllow PolicyDecisionCode = "allow"
	DecisionDeny  PolicyDecisionCode = "deny"
)

type PolicyObligation struct {
	Kind  string `json:"kind"`
	Value string `json:"value,omitempty"`
}

type PolicyDecision struct {
	Code        PolicyDecisionCode `json:"code"`
	RuleID      string             `json:"rule_id,omitempty"`
	Reason      string             `json:"reason,omitempty"`
	Obligations []PolicyObligation `json:"obligations,omitempty"`
}

type ToolAuthorizationRequest struct {
	Agent        string
	Tool         string
	AllowedTools map[string]bool
	FailureMode  PolicyFailureMode
}

type MCPAuthorizationRequest struct {
	Agent        string
	Server       string
	Tool         string
	AllowedTools map[string]bool
	FailureMode  PolicyFailureMode
}

type TaskTransitionRequest struct {
	TaskID      string
	From        TaskStatus
	To          TaskStatus
	FailureMode PolicyFailureMode
}

// AuthorizationPolicy is the fail-closed middleware contract for built-in,
// MCP, and task lifecycle operations. It remains separate from the legacy
// cache PolicyEngine interface so existing adapters stay source-compatible.
type AuthorizationPolicy interface {
	AuthorizeToolCall(context.Context, ToolAuthorizationRequest) (PolicyDecision, error)
	AuthorizeMCPCall(context.Context, MCPAuthorizationRequest) (PolicyDecision, error)
	AuthorizeTransition(context.Context, TaskTransitionRequest) (PolicyDecision, error)
}

type defaultAuthorizationPolicy struct{}

func (defaultAuthorizationPolicy) AuthorizeToolCall(ctx context.Context, req ToolAuthorizationRequest) (PolicyDecision, error) {
	if err := ctx.Err(); err != nil {
		return PolicyDecision{Code: DecisionDeny, RuleID: "policy.context", Reason: err.Error()}, err
	}
	// ask_user is the explicit human-escalation boundary and is handled by
	// its own unattended/interactive policy. It must remain callable even when
	// a worker has no execution-tool grants.
	if req.Tool == "ask_user" {
		return PolicyDecision{Code: DecisionAllow, RuleID: "tool.ask_user"}, nil
	}
	if strings.TrimSpace(req.Tool) == "" || req.AllowedTools == nil || !req.AllowedTools[req.Tool] {
		return PolicyDecision{Code: DecisionDeny, RuleID: "tool.allowlist", Reason: fmt.Sprintf("tool %q is not authorized", req.Tool)}, nil
	}
	return PolicyDecision{Code: DecisionAllow, RuleID: "tool.allowlist"}, nil
}

func (p defaultAuthorizationPolicy) AuthorizeMCPCall(ctx context.Context, req MCPAuthorizationRequest) (PolicyDecision, error) {
	name := req.Tool
	if req.Server != "" {
		name = req.Server + ":" + req.Tool
	}
	return p.AuthorizeToolCall(ctx, ToolAuthorizationRequest{Agent: req.Agent, Tool: name, AllowedTools: req.AllowedTools, FailureMode: req.FailureMode})
}

func (defaultAuthorizationPolicy) AuthorizeTransition(ctx context.Context, req TaskTransitionRequest) (PolicyDecision, error) {
	if err := ctx.Err(); err != nil {
		return PolicyDecision{Code: DecisionDeny, RuleID: "transition.context", Reason: err.Error()}, err
	}
	if req.TaskID == "" || req.To == "" {
		return PolicyDecision{Code: DecisionDeny, RuleID: "transition.invalid", Reason: "task ID and target status are required"}, nil
	}
	return PolicyDecision{Code: DecisionAllow, RuleID: "transition.default"}, nil
}
