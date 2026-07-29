package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskTranscriptCapturesCompleteToolEvidenceAndBuildsManifest(t *testing.T) {
	workspace := t.TempDir()
	transcript, err := newTaskTranscript(workspace, "17", "run-17")
	if err != nil {
		t.Fatalf("newTaskTranscript() error = %v", err)
	}
	t.Cleanup(func() { _ = transcript.Close() })

	if err := transcript.RecordToolCall("call-1", "bash", `{"command":"printf hello"}`); err != nil {
		t.Fatalf("RecordToolCall() error = %v", err)
	}
	if err := transcript.RecordToolResult("call-1", "bash", "hello\nworld\n", false); err != nil {
		t.Fatalf("RecordToolResult() error = %v", err)
	}

	ref, err := transcript.Manifest()
	if err != nil {
		t.Fatalf("Manifest() error = %v", err)
	}
	if ref.Path != filepath.Join(workspace, "logs", "task-output", "17.jsonl") {
		t.Errorf("manifest path = %q", ref.Path)
	}
	if ref.Type != taskTranscriptMediaType {
		t.Errorf("manifest type = %q, want %q", ref.Type, taskTranscriptMediaType)
	}
	if ref.SHA256 == "" || ref.Bytes == 0 {
		t.Errorf("manifest = %#v, want checksum and non-zero bytes", ref)
	}

	data, err := os.ReadFile(ref.Path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", ref.Path, err)
	}
	for _, want := range []string{`"event":"tool_call"`, `"event":"tool_result"`, `printf hello`, "hello\\nworld\\n"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("transcript missing %q:\n%s", want, data)
		}
	}

	manifest := formatVerbatimTranscriptManifest(ref)
	for _, want := range []string{"VERBATIM TRANSCRIPT CAPTURED", ref.Path, ref.SHA256, "bytes="} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest missing %q:\n%s", want, manifest)
		}
	}
}

func TestTaskTranscriptRequiresAtLeastOneToolResult(t *testing.T) {
	transcript, err := newTaskTranscript(t.TempDir(), "18", "run-18")
	if err != nil {
		t.Fatalf("newTaskTranscript() error = %v", err)
	}
	t.Cleanup(func() { _ = transcript.Close() })

	if _, err := transcript.Manifest(); err == nil {
		t.Fatal("Manifest() error = nil, want missing tool result error")
	}
}

func TestVerbatimTaskResultUsesTranscriptManifestInsteadOfWorkerSummary(t *testing.T) {
	workspace := t.TempDir()
	transcript, err := newTaskTranscript(workspace, "19", "run-19")
	if err != nil {
		t.Fatalf("newTaskTranscript() error = %v", err)
	}
	t.Cleanup(func() { _ = transcript.Close() })
	if err := transcript.RecordToolResult("call-2", "bash", "complete raw output", false); err != nil {
		t.Fatalf("RecordToolResult() error = %v", err)
	}

	result := &TaskResult{Status: "success", Summary: "worker prose summary"}
	output, err := finalizeVerbatimTaskResult(transcript, result)
	if err != nil {
		t.Fatalf("finalizeVerbatimTaskResult() error = %v", err)
	}
	if result.RawOutputRef == nil {
		t.Fatal("RawOutputRef is nil")
	}
	if strings.Contains(output, "worker prose summary") {
		t.Errorf("coordinator output included worker summary:\n%s", output)
	}
	if !strings.Contains(output, result.RawOutputRef.Path) {
		t.Errorf("coordinator output omitted transcript path:\n%s", output)
	}
}

func TestValidateTaskOutputModeRejectsUnknownValue(t *testing.T) {
	err := validateTaskOutputMode(TaskDef{OutputMode: "all-the-things"})
	if err == nil {
		t.Fatal("validateTaskOutputMode() error = nil, want invalid output mode")
	}
}

func TestValidateTaskOutputModeRejectsIncompatibleVerbatimOptions(t *testing.T) {
	for _, task := range []TaskDef{
		{OutputMode: TaskOutputModeVerbatim, Sidecar: true},
		{OutputMode: TaskOutputModeVerbatim, Summarize: true},
		{OutputMode: TaskOutputModeVerbatim, AdversarialVerify: 1},
	} {
		if err := validateTaskOutputMode(task); err == nil {
			t.Fatalf("validateTaskOutputMode(%+v) error = nil, want incompatible option error", task)
		}
	}
}
