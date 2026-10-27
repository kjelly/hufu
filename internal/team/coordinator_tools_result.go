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
	Status               string                      `json:"status"`
	Summary              string                      `json:"summary"`
	Details              string                      `json:"details,omitempty"`
	Artifacts            []SubmitArtifactInput       `json:"artifacts,omitempty"`
	Evidence             []EvidenceRef               `json:"evidence,omitempty"`
	FilesRead            []FileRef                   `json:"files_read,omitempty"`
	FilesModified        []FileRef                   `json:"files_modified,omitempty"`
	Commands             []CommandResult             `json:"commands,omitempty"`
	Verification         []VerificationResult        `json:"verification,omitempty"`
	Decisions            []Decision                  `json:"decisions,omitempty"`
	Findings             []Finding                   `json:"findings,omitempty"`
	Risks                []Risk                      `json:"risks,omitempty"`
	OpenQuestions        OpenQuestions               `json:"open_questions,omitempty"`
	SuggestedNextTasks   []TaskProposal              `json:"suggested_next_tasks,omitempty"`
	RetryHint            string                      `json:"retry_hint,omitempty"`
	ReceiptIDs           []string                    `json:"receipt_ids,omitempty"`
	MemoryUses           []MemoryUseRef              `json:"memory_uses,omitempty"`
	AssumptionChecks     []AssumptionCheck           `json:"assumption_checks,omitempty"`
	InvariantAssessments *[]InvariantAssessmentClaim `json:"invariant_assessments,omitempty"`
	Facts                map[string]any              `json:"facts,omitempty"`
	Confidence           float64                     `json:"confidence,omitempty"`
	// StructuredPayload is validated against the task's result contract by
	// the runtime; it never becomes part of the TaskResult unvalidated.
	StructuredPayload json.RawMessage `json:"structured_payload,omitempty"`
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
		AssumptionChecks:   input.AssumptionChecks,
		Facts:              input.Facts,
		Confidence:         input.Confidence,
	}
}

func decodeSubmitResultInput(raw []byte, contract taskResultSubmissionContract) (SubmitResultInput, error) {
	var input SubmitResultInput
	// Reject ambiguity before legacy shape normalization can merge object keys.
	if _, err := decodeUniqueJSON(raw); err != nil {
		return input, err
	}
	normalized, err := normalizeSubmitResultInput(raw)
	if err != nil {
		return SubmitResultInput{}, err
	}
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
	candidate   *ResultCandidate
}

func (*submitResultTool) DescribeWorkspaceScope() tools.ToolWorkspaceScopeDescriptor {
	return tools.ToolWorkspaceScopeDescriptor{}
}

func (t *submitResultTool) ProviderOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{}
}
func (t *submitResultTool) SetProviderOptions(opts fantasy.ProviderOptions) {}

func (t *submitResultTool) Info() fantasy.ToolInfo {
	if t.candidate != nil {
		return resultPatchToolInfo(t.candidate)
	}
	return t.fullInfo()
}

func (t *submitResultTool) fullInfo() fantasy.ToolInfo {
	return localResultSubmissionInfo(t.coordinator, t.todoID)
}

func localResultSubmissionInfo(coordinator *Coordinator, todoID string) fantasy.ToolInfo {
	info := submitResultToolInfo(localResultSubmissionContract(coordinator, todoID))
	if coordinator != nil {
		_, compiled, _ := coordinator.boundResultContract(todoID)
		if compiled != nil {
			info = projectResultSubmissionFields(info, compiled.submissionFields)
			schema, _ := info.Parameters["structured_payload"].(map[string]any)
			advertiseResultEvidenceKeys(schema, compiled.evidenceKeys)
			info.Parameters["structured_payload"] = providerResultIntegerStrings(schema, 0)
			if hasResultDecisions(compiled) {
				// A failed bound decision cannot provide a successful payload.
				// Successful results still pass the full contract in Run/commit.
				info.Required = []string{"status", "summary"}
				info.Description += " For a bound-decision task that cannot settle its required decisions, submit status=blocked and summary, omitting structured_payload and any unavailable success-only fields. Success and completed_with_gaps still require the complete validated payload."
			}
		}
	}
	return info
}

