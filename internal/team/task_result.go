package team

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

type ArtifactRef struct {
	ID          string    `json:"id,omitempty"`
	Kind        string    `json:"kind,omitempty"`
	Path        string    `json:"path"`
	Description string    `json:"description,omitempty"`
	Type        string    `json:"type,omitempty"`
	SHA256      string    `json:"sha256,omitempty"`
	Bytes       int64     `json:"bytes,omitempty"`
	ByteSize    int64     `json:"byte_size,omitempty"`
	MediaType   string    `json:"media_type,omitempty"`
	RunID       string    `json:"run_id,omitempty"`
	TaskID      string    `json:"task_id,omitempty"`
	Attempt     int       `json:"attempt,omitempty"`
	Agent       string    `json:"agent,omitempty"`
	ToolCallID  string    `json:"tool_call_id,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
}

type FileRef struct {
	Path    string `json:"path"`
	Purpose string `json:"purpose,omitempty"`
}

type EvidenceRef struct {
	TaskID      string `json:"task_id,omitempty"`
	RunID       string `json:"run_id,omitempty"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Value       string `json:"value,omitempty"`
	SystemHMAC  string `json:"system_hmac,omitempty"`
}

var (
	systemSecretOnce sync.Once
	systemSecretKey  string
	systemSecretErr  error
	randReader       io.Reader = rand.Reader
)

// GetSystemSecret returns a process-isolated, cryptographically random HMAC secret key.
// It fails closed if cryptographic randomness is unavailable, and unsets HUFU_HMAC_SECRET to prevent subprocess inheritance.
func GetSystemSecret() (string, error) {
	systemSecretOnce.Do(func() {
		// Immediately scrub HUFU_HMAC_SECRET from environment so subprocesses cannot inherit it
		_ = os.Unsetenv("HUFU_HMAC_SECRET")

		r := randReader
		if r == nil {
			r = rand.Reader
		}
		b := make([]byte, 32)
		if _, err := io.ReadFull(r, b); err != nil {
			systemSecretErr = fmt.Errorf("failed to generate cryptographically secure secret: %w", err)
			return
		}
		systemSecretKey = hex.EncodeToString(b)
	})
	if systemSecretErr != nil {
		return "", systemSecretErr
	}
	return systemSecretKey, nil
}

func ComputeEvidenceHMAC(ref EvidenceRef, secret string) (string, error) {
	if secret == "" {
		sec, err := GetSystemSecret()
		if err != nil {
			return "", err
		}
		secret = sec
	}
	h := hmac.New(sha256.New, []byte(secret))
	// Bind task_id, run_id, type, description, and value to prevent replay attacks across tasks or runs
	h.Write([]byte(ref.TaskID + ":" + ref.RunID + ":" + ref.Type + ":" + ref.Description + ":" + ref.Value))
	return hex.EncodeToString(h.Sum(nil)), nil
}

func SignEvidence(ref EvidenceRef, secret string) EvidenceRef {
	sig, err := ComputeEvidenceHMAC(ref, secret)
	if err != nil {
		ref.SystemHMAC = ""
		return ref
	}
	ref.SystemHMAC = sig
	return ref
}

func VerifyEvidenceSignature(ref EvidenceRef, secret string, expectedTaskID string, expectedRunID string) bool {
	if ref.SystemHMAC == "" || ref.TaskID == "" || ref.RunID == "" {
		return false
	}
	if expectedTaskID != "" && ref.TaskID != expectedTaskID {
		return false // Task ID mismatch -> replay attack prevention
	}
	if expectedRunID != "" && ref.RunID != expectedRunID {
		return false // Run ID mismatch -> replay attack prevention
	}
	expected, err := ComputeEvidenceHMAC(ref, secret)
	if err != nil || expected == "" {
		return false
	}
	return hmac.Equal([]byte(ref.SystemHMAC), []byte(expected))
}

type CommandResult struct {
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output,omitempty"`
}

type Decision struct {
	Topic  string `json:"topic"`
	Choice string `json:"choice"`
	Reason string `json:"reason,omitempty"`
}

type Finding struct {
	Category string `json:"category"`
	Summary  string `json:"summary"`
	Detail   string `json:"detail,omitempty"`
}

type Risk struct {
	Description string `json:"description"`
	Impact      string `json:"impact,omitempty"`
	Mitigation  string `json:"mitigation,omitempty"`
}

