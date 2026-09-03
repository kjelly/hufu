package team

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	contextstore "github.com/kjelly/hufu/internal/context"

	"charm.land/fantasy"
)

const verifiedHistoryBudgetTokens = 16_000

// canonicalCompactionInput is the single conversation-to-compaction boundary.
// Its checked ContextItems and edges are the source for both the verified
// history projection and the sidecar conversation. Raw fantasy messages stay
// available to invariant validation, but never cross into the sidecar.
type canonicalCompactionInput struct {
	result contextstore.CompactionResult
}

func (c *Coordinator) compactionScope() contextstore.Scope {
	scope := contextstore.Scope{ProjectID: "hufu"}
	if c != nil && c.session != nil {
		scope.TeamID = c.session.Config.Name
		scope.SessionID = c.session.Workspace
	}
	return scope
}

func (c *Coordinator) compactionOutputPolicy() contextstore.ToolOutputPolicy {
	policy := c.compactionPolicy()
	return contextstore.ToolOutputPolicy{
		MaxBytes: policy.ToolOutputMaxBytes, MaxRunes: policy.ToolOutputMaxRunes, MaxTokens: policy.ToolOutputMaxTokens,
		DiagnosticLines: policy.DiagnosticMaxLines, DiagnosticTokens: policy.DiagnosticMaxTokens,
	}
}

// buildCanonicalCompactionInput runs the checked evidence pipeline exactly
// once. The deterministic compactor both validates the required evidence and
// renders the bounded canonical conversation supplied to the sidecar.
func buildCanonicalCompactionInput(ctx context.Context, messages []fantasy.Message, scope contextstore.Scope, outputPolicy contextstore.ToolOutputPolicy, targetTokens int) (canonicalCompactionInput, error) {
	items, edges, err := conversationEvidenceChecked(messages, scope, outputPolicy)
	if err != nil {
		return canonicalCompactionInput{}, err
	}
	result, err := (contextstore.ValidatedCompactor{}).Compact(ctx, contextstore.CompactionRequest{
		Items: items, Edges: edges, TargetTokens: targetTokens,
	})
	if err != nil {
		return canonicalCompactionInput{}, err
	}
	if strings.TrimSpace(result.Content) == "" {
		return canonicalCompactionInput{}, fmt.Errorf("canonical compaction input is empty")
	}
	return canonicalCompactionInput{result: result}, nil
}

func (c *Coordinator) buildCanonicalCompactionInput(ctx context.Context, messages []fantasy.Message) (canonicalCompactionInput, error) {
	policy := c.compactionPolicy()
	return buildCanonicalCompactionInput(ctx, messages, c.compactionScope(), c.compactionOutputPolicy(), policy.VerifiedHistoryTargetTokens)
}

// compactVerifiedConversation is the production bridge from fantasy history to
// canonical ContextItems. Its result is the evidence section injected into the
// compacted history message; a validation error retains original history.
func (c *Coordinator) compactVerifiedConversation(ctx context.Context, messages []fantasy.Message) (contextstore.CompactionResult, error) {
	input, err := c.buildCanonicalCompactionInput(ctx, messages)
	if err != nil {
		return contextstore.CompactionResult{}, err
	}
	return input.result, nil
}

func conversationEvidence(messages []fantasy.Message, scope contextstore.Scope, policies ...contextstore.ToolOutputPolicy) ([]contextstore.ContextItem, []contextstore.ContextEdge) {
	items, edges, _ := conversationEvidenceChecked(messages, scope, policies...)
	return items, edges
}

func conversationEvidenceChecked(messages []fantasy.Message, scope contextstore.Scope, policies ...contextstore.ToolOutputPolicy) ([]contextstore.ContextItem, []contextstore.ContextEdge, error) {
	var outputPolicy contextstore.ToolOutputPolicy
	if len(policies) > 0 {
		outputPolicy = policies[0]
	}
	items := make([]contextstore.ContextItem, 0, len(messages))
	edges := make([]contextstore.ContextEdge, 0)
	calls := map[string]contextstore.ToolCallEvidence{}
	for mi, msg := range messages {
		for pi, part := range msg.Content {
			id := fmt.Sprintf("history-%04d-%02d", mi, pi)
			if call, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				evidence := contextstore.ToolCallEvidence{ID: call.ToolCallID, Tool: call.ToolName, Command: call.Input, Scope: scope}
				var input map[string]any
				if json.Unmarshal([]byte(call.Input), &input) == nil {
					for _, key := range []string{"command", "cmd"} {
						if value, ok := input[key].(string); ok && value != "" {
							evidence.Command = value
							break
						}
					}
					for _, key := range []string{"working_dir", "workdir", "cwd", "dir"} {
						if value, ok := input[key].(string); ok && value != "" {
							evidence.WorkingDir = value
							break
						}
					}
				}
				if evidence.ID == "" {
					evidence.ID = id
				}
				calls[evidence.ID] = evidence
				callItems, _, err := contextstore.ToolEvidenceItemsChecked(evidence, contextstore.ToolResultEvidence{ID: "pending-" + evidence.ID, ToolCallID: evidence.ID, OutputPolicy: outputPolicy, Scope: scope})
				if err != nil {
					return nil, nil, fmt.Errorf("tool call %q evidence cannot be rendered: %w", evidence.ID, err)
				}
				items = append(items, callItems[0])
				continue
			}
			if result, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				output, isError := toolResultOutputText(result.Output)
				call := calls[result.ToolCallID]
				if call.ID == "" {
					call = contextstore.ToolCallEvidence{ID: result.ToolCallID, Tool: "unknown", Scope: scope}
					if call.ID == "" {
						call.ID = id + "-call"
					}
					callItems, _, err := contextstore.ToolEvidenceItemsChecked(call, contextstore.ToolResultEvidence{ID: "pending-" + call.ID, ToolCallID: call.ID, OutputPolicy: outputPolicy, Scope: scope})
					if err != nil {
						return nil, nil, fmt.Errorf("tool call %q evidence cannot be rendered: %w", call.ID, err)
					}
					items = append(items, callItems[0])
				}
				verification := "passed"
				if isError {
					verification = "failed"
				}
				evidence := contextstore.ToolResultEvidence{ID: id, ToolCallID: call.ID, Tool: call.Tool, Command: call.Command, WorkingDir: call.WorkingDir, Stdout: output, Verification: verification, OutputPolicy: outputPolicy, Scope: scope}
				resultItems, resultEdges, err := contextstore.ToolEvidenceItemsChecked(call, evidence)
				if err != nil {
					return nil, nil, fmt.Errorf("tool result %q evidence cannot be rendered: %w", evidence.ID, err)
				}
				items = append(items, resultItems[1])
				edges = append(edges, resultEdges...)
				continue
			}
			if text, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && strings.TrimSpace(text.Text) != "" {
				kind, priority, mustKeep := contextstore.ContextProgress, contextstore.PriorityNormal, false
				if msg.Role == fantasy.MessageRoleUser {
					kind, priority, mustKeep = contextstore.ContextRequirement, contextstore.PriorityCritical, mi == 0
				}
				if msg.Role == fantasy.MessageRoleUser && strings.Contains(text.Text, "?") {
					kind, mustKeep = contextstore.ContextOpenQuestion, true
				}
				items = append(items, contextstore.ContextItem{ID: id, Kind: kind, Content: text.Text, Scope: scope, Authority: contextstore.AuthorityAgent, TrustLevel: contextstore.TrustInternal, Priority: priority, MustKeep: mustKeep, Confidence: 1.0})
			}
		}
	}
	return items, edges, nil
}