func (t *submitResultTool) submissionContract() taskResultSubmissionContract {
	if t == nil {
		return localResultSubmissionContract(nil, "")
	}
	return localResultSubmissionContract(t.coordinator, t.todoID)
}

func localResultSubmissionContract(coordinator *Coordinator, todoID string) taskResultSubmissionContract {
	if coordinator == nil {
		return taskResultSubmissionContract{AllowEvidence: true, AllowArtifacts: true}
	}
	item := coordinator.todoItemByID(todoID)
	if item == nil {
		return taskResultSubmissionContract{AllowEvidence: true, AllowArtifacts: true}
	}
	contract := taskResultSubmissionContractForTask(TaskDef{
		ID:                    item.ID,
		InvariantVerification: item.InvariantVerification,
		Verify:                item.Verify,
		VerifyMode:            item.VerifyMode,
		VerifySpec:            item.VerifySpec,
		Execution:             item.Execution,
	})
	if item.ResultContract != nil {
		// A drifted contract still advertises the property; the submission
		// itself is then rejected by structuredPayloadForSubmission.
		compiled, _ := coordinator.compiledResultContract(item.ResultContract)
		contract.ResultContract = item.ResultContract.clone()
		contract.resultPayloadSchema = coordinator.fixedCollectionSubmissionSchema(todoID, *item.ResultContract, compiled)
	}
	return contract
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
					"required": []string{"choice"},
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
					"required":             []string{"type"},
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
			"assumption_checks": map[string]any{
				"type":        "array",
				"description": "Decision assumptions this task actually checked. Only report an assumption the task was given, only when you checked it, and only as supported or contradicted. Omit anything you did not verify; saying nothing is the correct answer for an unchecked assumption.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"assumption_id": map[string]any{"type": "string"},
						"status":        map[string]any{"type": "string", "enum": []string{"supported", "contradicted"}},
						"evidence":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"note":          map[string]any{"type": "string"},
					},
					"required": []string{"assumption_id", "status"}, "additionalProperties": false,
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
						"severity": map[string]any{"type": "string", "enum": []string{FindingSeverityError, FindingSeverityWarning, FindingSeverityInfo}},
					},
					"required":             []string{"summary"},
					"additionalProperties": false,
				},
			},
			"invariant_assessments": invariantAssessmentClaimsSchema(false),
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
				"description": "Unresolved questions or follow-ups. Prefer strings. Also accepts a structured question/context/detail object or exactly {\"$text\":\"nonempty text\"}; both normalize to canonical strings.",
				"items": map[string]any{
					"oneOf": []any{
						map[string]any{"type": "string"},
						map[string]any{
							"type":                 "object",
							"properties":           map[string]any{"$text": map[string]any{"type": "string", "minLength": 1}},
							"required":             []string{"$text"},
							"additionalProperties": false,
						},
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
				"description": "Canonical memory records actually applied, consulted, or rejected. Each injected record is preceded by a `<!-- hufu-context ... id=context:<id> -->` marker; report that id as context_item_id. An empty array is valid.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"retrieval_id":    map[string]any{"type": "string", "description": "Optional; the runtime binds the current attempt's retrieval when omitted."},
						"context_item_id": map[string]any{"type": "string"},
						"disposition":     map[string]any{"type": "string", "enum": []string{MemoryUseApplied, MemoryUseConsulted, MemoryUseRejected}},
						"reason_code":     map[string]any{"type": "string"},
						"confidence":      map[string]any{"type": "number", "minimum": 0.0, "maximum": 1.0},
					},
					"required":             []string{"context_item_id", "disposition", "confidence"},
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
	if contract.InvariantVerification != "" && !slices.Contains(info.Required, "invariant_assessments") {
		info.Required = append(info.Required, "invariant_assessments")
	}
	if contract.ResultContract != nil {
		info.Parameters["structured_payload"] = contract.resultPayloadSchema
		if contract.ResultContract.RequireStructured && !slices.Contains(info.Required, "structured_payload") {
			info.Required = append(info.Required, "structured_payload")
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

func (t *submitResultTool) runFull(ctx context.Context, call fantasy.ToolCall) (response fantasy.ToolResponse, runErr error) {
	reason := RepairFailureInvalidSchema
	defer func() { response = tagSubmissionFailure(response, reason) }()
	contract := t.submissionContract()
	if t.coordinator != nil {
		if _, _, err := t.coordinator.boundResultContract(t.todoID); err != nil {
			return submissionFailureResponse(RepairFailureContract, err.Error()), nil
		}
		if err := t.coordinator.validateFixedCollectionInputJSON(t.todoID, call.Input); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
	}
	call.Input = canonicalToolArgumentObjects(call.Input, t.fullInfo())
	call.Input, _ = tools.CanonicalToolArguments(call.Input, t.fullInfo())
	if t.usesSubmissionProjection() {
		if violation := validateSubmissionFieldPresence(call.Input, t.fullInfo()); violation != nil {
			return fantasy.NewTextErrorResponse(buildToolSchemaValidationPrompt(submitResultToolName, violation, t.fullInfo())), nil
		}
	}
	if t.coordinator != nil {
		normalized, err := t.coordinator.resolveResultEvidenceKeys(ctx, t.todoID, call.Input)
		if err != nil {
			return submissionFailureResponse(RepairFailureSource, "invalid evidence key: "+err.Error()), nil
		}
		call.Input = normalized
	}
	input, err := decodeSubmitResultInput([]byte(call.Input), contract)
	if err != nil {
		if strings.Contains(err.Error(), `unknown field "raw_output_ref"`) {
			return fantasy.NewTextErrorResponse("raw_output_ref is runtime-owned; workers cannot declare or copy transcript references"), nil
		}
		if strings.Contains(err.Error(), `unknown field "outputs"`) {
			return fantasy.NewTextErrorResponse("outputs are runtime-owned; cite execution receipt_ids instead of declaring task outputs"), nil
		}
		// The DTO decoder names an unknown field without its parent path.
		// Resolve that path against the normalized input so a repair can fix
		// the rejected object without removing a legal namesake elsewhere.
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			if normalized, normalizeErr := normalizeSubmitResultInput([]byte(call.Input)); normalizeErr == nil {
				validationInfo := t.fullInfo()
				validationInfo.Required = nil
				if validationErr := validateToolArguments(string(normalized), validationInfo); validationErr != nil {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid submit_result arguments: %v. %s", err, buildToolSchemaValidationPrompt(submitResultToolName, validationErr, t.fullInfo()))), nil
				}
			}
		}
		info := t.fullInfo()
		violation := validateToolArguments(call.Input, info)
		if violation == nil {
			violation = &toolArgumentSchemaError{Path: "$", Expected: "complete tool argument object", Actual: "invalid arguments"}
		}
		if violation.schema == nil {
			violation.schema, violation.schemaPath = map[string]any{"type": "object", "properties": info.Parameters}, "$"
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid submit_result arguments: %v. %s", err, buildToolSchemaValidationPrompt(submitResultToolName, violation, info))), nil
	}
	validationInfo := t.fullInfo()
	// Top-level presence has task-specific semantic handling below (including
	// stable result-contract error codes and runtime defaults). Validate the
	// decoder's canonical DTO here so losslessly normalized shorthand remains
	// accepted while nested object schemas still reject missing/extra fields.
	validationInfo.Required = nil
	canonicalInput, marshalErr := json.Marshal(input)
	if marshalErr != nil {
		return fantasy.NewTextErrorResponse("invalid submit_result arguments: canonicalize decoded input"), nil
	}
	if validationErr := validateToolArguments(string(canonicalInput), validationInfo); validationErr != nil {
		return fantasy.NewTextErrorResponse(buildToolSchemaValidationPrompt(submitResultToolName, validationErr, t.fullInfo())), nil
	}
	admission := localResultAdmission{coordinator: t.coordinator, todoID: t.todoID, sink: t.sink}
	err = admission.admit(ctx, input, contract, func() RepairFailureReason {
		return t.payloadRejectionReason(call.Input)
	})
	if err != nil {
		return submissionFailureResponse(localResultRejectionReason(err), err.Error()), nil
	}
	return fantasy.NewTextResponse("Task result submitted successfully."), nil
}

// normalizeSubmitResultInput preserves the typed result contract while being
// tolerant of common model shorthand for descriptive arrays. Models often
// collapse a one-entry array to a scalar or use an array of strings where the
// schema expects objects. These fields are result evidence, not mutation
// instructions, so the scalar forms can be losslessly promoted instead of
// turning an otherwise valid terminal result into a schema-repair loop.
//
// A string that holds serialized JSON is not that shorthand: the model
// encoded the array or object twice. It is decoded when it parses and
// rejected when it does not. Promoting it to a single summary hid every
// severity inside one string, so a blocker read as a finding with none.
func normalizeSubmitResultInput(input []byte) ([]byte, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(input, &object); err != nil {
		return input, nil
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
		decoded, serialized, err := decodeSerializedJSONText(field, raw)
		if err != nil {
			return nil, err
		}
		if serialized {
			if jsonObject(decoded) {
				// One serialized entry: the same one-entry collapse as a
				// scalar string, so it becomes a one-entry array.
				decoded = json.RawMessage("[" + string(decoded) + "]")
			}
			raw = decoded
			object[field] = decoded
		}
		entries, ok := normalizedStringEntries(raw)
		if !ok {
			continue
		}
		normalized := make([]json.RawMessage, 0, len(entries))
		changed := false
		for _, entry := range entries {
			decoded, serialized, err := decodeSerializedJSONText(field, entry)
			if err != nil {
				return nil, err
			}
			if serialized {
				if !jsonObject(decoded) {
					return nil, fmt.Errorf("%s entry holds serialized JSON that is not an object; send each %s entry as a JSON object", field, field)
				}
				normalized = append(normalized, decoded)
				changed = true
				continue
			}
			var value string
			if err := json.Unmarshal(entry, &value); err != nil || strings.TrimSpace(value) == "" {
				normalized = append(normalized, entry)
				continue
			}
			ref, err := json.Marshal(map[string]string{key: value})
			if err != nil {
				return input, nil
			}
			normalized = append(normalized, ref)
			changed = true
		}
		if changed {
			encoded, err := json.Marshal(normalized)
			if err != nil {
				return input, nil
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
		return input, nil
	}
	return normalized, nil
}

// decodeSerializedJSONText reports whether raw is a JSON string whose text is
// itself a JSON object or array, and returns that inner value. Only text that
// opens like JSON is considered, so prose such as "[security] ..." stays a
// summary. Text that opens like JSON but does not parse is an error.
func decodeSerializedJSONText(field string, raw json.RawMessage) (json.RawMessage, bool, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil || !opensLikeJSON(text) {
		return nil, false, nil
	}
	var inner json.RawMessage
	if err := json.Unmarshal([]byte(text), &inner); err != nil {
		return nil, false, fmt.Errorf("%s was sent as a string holding malformed JSON (%v); send %s as a JSON array of objects, not as a string", field, err, field)
	}
	return inner, true, nil
}

// opensLikeJSON is true for text that starts like a serialized object or
// array: `{"`, `{}`, or `[` followed by an object, string, array, or `]`.
func opensLikeJSON(text string) bool {
	text = strings.TrimSpace(text)
	if len(text) < 2 || (text[0] != '{' && text[0] != '[') {
		return false
	}
	next := strings.TrimLeft(text[1:], " \t\r\n")
	if next == "" {
		return false
	}
	if text[0] == '{' {
		return next[0] == '"' || next[0] == '}'
	}
	return strings.ContainsRune(`{"[]`, rune(next[0]))
}

func jsonObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
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
	RunID              string
	TaskID             string
	Attempt            int
	Agent              string
	OccurrenceRevision int
	DispatchID         string
}

func (c *Coordinator) stageSubmittedArtifacts(identity submitResultRuntimeIdentity, refs []ArtifactRef) {
	if c == nil || len(refs) == 0 {
		return
	}
	identity = c.completeOccurrenceLease(identity)
	if !validSubmitResultIdentity(identity) {
		return
	}
	controller := c.occurrenceController(identity.TaskID)
	controller.mu.Lock()
	if controller.opened && sameTaskResultOccurrence(controller.identity, identity) && !controller.reserved && controller.result == nil {
		controller.pending = append([]ArtifactRef(nil), refs...)
	}
	controller.mu.Unlock()
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
