package team

import (
	"context"
	"encoding/json"
	"fmt"

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
		Description: "Submit a structured task result for your assigned task.",
		Parameters: map[string]any{
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
		Required: []string{"summary"},
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

	res.TaskID = t.todoID
	res.Source = "submitted"
	if res.Confidence == 0 {
		res.Confidence = 1.0
	}

	if t.coordinator != nil {
		t.coordinator.storeSubmittedTaskResult(t.todoID, &res)
	}
	return fantasy.NewTextResponse("Task result submitted successfully."), nil
}
