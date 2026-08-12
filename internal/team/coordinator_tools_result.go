package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
		Description: "Submit the terminal structured result for your assigned task. Call exactly once when finished. status=success or completed_with_gaps marks the task done; completed_with_gaps means the assigned work is complete but it discovered a target-system limitation. Use partial, failed, or blocked when the assigned work itself was not completed.",
		Parameters: map[string]any{
			"status": map[string]any{
				"type": "string", "enum": []string{TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps, TaskResultStatusPartial, TaskResultStatusFailed, TaskResultStatusBlocked},
				"description": "Terminal outcome of the assigned goal",
			},
			"summary": map[string]any{
				"type":        "string",
				"description": "High-level summary of the task result and accomplishments.",
			},
			"details": map[string]any{
				"type":        "string",
				"description": "Complete textual deliverable for a plan, analysis, review, or handoff. Use this instead of writing a report file solely so the coordinator can read it; keep summary concise.",
			},
			"artifacts": map[string]any{
				"type":        "array",
				"description": "List of regular files already created inside the team workspace. Hufu snapshots each file when this result is submitted; do not declare planned or external files.",
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
				"description": "Unresolved questions or follow-ups. Each entry may be a string or a structured object with required question and optional context/detail strings.",
				"items": map[string]any{
					"oneOf": []any{
						map[string]any{"type": "string"},
						map[string]any{
							"type":                 "object",
							"properties":           map[string]any{"question": map[string]any{"type": "string"}, "context": map[string]any{"type": "string"}, "detail": map[string]any{"type": "string"}},
							"required":             []string{"question"},
							"additionalProperties": false,
						},
					},
				},
			},
			"retry_hint": map[string]any{
				"type":        "string",
				"description": "Feedback or hint if this result is part of a retry or indicates partial failure.",
			},
			"confidence": map[string]any{
				"type":        "number",
				"description": "Self-assessed confidence level (0.0 to 1.0).",
			},
			"receipt_ids": map[string]any{
				"type":        "array",
				"description": "Runtime-issued receipt IDs that support this result. Structured execution tasks require actual receipts from the current task attempt.",
				"items":       map[string]any{"type": "string"},
			},
		},
		Required: []string{"status", "summary"},
	}
}

func (t *submitResultTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var res TaskResult
	input := normalizeSubmitResultInput([]byte(call.Input))
	if err := json.Unmarshal(input, &res); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid submit_result arguments: %v", err)), nil
	}
	if res.Summary == "" {
		return fantasy.NewTextErrorResponse("summary is required"), nil
	}
	switch strings.ToLower(strings.TrimSpace(res.Status)) {
	case TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps, TaskResultStatusPartial, TaskResultStatusFailed, TaskResultStatusBlocked:
		res.Status = strings.ToLower(strings.TrimSpace(res.Status))
	default:
		return fantasy.NewTextErrorResponse("status must be success, completed_with_gaps, partial, failed, or blocked"), nil
	}

	res.TaskID = t.todoID
	res.Source = "submitted"
	if len(res.Outputs) > 0 {
		return fantasy.NewTextErrorResponse("outputs are runtime-owned; cite execution receipt_ids instead of declaring task outputs"), nil
	}
	if res.RawOutputRef != nil {
		return fantasy.NewTextErrorResponse("raw_output_ref is runtime-owned; workers cannot declare or copy transcript references"), nil
	}
	if res.Confidence == 0 {
		res.Confidence = 1.0
	}
	if t.forbidsArtifacts() && len(res.Artifacts) > 0 {
		return fantasy.NewTextErrorResponse("artifacts are forbidden by this task's execution contract; omit the artifacts field and submit the result again"), nil
	}
	if t.coordinator != nil {
		if err := t.coordinator.validateTaskResultReceiptClaims(t.todoID, &res); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
	}
	// A model-authored failure summary is useful context, but it is not the
	// canonical execution record. Prefix non-success results with the first
	// runtime-observed closed-sequence failure so downstream errors cannot
	// misidentify which slot or exit status actually failed.
	if !taskResultStatusIsSuccessful(res.Status) {
		if fact := taskToolSequenceFromContext(ctx).failureSummary(); fact != "" {
			workerSummary := strings.TrimSpace(res.Summary)
			res.Summary = fact
			if workerSummary != "" {
				res.Summary += ". Worker report: " + workerSummary
			}
			res.Findings = append(res.Findings, Finding{
				Category: "runtime_execution",
				Summary:  fact,
				Detail:   "Runtime-owned closed-sequence evidence; consult the sealed transcript for tool output.",
			})
		}
	}
	// Only a success claim can be mechanically contradicted by terminal
	// evidence — completed_with_gaps reports a target limitation rather than a
	// failed task, while partial/failed/blocked already say the task itself is
	// not done, so
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
		if err := t.materializeSubmittedArtifacts(ctx, &res); err != nil {
			return fantasy.NewTextErrorResponse("invalid artifact declaration: " + err.Error()), nil
		}
		t.coordinator.storeSubmittedTaskResult(t.todoID, &res)
	}
	return fantasy.NewTextResponse("Task result submitted successfully."), nil
}

