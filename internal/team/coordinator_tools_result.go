package team

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/tools"
)

// SubmitArtifactInput is the public, model-authored artifact declaration.
// Artifact identity and occurrence provenance are deliberately absent: those
// fields are issued by the runtime after the source file is snapshotted.
type SubmitArtifactInput struct {
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"`
}

// SubmitResultInput is the public submit_result DTO. It contains only model
// claims and descriptive handoff data; runtime-owned TaskResult identity,
// outputs, and transcript references cannot be decoded through this boundary.
type SubmitResultInput struct {
	Status             string                `json:"status"`
	Summary            string                `json:"summary"`
	Details            string                `json:"details,omitempty"`
	Artifacts          []SubmitArtifactInput `json:"artifacts,omitempty"`
	Evidence           []EvidenceRef         `json:"evidence,omitempty"`
	FilesRead          []FileRef             `json:"files_read,omitempty"`
	FilesModified      []FileRef             `json:"files_modified,omitempty"`
	Commands           []CommandResult       `json:"commands,omitempty"`
	Verification       []VerificationResult  `json:"verification,omitempty"`
	Decisions          []Decision            `json:"decisions,omitempty"`
	Findings           []Finding             `json:"findings,omitempty"`
	Risks              []Risk                `json:"risks,omitempty"`
	OpenQuestions      OpenQuestions         `json:"open_questions,omitempty"`
	SuggestedNextTasks []TaskProposal        `json:"suggested_next_tasks,omitempty"`
	RetryHint          string                `json:"retry_hint,omitempty"`
	ReceiptIDs         []string              `json:"receipt_ids,omitempty"`
	MemoryUses         []MemoryUseRef        `json:"memory_uses,omitempty"`
	Facts              map[string]any        `json:"facts,omitempty"`
	Confidence         float64               `json:"confidence"`
}

func (input SubmitResultInput) taskResult() TaskResult {
	artifacts := make([]ArtifactRef, len(input.Artifacts))
	for i, artifact := range input.Artifacts {
		artifacts[i] = ArtifactRef{
			Path:        artifact.Path,
			Description: artifact.Description,
			Type:        artifact.Type,
			Kind:        artifact.Type,
		}
	}
	return TaskResult{
		Status:             input.Status,
		Summary:            input.Summary,
		Details:            input.Details,
		Artifacts:          artifacts,
		Evidence:           input.Evidence,
		FilesRead:          input.FilesRead,
		FilesModified:      input.FilesModified,
		Commands:           input.Commands,
		Verification:       input.Verification,
		Decisions:          input.Decisions,
		Findings:           input.Findings,
		Risks:              input.Risks,
		OpenQuestions:      input.OpenQuestions,
		SuggestedNextTasks: input.SuggestedNextTasks,
		RetryHint:          input.RetryHint,
		ReceiptIDs:         input.ReceiptIDs,
		MemoryUses:         input.MemoryUses,
		Facts:              input.Facts,
		Confidence:         input.Confidence,
	}
}

func decodeSubmitResultInput(raw []byte, contract taskResultSubmissionContract) (SubmitResultInput, error) {
	var input SubmitResultInput
	normalized := normalizeSubmitResultInput(raw)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(normalized, &fields); err == nil {
		if !contract.AllowEvidence {
			if _, ok := fields["evidence"]; ok {
				return SubmitResultInput{}, fmt.Errorf("evidence is not a legal field for this task; report observed files in files_read")
			}
		}
		if !contract.AllowArtifacts {
			if _, ok := fields["artifacts"]; ok {
				return SubmitResultInput{}, fmt.Errorf("artifacts are forbidden by this task's execution contract; omit the artifacts field")
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(normalized))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return SubmitResultInput{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return SubmitResultInput{}, fmt.Errorf("trailing JSON value")
		}
		return SubmitResultInput{}, err
	}
	return input, nil
}

type submitResultTool struct {
	coordinator *Coordinator
	todoID      string
	sink        TaskResultSink
}

func (t *submitResultTool) ProviderOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{}
}
func (t *submitResultTool) SetProviderOptions(opts fantasy.ProviderOptions) {}