type TaskProposal struct {
	Goal   string `json:"goal"`
	Agent  string `json:"agent,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type MemoryUseRef struct {
	RetrievalID   string  `json:"retrieval_id"`
	ContextItemID string  `json:"context_item_id"`
	Disposition   string  `json:"disposition"`
	ReasonCode    string  `json:"reason_code,omitempty"`
	Confidence    float64 `json:"confidence"`
}

const (
	MemoryUseApplied   = "applied"
	MemoryUseConsulted = "consulted"
	MemoryUseRejected  = "rejected"
)

// OpenQuestions is the canonical textual handoff form for unresolved work.
// Models naturally produce either terse strings or structured question
// objects. Accept the documented structured form at the typed-result boundary
// and normalize it into durable text, while rejecting arbitrary objects that
// could otherwise silently change the task-result contract.
type OpenQuestions []string

func (questions *OpenQuestions) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*questions = nil
		return nil
	}

	entries := []json.RawMessage{data}
	var array []json.RawMessage
	if err := json.Unmarshal(data, &array); err == nil {
		entries = array
	}

	normalized := make([]string, 0, len(entries))
	for _, entry := range entries {
		question, err := normalizeOpenQuestion(entry)
		if err != nil {
			return err
		}
		normalized = append(normalized, question)
	}
	*questions = normalized
	return nil
}

func normalizeOpenQuestion(data json.RawMessage) (string, error) {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		if text = strings.TrimSpace(text); text != "" {
			return text, nil
		}
		return "", fmt.Errorf("open_questions entries must not be empty")
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return "", fmt.Errorf("open_questions entries must be strings or objects with question, context?, and detail?")
	}
	for key := range object {
		if key != "question" && key != "context" && key != "detail" {
			return "", fmt.Errorf("open_questions object contains unsupported field %q", key)
		}
	}

	read := func(key string, required bool) (string, error) {
		raw, ok := object[key]
		if !ok {
			if required {
				return "", fmt.Errorf("open_questions object requires %q", key)
			}
			return "", nil
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", fmt.Errorf("open_questions.%s must be a string", key)
		}
		value = strings.TrimSpace(value)
		if required && value == "" {
			return "", fmt.Errorf("open_questions.%s must not be empty", key)
		}
		return value, nil
	}

	question, err := read("question", true)
	if err != nil {
		return "", err
	}
	context, err := read("context", false)
	if err != nil {
		return "", err
	}
	detail, err := read("detail", false)
	if err != nil {
		return "", err
	}
	if context != "" {
		question += "\nContext: " + context
	}
	if detail != "" {
		question += "\nDetail: " + detail
	}
	return question, nil
}

type TaskResult struct {
	TaskID  string `json:"task_id,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Status  string `json:"status,omitempty"`
	Summary string `json:"summary"`
	// Details is the complete textual deliverable when a task produces a plan,
	// analysis, review, or other handoff that does not need a separate file.
	// It is part of the typed result, so downstream coordinators can consume it
	// without asking a successful worker to write or repeat a report.
	Details            string                           `json:"details,omitempty"`
	Artifacts          []ArtifactRef                    `json:"artifacts,omitempty"`
	Evidence           []EvidenceRef                    `json:"evidence,omitempty"`
	FilesRead          []FileRef                        `json:"files_read,omitempty"`
	FilesModified      []FileRef                        `json:"files_modified,omitempty"`
	Commands           []CommandResult                  `json:"commands,omitempty"`
	Verification       []VerificationResult             `json:"verification,omitempty"`
	Decisions          []Decision                       `json:"decisions,omitempty"`
	Findings           []Finding                        `json:"findings,omitempty"`
	Risks              []Risk                           `json:"risks,omitempty"`
	OpenQuestions      OpenQuestions                    `json:"open_questions,omitempty"`
	SuggestedNextTasks []TaskProposal                   `json:"suggested_next_tasks,omitempty"`
	RetryHint          string                           `json:"retry_hint,omitempty"`
	RawOutputRef       *ArtifactRef                     `json:"raw_output_ref,omitempty"`
	ReceiptIDs         []string                         `json:"receipt_ids,omitempty"`
	Outputs            map[string]StructuredOutputValue `json:"outputs,omitempty"`
	MemoryUses         []MemoryUseRef                   `json:"memory_uses,omitempty"`
	// Facts are named JSON values a plain (non-steps) task result declares for
	// a later flat task to reference by name via TaskDef.FactRefs, instead of
	// a coordinator retyping this task's own discovered value (a list, a
	// computed count, a resolved path) into a later dispatch's goal text.
	// This is deliberately separate from the steps-DAG's Outputs/StructuredFact
	// machinery (which needs schema/SHA256/scope for its own repair and
	// receipt semantics): an ordinary worker just names a value.
	Facts map[string]any `json:"facts,omitempty"`

	Confidence float64 `json:"confidence"`
	Source     string  `json:"source"` // "submitted", "promoted_free_text", or "parsed_free_text"
}

const (
	// TaskResultStatusSuccess means the assigned task reached its intended
	// outcome without a known external limitation.
	TaskResultStatusSuccess = "success"
	// TaskResultStatusCompletedWithGaps means the assigned task itself is
	// complete and its evidence is usable, but it discovered a limitation in
	// the target system or an unmet prerequisite. The limitation belongs in
	// Summary, Findings, Risks, or OpenQuestions; it does not make completed
	// discovery or analysis work incomplete.
	TaskResultStatusCompletedWithGaps = "completed_with_gaps"
	TaskResultStatusPartial           = "partial"
	TaskResultStatusFailed            = "failed"
	TaskResultStatusBlocked           = "blocked"
)