func (t *submitResultTool) forbidsArtifacts() bool {
	if t == nil || t.coordinator == nil || t.coordinator.taskTracker == nil || t.coordinator.taskTracker.TodoList() == nil {
		return false
	}
	for _, item := range t.coordinator.taskTracker.TodoList().Items() {
		if item != nil && item.ID == t.todoID {
			return item.Execution.ForbidArtifacts
		}
	}
	return false
}

// normalizeSubmitResultInput preserves the typed result contract while being
// tolerant of common model shorthand for descriptive arrays. Models often
// collapse a one-entry array to a scalar or use an array of strings where the
// schema expects objects. These fields are result evidence, not mutation
// instructions, so the scalar forms can be losslessly promoted instead of
// turning an otherwise valid terminal result into a schema-repair loop.
func normalizeSubmitResultInput(input []byte) []byte {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(input, &object); err != nil {
		return input
	}
	objectFields := map[string]string{
		"files_read":           "path",
		"files_modified":       "path",
		"decisions":            "choice",
		"findings":             "summary",
		"risks":                "description",
		"suggested_next_tasks": "goal",
	}
	for field, key := range objectFields {
		raw, ok := object[field]
		if !ok {
			continue
		}
		entries, ok := normalizedStringEntries(raw)
		if !ok {
			continue
		}
		normalized := make([]json.RawMessage, 0, len(entries))
		changed := false
		for _, entry := range entries {
			var value string
			if err := json.Unmarshal(entry, &value); err != nil || strings.TrimSpace(value) == "" {
				normalized = append(normalized, entry)
				continue
			}
			ref, err := json.Marshal(map[string]string{key: value})
			if err != nil {
				return input
			}
			normalized = append(normalized, ref)
			changed = true
		}
		if changed {
			encoded, err := json.Marshal(normalized)
			if err != nil {
				return input
			}
			object[field] = encoded
		}
	}
	// Open questions have a dedicated typed normalizer because their
	// documented contract accepts both textual and structured entries. Keep
	// that normalization at TaskResult's JSON boundary so every caller sees
	// the same validation and canonical representation.
	normalized, err := json.Marshal(object)
	if err != nil {
		return input
	}
	return normalized
}

func normalizedStringEntries(raw json.RawMessage) ([]json.RawMessage, bool) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err == nil {
		return entries, true
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return nil, false
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	return []json.RawMessage{encoded}, true
}

// materializeSubmittedArtifacts makes a worker's artifact claim durable before
// accepting its terminal result. Previously a model could declare a missing
// path, receive a successful submit_result response, and leave the evidence
// manifest to fail only at run finalization. Artifacts are workspace-contained
// evidence, so snapshot them here or reject the result while the worker can
// still correct its claim.
func (t *submitResultTool) materializeSubmittedArtifacts(ctx context.Context, res *TaskResult) error {
	if res == nil || len(res.Artifacts) == 0 {
		return nil
	}
	if t.coordinator == nil || t.coordinator.session == nil || strings.TrimSpace(t.coordinator.session.Workspace) == "" {
		return fmt.Errorf("artifact submission requires a workspace")
	}
	workspace := t.coordinator.session.Workspace
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		return err
	}
	runID := t.coordinator.executionRunID
	if runID == "" {
		runID = "run-unknown"
	}
	// Validate the complete declaration before snapshotting any files, so a bad
	// later artifact cannot leave an accepted result with a partial artifact set.
	for i, artifact := range res.Artifacts {
		if artifact.ID != "" {
			if err := store.Verify(ctx, artifact); err != nil {
				return fmt.Errorf("artifacts[%d] %q: %w", i, artifact.Path, err)
			}
			continue
		}
		if strings.TrimSpace(artifact.Path) == "" {
			return fmt.Errorf("artifacts[%d] path is required", i)
		}
		path, err := resolveArtifactSourcePath(workspace, artifact.Path)
		if err != nil {
			return fmt.Errorf("artifacts[%d] %q: %w", i, artifact.Path, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("artifacts[%d] %q: %w", i, artifact.Path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("artifacts[%d] %q must be a regular file", i, artifact.Path)
		}
	}
	for i, artifact := range res.Artifacts {
		if artifact.ID != "" {
			continue
		}
		ref, err := store.Put(ctx, PutArtifactRequest{
			Kind:        artifact.Kind,
			Path:        artifact.Path,
			Description: artifact.Description,
			MediaType:   artifact.MediaType,
			SourcePath:  artifact.Path,
			RunID:       runID,
			TaskID:      t.todoID,
			Attempt:     1,
			Agent:       res.Agent,
		})
		if err != nil {
			return fmt.Errorf("artifacts[%d] %q: %w", i, artifact.Path, err)
		}
		res.Artifacts[i] = ref
	}
	return nil
}
