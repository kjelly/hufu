package team

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	contextstore "github.com/kjelly/hufu/internal/context"

	"charm.land/fantasy"
)

const verifiedHistoryBudgetTokens = 16_000

var exitStatusPattern = regexp.MustCompile(`(?i)exit (?:status|code)\s+(\d+)`)
var historyPathPattern = regexp.MustCompile(`(?:[A-Za-z0-9._-]+/)+[A-Za-z0-9._-]+`)

// compactVerifiedConversation is the production bridge from fantasy history to
// canonical ContextItems. Its result is the evidence section injected into the
// compacted history message; a validation error retains original history.
func (c *Coordinator) compactVerifiedConversation(ctx context.Context, messages []fantasy.Message) (contextstore.CompactionResult, error) {
	scope := contextstore.Scope{ProjectID: "hufu"}
	if c.session != nil {
		scope.TeamID = c.session.Config.Name
		scope.SessionID = c.session.Workspace
	}
	items, edges := conversationEvidence(messages, scope)
	return (contextstore.ValidatedCompactor{}).Compact(ctx, contextstore.CompactionRequest{Items: items, Edges: edges, TargetTokens: verifiedHistoryBudgetTokens})
}

func conversationEvidence(messages []fantasy.Message, scope contextstore.Scope) ([]contextstore.ContextItem, []contextstore.ContextEdge) {
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
				callItems, _ := contextstore.ToolEvidenceItems(evidence, contextstore.ToolResultEvidence{ID: "pending-" + evidence.ID, ToolCallID: evidence.ID, Scope: scope})
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
					callItems, _ := contextstore.ToolEvidenceItems(call, contextstore.ToolResultEvidence{ID: "pending-" + call.ID, ToolCallID: call.ID, Scope: scope})
					items = append(items, callItems[0])
				}
				var exitCode *int
				if match := exitStatusPattern.FindStringSubmatch(output); len(match) == 2 {
					n, _ := strconv.Atoi(match[1])
					exitCode = &n
				}
				verification := "passed"
				if isError || exitCode != nil && *exitCode != 0 {
					verification = "failed"
				}
				paths := historyPathPattern.FindAllString(output, -1)
				head, tail := outputHeadTail(output, 1_000)
				evidence := contextstore.ToolResultEvidence{ID: id, ToolCallID: call.ID, Tool: call.Tool, Command: call.Command, WorkingDir: call.WorkingDir, ExitCode: exitCode, Stdout: output, StdoutHead: head, StdoutTail: tail, MatchedErrors: diagnosticLines(output), ArtifactPaths: paths, ModifiedFiles: paths, Verification: verification, Scope: scope}
				_, resultEdges := contextstore.ToolEvidenceItems(call, evidence)
				resultItems, _ := contextstore.ToolEvidenceItems(call, evidence)
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
	return items, edges
}

func diagnosticLines(output string) []string {
	var out []string
	for _, line := range strings.Split(output, "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "fail") || strings.Contains(lower, "panic") || strings.Contains(lower, "exit status") {
			out = append(out, line)
		}
	}
	return out
}

func outputHeadTail(output string, capRunes int) (string, string) {
	runes := []rune(output)
	if capRunes <= 0 || len(runes) <= capRunes {
		return output, output
	}
	head := capRunes / 2
	return string(runes[:head]), string(runes[len(runes)-(capRunes-head):])
}
