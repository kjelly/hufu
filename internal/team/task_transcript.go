package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/anomalyco/hufu/internal/utils"
)

const (
	taskTranscriptMediaType = "application/x-ndjson"
	taskTranscriptDir       = "task-output"
)

const (
	TaskOutputModeSummary  = "summary"
	TaskOutputModeVerbatim = "verbatim"
)

func taskUsesVerbatimTranscript(task TaskDef) bool {
	return task.OutputMode == TaskOutputModeVerbatim
}

func validateTaskOutputMode(task TaskDef) error {
	switch task.OutputMode {
	case "", TaskOutputModeSummary:
		return nil
	case TaskOutputModeVerbatim:
		if task.Sidecar {
			return fmt.Errorf("output_mode=verbatim is not supported for sidecar tasks because they have no tool transcript")
		}
		if task.Summarize {
			return fmt.Errorf("output_mode=verbatim cannot be combined with summarize")
		}
		if task.AdversarialVerify > 0 {
			return fmt.Errorf("output_mode=verbatim cannot be combined with adversarial_verify because verification would send task output to a sidecar")
		}
		return nil
	default:
		return fmt.Errorf("invalid output_mode %q; expected summary or verbatim", task.OutputMode)
	}
}

// taskTranscript is a runner-owned, append-only record of a verbatim task's
// tool activity. It deliberately sits below the model: workers cannot replace
// command output with a prose summary.
type taskTranscript struct {
	mu              sync.Mutex
	path            string
	todoID          string
	runID           string
	f               *os.File
	toolResults     int
	assistantOutput bool
}

type taskTranscriptRecord struct {
	Timestamp  string `json:"timestamp"`
	Event      string `json:"event"`
	ToolCallID string `json:"tool_call_id"`
	Tool       string `json:"tool"`
	Input      string `json:"input,omitempty"`
	Output     string `json:"output,omitempty"`
	Error      bool   `json:"error,omitempty"`
}

func newTaskTranscript(workspace, todoID, runID string) (*taskTranscript, error) {
	return newTaskTranscriptForAttempt(workspace, todoID, runID, 0)
}

// newTaskTranscriptForAttempt creates a distinct runner-owned transcript for
// one execution attempt. Attempt transcripts are never truncated by retries
// or by repair; the receipt can therefore identify the exact original
// execution that produced its evidence.
func newTaskTranscriptForAttempt(workspace, todoID, runID string, attempt int) (*taskTranscript, error) {
	if workspace == "" {
		return nil, fmt.Errorf("create task transcript: empty workspace")
	}
	if todoID == "" {
		return nil, fmt.Errorf("create task transcript: empty todo ID")
	}
	dir := filepath.Join(workspace, logsDir, taskTranscriptDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create task transcript directory: %w", err)
	}
	name := todoID + ".jsonl"
	if attempt > 0 && runID != "" {
		name = fmt.Sprintf("%s-%s", todoID, runID)
		if attempt > 0 {
			name += fmt.Sprintf("-attempt-%d", attempt)
		}
		name += ".jsonl"
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create task transcript: %w", err)
	}
	return &taskTranscript{path: path, todoID: todoID, runID: runID, f: f}, nil
}

// RecordAssistantOutput preserves the original worker's final response in
// the same immutable attempt transcript. It is deliberately separate from
// tool_result so a task that used no tools still has durable execution
// evidence for protocol repair and reconciliation.
func (t *taskTranscript) RecordAssistantOutput(output string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return fmt.Errorf("record task transcript: closed")
	}
	record := taskTranscriptRecord{
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Event:     "assistant_output",
		Output:    utils.RedactSecrets(output),
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode task transcript: %w", err)
	}
	if _, err := t.f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write task transcript: %w", err)
	}
	t.assistantOutput = true
	return nil
}

func (t *taskTranscript) RecordToolCall(id, tool, input string) error {
	return t.append(taskTranscriptRecord{Timestamp: time.Now().Format(time.RFC3339Nano), Event: "tool_call", ToolCallID: id, Tool: tool, Input: input})
}

func (t *taskTranscript) RecordToolResult(id, tool, output string, isError bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return fmt.Errorf("record task transcript: closed")
	}
	record := taskTranscriptRecord{Timestamp: time.Now().Format(time.RFC3339Nano), Event: "tool_result", ToolCallID: id, Tool: tool, Output: utils.RedactSecrets(output), Error: isError}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode task transcript: %w", err)
	}
	if _, err := t.f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write task transcript: %w", err)
	}
	t.toolResults++
	return nil
}

func (t *taskTranscript) append(record taskTranscriptRecord) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return fmt.Errorf("record task transcript: closed")
	}
	record.Input = utils.RedactSecrets(record.Input)
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode task transcript: %w", err)
	}
	if _, err := t.f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write task transcript: %w", err)
	}
	return nil
}

func (t *taskTranscript) Manifest() (*ArtifactRef, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return nil, fmt.Errorf("create task transcript manifest: closed")
	}
	if t.toolResults == 0 && !t.assistantOutput {
		return nil, fmt.Errorf("task produced no tool results or assistant output")
	}
	if err := t.f.Sync(); err != nil {
		return nil, fmt.Errorf("sync task transcript: %w", err)
	}
	f, err := os.Open(t.path)
	if err != nil {
		return nil, fmt.Errorf("read task transcript: %w", err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	bytes, err := io.Copy(h, f)
	if err != nil {
		return nil, fmt.Errorf("hash task transcript: %w", err)
	}
	return &ArtifactRef{Path: t.path, Description: "Complete tool-call transcript captured by hufu", Type: taskTranscriptMediaType, SHA256: hex.EncodeToString(h.Sum(nil)), Bytes: bytes}, nil
}

func (t *taskTranscript) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return nil
	}
	err := t.f.Close()
	t.f = nil
	return err
}

func formatVerbatimTranscriptManifest(ref *ArtifactRef) string {
	if ref == nil {
		return ""
	}
	return fmt.Sprintf("VERBATIM TRANSCRIPT CAPTURED\npath=%s\nsha256=%s\nbytes=%d\n\nThe transcript is authoritative. Do not re-read source files merely to reconstruct its contents.", ref.Path, ref.SHA256, ref.Bytes)
}

// finalizeVerbatimTaskResult associates runner-owned evidence with the typed
// result and returns the only text safe to pass back into coordinator context.
func finalizeVerbatimTaskResult(transcript *taskTranscript, result *TaskResult) (string, error) {
	if transcript == nil {
		return "", fmt.Errorf("verbatim task transcript was not initialized")
	}
	ref, err := transcript.Manifest()
	if err != nil {
		return "", err
	}
	if result != nil {
		result.RawOutputRef = ref
		sec, err := GetSystemSecret()
		if err != nil {
			return "", fmt.Errorf("failed to obtain system secret for transcript signing: %w", err)
		}
		ev := EvidenceRef{
			TaskID:      transcript.todoID,
			RunID:       transcript.runID,
			Type:        "task_transcript",
			Description: "Complete runner-captured tool transcript",
			Value:       ref.Path,
		}
		result.Evidence = append(result.Evidence, SignEvidence(ev, sec))
	}
	return formatVerbatimTranscriptManifest(ref), nil
}