func (t *submitResultTool) Info() fantasy.ToolInfo {
	return submitResultToolInfo(t.submissionContract())
}

func (t *submitResultTool) submissionContract() taskResultSubmissionContract {
	if t == nil || t.coordinator == nil {
		return taskResultSubmissionContract{AllowEvidence: true, AllowArtifacts: true}
	}
	item := t.coordinator.todoItemByID(t.todoID)
	if item == nil {
		return taskResultSubmissionContract{AllowEvidence: true, AllowArtifacts: true}
	}
	return taskResultSubmissionContractForTask(TaskDef{
		ID:         item.ID,
		Verify:     item.Verify,
		VerifyMode: item.VerifyMode,
		VerifySpec: item.VerifySpec,
		Execution:  item.Execution,
	})
}

func submitResultToolInfo(contract taskResultSubmissionContract) fantasy.ToolInfo {
	info := fantasy.ToolInfo{
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
					"required":             []string{"path"},
					"additionalProperties": false,
				},
			},
			"files_read": map[string]any{
				"type":        "array",
				"description": "List of files read during the task.",
				"items": map[string]any{"oneOf": []any{
					map[string]any{"type": "string"},
					map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":    map[string]any{"type": "string"},
							"purpose": map[string]any{"type": "string"},
						},
						"required": []string{"path"}, "additionalProperties": false,
					},
				}},
			},
			"files_modified": map[string]any{
				"type":        "array",
				"description": "List of files modified or created during the task.",
				"items": map[string]any{"oneOf": []any{
					map[string]any{"type": "string"},
					map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":    map[string]any{"type": "string"},
							"purpose": map[string]any{"type": "string"},
						},
						"required": []string{"path"}, "additionalProperties": false,
					},
				}},
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
			"evidence": map[string]any{
				"type":        "array",
				"description": "Optional structured evidence references for generic tasks. Do not use this as a substitute for files_read when the task requires observed files.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task_id":     map[string]any{"type": "string"},
						"run_id":      map[string]any{"type": "string"},
						"type":        map[string]any{"type": "string"},
						"description": map[string]any{"type": "string"},
						"value":       map[string]any{"type": "string"},
						"system_hmac": map[string]any{"type": "string", "description": "Compatibility field ignored by the runtime; workers cannot authorize evidence with it."},
					},
					"required":             []string{"type", "description"},
					"additionalProperties": false,
				},
			},
			"commands": map[string]any{
				"type": "array", "items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command":   map[string]any{"type": "string"},
						"exit_code": map[string]any{"type": "integer"},
						"output":    map[string]any{"type": "string"},
					},
					"required": []string{"command", "exit_code"}, "additionalProperties": false,
				},
			},
			"verification": map[string]any{
				"type": "array", "items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"}, "work_dir": map[string]any{"type": "string"},
						"exit_code": map[string]any{"type": "integer"}, "stdout": map[string]any{"type": "string"},
						"stderr": map[string]any{"type": "string"}, "duration": map[string]any{"type": "integer"},
						"timed_out": map[string]any{"type": "boolean"}, "weak_warning": map[string]any{"type": "boolean"},
						"weak_reason": map[string]any{"type": "string"}, "overturned": map[string]any{"type": "boolean"},
						"overturn_reason": map[string]any{"type": "string"}, "fingerprint": map[string]any{"type": "string"},
						"evaluated_at": map[string]any{"type": "string"},
					},
					"additionalProperties": false,
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
				"description": "Identified risks or concerns for the final report. This is a non-blocking handoff field; use status=partial, failed, or blocked when the assigned task itself is incomplete.",
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
			"suggested_next_tasks": map[string]any{
				"type": "array", "items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"goal": map[string]any{"type": "string"}, "agent": map[string]any{"type": "string"}, "reason": map[string]any{"type": "string"},
					},
					"required": []string{"goal"}, "additionalProperties": false,
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
			"memory_uses": map[string]any{
				"type":        "array",
				"description": "Canonical memory records actually applied, consulted, or rejected. Use only IDs and retrieval_id from the injected context; an empty array is valid.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"retrieval_id":    map[string]any{"type": "string"},
						"context_item_id": map[string]any{"type": "string"},
						"disposition":     map[string]any{"type": "string", "enum": []string{MemoryUseApplied, MemoryUseConsulted, MemoryUseRejected}},
						"reason_code":     map[string]any{"type": "string"},
						"confidence":      map[string]any{"type": "number", "minimum": 0.0, "maximum": 1.0},
					},
					"required":             []string{"retrieval_id", "context_item_id", "disposition", "confidence"},
					"additionalProperties": false,
				},
			},
			"facts": map[string]any{
				"type":        "object",
				"description": "Named JSON values a later task can reference by name via its own fact_refs, instead of a coordinator retyping a value this task already discovered (a list, a computed count, a resolved path) into a later dispatch's goal or constraints. Keys are fact names; values may be any JSON type.",
			},
		},
		Required: []string{"status", "summary"},
	}
	if !contract.AllowEvidence {
		delete(info.Parameters, "evidence")
	}
	if !contract.AllowArtifacts {
		delete(info.Parameters, "artifacts")
	}
	if contract.FilesReadMinItems > 0 {
		filesRead, ok := info.Parameters["files_read"].(map[string]any)
		if ok {
			filesRead["minItems"] = contract.FilesReadMinItems
		}
	}
	for _, field := range contract.RequiredFields {
		if _, ok := info.Parameters[field]; !ok || slices.Contains(info.Required, field) {
			continue
		}
		info.Required = append(info.Required, field)
	}
	return info
}