// ParseFreeTextResult constructs a TaskResult from unstructured text output when
// submit_result tool was not invoked by the agent.
func ParseFreeTextResult(text string) *TaskResult {
	return &TaskResult{
		Summary:    strings.TrimSpace(text),
		Confidence: 0.4,
		Source:     "parsed_free_text",
	}
}

// validateSubmittedTaskResult validates the submitted-result schema boundary.
// partial, failed, and blocked are honest progress/failure reports, not
// malformed protocol calls.
func validateSubmittedTaskResult(result *TaskResult) error {
	if result == nil {
		return fmt.Errorf("missing structured task result")
	}
	switch result.Status {
	case TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps,
		TaskResultStatusPartial, TaskResultStatusFailed, TaskResultStatusBlocked:
		return nil
	default:
		return fmt.Errorf("invalid task result status %q; expected success, completed_with_gaps, partial, failed, or blocked", result.Status)
	}
}


// validateCompletedTaskResult validates a structured result for terminal Todo
// completion. It deliberately keeps schema validity separate from semantics:
// an honest partial/failed/blocked result is retained as evidence and routed
// through the task's recovery policy rather than treated as a protocol error.
func validateCompletedTaskResult(result *TaskResult) error {
	if err := validateSubmittedTaskResult(result); err != nil {
		return err
	}
	switch result.Status {
	case TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps:
		return nil
	default:
		return fmt.Errorf("worker reported incomplete task status %q: %s", result.Status, strings.TrimSpace(result.Summary))
	}
}

// FormatForContext formats the typed result into a human-readable string suitable
// for passing as context to downstream agents or dependencies.
func (tr *TaskResult) FormatForContext() string {
	if tr == nil {
		return ""
	}
	var sb strings.Builder
	if tr.Summary != "" {
		sb.WriteString("Summary: " + tr.Summary + "\n")
	}
	if tr.Details != "" {
		sb.WriteString("Detailed Deliverable:\n" + tr.Details + "\n")
	}
	if tr.Source != "" {
		fmt.Fprintf(&sb, "Result Source: %s (confidence: %.2f)\n", tr.Source, tr.Confidence)
	}
	if len(tr.Artifacts) > 0 {
		sb.WriteString("Artifacts:\n")
		for _, a := range tr.Artifacts {
			if a.Description != "" {
				fmt.Fprintf(&sb, "  - %s (%s)\n", a.Path, a.Description)
			} else {
				fmt.Fprintf(&sb, "  - %s\n", a.Path)
			}
		}
	}
	if tr.RawOutputRef != nil {
		if tr.RawOutputRef.ID != "" {
			fmt.Fprintf(&sb, "Verbatim Transcript Ref: %s (sha256: %s, bytes: %d)\n", tr.RawOutputRef.ID, tr.RawOutputRef.SHA256, tr.RawOutputRef.Bytes)
		} else {
			fmt.Fprintf(&sb, "Legacy Verbatim Transcript: %s (sha256: %s, bytes: %d)\n", tr.RawOutputRef.Path, tr.RawOutputRef.SHA256, tr.RawOutputRef.Bytes)
		}
	}
	if len(tr.FilesModified) > 0 {
		sb.WriteString("Files Modified:\n")
		for _, f := range tr.FilesModified {
			fmt.Fprintf(&sb, "  - %s\n", f.Path)
		}
	}
	if len(tr.Findings) > 0 {
		sb.WriteString("Findings:\n")
		for _, f := range tr.Findings {
			fmt.Fprintf(&sb, "  - [%s] %s\n", f.Category, f.Summary)
			if f.Detail != "" {
				fmt.Fprintf(&sb, "    %s\n", f.Detail)
			}
		}
	}
	if len(tr.Decisions) > 0 {
		sb.WriteString("Decisions:\n")
		for _, d := range tr.Decisions {
			fmt.Fprintf(&sb, "  - %s: %s\n", d.Topic, d.Choice)
		}
	}
	return strings.TrimSpace(sb.String())
}

// coordinatorTaskOutput makes the submitted typed result, rather than an
// optional post-tool prose response, the authoritative worker handoff. A
// worker may acknowledge submit_result in prose or emit nothing after it;
// neither should hide the data the coordinator needs to continue safely.
func coordinatorTaskOutput(fallback string, result *TaskResult) string {
	if result != nil && result.Source == "submitted" && strings.TrimSpace(result.Details) != "" {
		if formatted := result.FormatForContext(); formatted != "" {
			return formatted
		}
	}
	return fallback
}
