package team

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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

type TaskResult struct {
	TaskID             string               `json:"task_id,omitempty"`
	Agent              string               `json:"agent,omitempty"`
	Status             string               `json:"status,omitempty"`
	Summary            string               `json:"summary"`
	Artifacts          []ArtifactRef        `json:"artifacts,omitempty"`
	Evidence           []EvidenceRef        `json:"evidence,omitempty"`
	FilesRead          []FileRef            `json:"files_read,omitempty"`
	FilesModified      []FileRef            `json:"files_modified,omitempty"`
	Commands           []CommandResult      `json:"commands,omitempty"`
	Verification       []VerificationResult `json:"verification,omitempty"`
	Decisions          []Decision           `json:"decisions,omitempty"`
	Findings           []Finding            `json:"findings,omitempty"`
	Risks              []Risk               `json:"risks,omitempty"`
	OpenQuestions      []string             `json:"open_questions,omitempty"`
	SuggestedNextTasks []TaskProposal       `json:"suggested_next_tasks,omitempty"`
	RetryHint          string               `json:"retry_hint,omitempty"`
	RawOutputRef       *ArtifactRef         `json:"raw_output_ref,omitempty"`

	Confidence float64 `json:"confidence"`
	Source     string  `json:"source"` // "submitted" or "parsed_free_text"
}

// ParseFreeTextResult constructs a TaskResult from unstructured text output when
// submit_result tool was not invoked by the agent.
func ParseFreeTextResult(text string) *TaskResult {
	return &TaskResult{
		Summary:    strings.TrimSpace(text),
		Confidence: 0.4,
		Source:     "parsed_free_text",
	}
}

// validateSubmittedTaskResult makes the worker's terminal state explicit.
// A non-success result is useful evidence, but must flow through retries and
// error handling rather than silently becoming a completed Todo item.
func validateSubmittedTaskResult(result *TaskResult) error {
	if result == nil {
		return fmt.Errorf("missing structured task result")
	}
	switch result.Status {
	case "success":
		return nil
	case "partial", "failed", "blocked":
		return fmt.Errorf("worker reported task status %q: %s", result.Status, strings.TrimSpace(result.Summary))
	default:
		return fmt.Errorf("invalid task result status %q; expected success, partial, failed, or blocked", result.Status)
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
		fmt.Fprintf(&sb, "Verbatim Transcript: %s (sha256: %s, bytes: %d)\n", tr.RawOutputRef.Path, tr.RawOutputRef.SHA256, tr.RawOutputRef.Bytes)
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