func (t *submitResultTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	contract := t.submissionContract()
	input, err := decodeSubmitResultInput([]byte(call.Input), contract)
	if err != nil {
		if strings.Contains(err.Error(), `unknown field "raw_output_ref"`) {
			return fantasy.NewTextErrorResponse("raw_output_ref is runtime-owned; workers cannot declare or copy transcript references"), nil
		}
		if strings.Contains(err.Error(), `unknown field "outputs"`) {
			return fantasy.NewTextErrorResponse("outputs are runtime-owned; cite execution receipt_ids instead of declaring task outputs"), nil
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid submit_result arguments: %v", err)), nil
	}
	res := input.taskResult()
	if res.Summary == "" {
		return fantasy.NewTextErrorResponse("summary is required"), nil
	}
	switch strings.ToLower(strings.TrimSpace(res.Status)) {
	case TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps, TaskResultStatusPartial, TaskResultStatusFailed, TaskResultStatusBlocked:
		res.Status = strings.ToLower(strings.TrimSpace(res.Status))
	default:
		return fantasy.NewTextErrorResponse("status must be success, completed_with_gaps, partial, failed, or blocked"), nil
	}
	if err := contract.validateWorkerClaims(&res); err != nil {
		return fantasy.NewTextErrorResponse("submit_result contract violation: " + err.Error()), nil
	}

	var identity submitResultRuntimeIdentity
	if protocolRepairExecution(ctx) && len(res.Artifacts) > 0 {
		return fantasy.NewTextErrorResponse("protocol repair cannot add artifact evidence; submit the corrected result without artifacts"), nil
	}
	if len(res.Outputs) > 0 {
		return fantasy.NewTextErrorResponse("outputs are runtime-owned; cite execution receipt_ids instead of declaring task outputs"), nil
	}
	if res.RawOutputRef != nil {
		return fantasy.NewTextErrorResponse("raw_output_ref is runtime-owned; workers cannot declare or copy transcript references"), nil
	}
	if t.forbidsArtifacts() && len(res.Artifacts) > 0 {
		return fantasy.NewTextErrorResponse("artifacts are forbidden by this task's execution contract; omit the artifacts field and submit the result again"), nil
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
	// Security: strip model-injected HMAC signatures from submit_result tool input
	for i := range res.Evidence {
		res.Evidence[i].SystemHMAC = ""
	}

	sink := t.sink
	if sink == nil && t.coordinator != nil {
		sink = coordinatorTaskResultSink{coordinator: t.coordinator}
	}
	if t.coordinator != nil {
		identity, err = submitResultRuntimeIdentityFromContext(ctx, t.coordinator, t.todoID)
		if err != nil {
			return fantasy.NewTextErrorResponse("invalid submit_result runtime identity: " + err.Error()), nil
		}
		// Receipt admission still needs the active occurrence, but it must run
		// before the transaction mutex is held because that validator reads the
		// occurrence through the public snapshot helper. The attempt is runtime
		// supplied here, never worker-authored.
		receiptCandidate := res
		receiptCandidate.Attempt = identity.Attempt
		if err := t.coordinator.validateTaskResultReceiptClaims(t.todoID, &receiptCandidate); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		tx, txErr := t.coordinator.beginTaskResultSubmission(identity)
		if txErr != nil {
			return fantasy.NewTextErrorResponse("store submitted result: " + txErr.Error()), nil
		}
		// Runtime ownership begins only after the occurrence is reserved. The
		// candidate now contains the values that worker-independent assertions
		// are allowed to inspect.
		res.TaskID = identity.TaskID
		res.Attempt = identity.Attempt
		res.Agent = identity.Agent
		res.Source = "submitted"
		if res.Confidence == 0 {
			res.Confidence = 1.0
		}
		rollback := func(prefix string) fantasy.ToolResponse {
			if rollbackErr := tx.rollback(); rollbackErr != nil {
				prefix += "; rollback: " + rollbackErr.Error()
			}
			return fantasy.NewTextErrorResponse(prefix)
		}
		if err := t.coordinator.validateMemoryUseClaims(ctx, t.todoID, &res); err != nil {
			return rollback(err.Error()), nil
		}
		// Only a success claim can be mechanically contradicted by terminal
		// evidence. Check it while the occurrence is reserved so no projection
		// can be published before the result is accepted.
		if res.Status == "success" {
			if err := t.coordinator.terminalTaskFailure(ctx, t.todoID); err != nil {
				return rollback("success rejected: " + err.Error()), nil
			}
		}
		if err := t.materializeSubmittedArtifacts(ctx, &res, tx); err != nil {
			return rollback("invalid artifact declaration: " + err.Error()), nil
		}
		if err := contract.validateFinalizableResult(&res); err != nil {
			return rollback("submit_result contract violation: " + err.Error()), nil
		}
		if _, isCoordinatorSink := sink.(coordinatorTaskResultSink); isCoordinatorSink {
			// The coordinator sink is represented by this transaction; invoking it
			// again would attempt a second reservation while this gate is held.
		} else if sink != nil {
			if err := sink.Submit(ctx, t.todoID, res); err != nil {
				return rollback("store submitted result: " + err.Error()), nil
			}
		}
		if err := tx.commit(&res); err != nil {
			return rollback("store submitted result: " + err.Error()), nil
		}
		if stop, ok := ctx.Value(acceptedTerminalResultStopKey{}).(*acceptedTerminalResultStop); ok {
			stop.markAccepted(identity)
		}
		t.coordinator.emitMemoryUsageEvents(&res)
		tx.finish()
	} else if sink != nil {
		res.TaskID = t.todoID
		res.Source = "submitted"
		if res.Confidence == 0 {
			res.Confidence = 1.0
		}
		if err := contract.validateFinalizableResult(&res); err != nil {
			return fantasy.NewTextErrorResponse("submit_result contract violation: " + err.Error()), nil
		}
		if err := sink.Submit(ctx, t.todoID, res); err != nil {
			return fantasy.NewTextErrorResponse("store submitted result: " + err.Error()), nil
		}
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

type submitResultRuntimeIdentity struct {
	RunID   string
	TaskID  string
	Attempt int
	Agent   string
}

func (c *Coordinator) stageSubmittedArtifacts(identity submitResultRuntimeIdentity, refs []ArtifactRef) {
	if c == nil || len(refs) == 0 || !validSubmitResultIdentity(identity) {
		return
	}
	controller := c.occurrenceController(identity.TaskID)
	controller.mu.Lock()
	if controller.opened && sameTaskResultOccurrence(controller.identity, identity) && !controller.reserved && controller.result == nil {
		controller.pending = append([]ArtifactRef(nil), refs...)
	}
	controller.mu.Unlock()
}

// materializeSubmittedArtifacts makes a worker's artifact claim durable before
// accepting its terminal result. Previously a model could declare a missing
// path, receive a successful submit_result response, and leave the evidence
// manifest to fail only at run finalization. Artifacts are workspace-contained
// evidence, so snapshot them here or reject the result while the worker can
// still correct its claim.
func (t *submitResultTool) materializeSubmittedArtifacts(ctx context.Context, res *TaskResult, tx *taskResultOccurrenceTransaction) error {
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
	identity, err := submitResultRuntimeIdentityFromContext(ctx, t.coordinator, t.todoID)
	if err != nil {
		return err
	}
	if res.TaskID != identity.TaskID || res.Attempt != identity.Attempt || res.Agent != identity.Agent {
		return fmt.Errorf("result provenance does not match runtime occurrence")
	}
	if err := validateSubmittedArtifactClaims(ctx, workspace, res.Artifacts); err != nil {
		return err
	}
	for i, artifact := range res.Artifacts {
		ref, err := putSubmittedArtifact(ctx, store, identity, artifact)
		if err != nil {
			if tx != nil {
				tx.addMaterialized(nil)
			}
			return fmt.Errorf("artifacts[%d] %q: %w", i, artifact.Path, err)
		}
		res.Artifacts[i] = ref
	}
	if tx != nil {
		tx.addMaterialized(res.Artifacts)
	}
	return nil
}

func validateSubmittedArtifactClaims(ctx context.Context, workspace string, artifacts []ArtifactRef) error {
	for i, artifact := range artifacts {
		if artifact.ID != "" {
			return fmt.Errorf("artifacts[%d] %q: artifact ids are runtime-owned", i, artifact.Path)
		}
		if artifact.SHA256 != "" || artifact.Bytes != 0 || artifact.ByteSize != 0 || artifact.RunID != "" || artifact.TaskID != "" || artifact.Attempt != 0 || artifact.Agent != "" || artifact.Provider != "" || artifact.ToolCallID != "" || !artifact.CreatedAt.IsZero() {
			return fmt.Errorf("artifacts[%d] %q: artifact provenance is runtime-owned", i, artifact.Path)
		}
		if strings.TrimSpace(artifact.Path) == "" {
			return fmt.Errorf("artifacts[%d] path is required", i)
		}
		path, err := resolveArtifactSourcePath(workspace, artifact.Path)
		if err != nil {
			return fmt.Errorf("artifacts[%d] %q: %w", i, artifact.Path, err)
		}
		if policy, ok := ctx.Value(tools.ArtifactPathPolicyKey).(tools.ArtifactPathPolicy); ok {
			if err := tools.EnforceArtifactPathPolicy(path, &policy); err != nil {
				return fmt.Errorf("artifacts[%d] %q: %w", i, artifact.Path, err)
			}
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("artifacts[%d] %q: %w", i, artifact.Path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("artifacts[%d] %q must be a regular file", i, artifact.Path)
		}
	}
	return nil
}

func putSubmittedArtifact(ctx context.Context, store *FileArtifactStore, identity submitResultRuntimeIdentity, artifact ArtifactRef) (ArtifactRef, error) {
	_, err := resolveArtifactSourcePath(store.sourceDir, artifact.Path)
	if err != nil {
		return ArtifactRef{}, err
	}
	putResult, err := store.Put(ctx, PutArtifactRequest{
		Kind: artifact.Kind, Path: artifact.Path, Description: artifact.Description, MediaType: artifact.MediaType,
		SourcePath: artifact.Path, RunID: identity.RunID, TaskID: identity.TaskID, Attempt: identity.Attempt, Agent: identity.Agent,
	})
	return putResult.ArtifactRef, err
}
