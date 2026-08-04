package team

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"
)

type submitResultTool struct {
	coordinator *Coordinator
	todoID      string
}

func (t *submitResultTool) ProviderOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{}
}
func (t *submitResultTool) SetProviderOptions(opts fantasy.ProviderOptions) {}

func (t *submitResultTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "submit_result",
		Description: "Submit the terminal structured result for your assigned task. Call exactly once when finished. status=success is required for the task to be marked done; use partial, failed, or blocked when the requested outcome was not achieved.",
		Parameters: map[string]any{
			"status": map[string]any{
				"type": "string", "enum": []string{"success", "partial", "failed", "blocked"},
				"description": "Terminal outcome of the assigned goal",
			},
			"summary": map[string]any{
				"type":        "string",
				"description": "High-level summary of the task result and accomplishments.",
			},
			"artifacts": map[string]any{
				"type":        "array",
				"description": "List of artifacts produced by this task.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":        map[string]any{"type": "string"},
						"description": map[string]any{"type": "string"},
						"type":        map[string]any{"type": "string"},
					},
					"required": []string{"path"},
				},
			},
			"raw_output_ref": map[string]any{
				"type":        "object",
				"description": "Runner-owned complete tool transcript. For output_mode=verbatim hufu fills and verifies this field automatically; workers must not invent it.",
				"properties": map[string]any{
					"path":        map[string]any{"type": "string"},
					"description": map[string]any{"type": "string"},
					"type":        map[string]any{"type": "string"},
					"sha256":      map[string]any{"type": "string"},
					"bytes":       map[string]any{"type": "integer"},
				},
			},
			"files_read": map[string]any{
				"type":        "array",
				"description": "List of files read during the task.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string"},
						"purpose": map[string]any{"type": "string"},
					},
					"required": []string{"path"},
				},
			},
			"files_modified": map[string]any{
				"type":        "array",
				"description": "List of files modified or created during the task.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string"},
						"purpose": map[string]any{"type": "string"},
					},
					"required": []string{"path"},
				},
			},
			"decisions": map[string]any{
				"type":        "array",
				"description": "Key technical decisions made during execution.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"topic":  map[string]any{"type": "string"},
						"choice": map[string]any{"type": "string"},
						"reason": map[string]any{"type": "string"},
					},
					"required": []string{"topic", "choice"},
				},
			},
			"findings": map[string]any{
				"type":        "array",
				"description": "Key findings or insights discovered.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"category": map[string]any{"type": "string"},
						"summary":  map[string]any{"type": "string"},
						"detail":   map[string]any{"type": "string"},
					},
					"required": []string{"summary"},
				},
			},
			"risks": map[string]any{
				"type":        "array",
				"description": "Identified risks or concerns.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"description": map[string]any{"type": "string"},
						"impact":      map[string]any{"type": "string"},
						"mitigation":  map[string]any{"type": "string"},
					},
					"required": []string{"description"},
				},
			},
			"open_questions": map[string]any{
				"type":        "array",
				"description": "Unresolved questions or follow-ups.",
				"items":       map[string]any{"type": "string"},
			},
			"retry_hint": map[string]any{
				"type":        "string",
				"description": "Feedback or hint if this result is part of a retry or indicates partial failure.",
			},
			"confidence": map[string]any{
				"type":        "number",
				"description": "Self-assessed confidence level (0.0 to 1.0).",
			},
		},
		Required: []string{"status", "summary"},
	}
}

func (t *submitResultTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var res TaskResult
	if err := json.Unmarshal([]byte(call.Input), &res); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid submit_result arguments: %v", err)), nil
	}
	if res.Summary == "" {
		return fantasy.NewTextErrorResponse("summary is required"), nil
	}
	switch strings.ToLower(strings.TrimSpace(res.Status)) {
	case "success", "partial", "failed", "blocked":
		res.Status = strings.ToLower(strings.TrimSpace(res.Status))
	default:
		return fantasy.NewTextErrorResponse("status must be success, partial, failed, or blocked"), nil
	}

	res.TaskID = t.todoID
	res.Source = "submitted"
	if res.Confidence == 0 {
		res.Confidence = 1.0
	}
	// Only a success claim can be mechanically contradicted by terminal
	// evidence — partial/failed/blocked already say the task isn't done, so
	// there is nothing here for terminal evidence to override. Rejecting the
	// claim in the tool response (rather than only at round-end) lets the
	// model see the contradiction immediately and reconsider within the same
	// round instead of finding out only after it believed it had succeeded.
	if res.Status == "success" && t.coordinator != nil {
		if err := t.coordinator.terminalTaskFailure(ctx, t.todoID); err != nil {
			return fantasy.NewTextErrorResponse("success rejected: " + err.Error()), nil
		}
	}
	// Security: strip model-injected HMAC signatures from submit_result tool input
	for i := range res.Evidence {
		res.Evidence[i].SystemHMAC = ""
	}

	if t.coordinator != nil {
		t.coordinator.storeSubmittedTaskResult(t.todoID, &res)
	}
	return fantasy.NewTextResponse("Task result submitted successfully."), nil
}
