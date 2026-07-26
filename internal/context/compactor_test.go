package context

import (
	"context"
	"strings"
	"testing"
	"time"
)

func compactionItem(id string, kind ContextKind, content string, priority Priority) ContextItem {
	return ContextItem{ID: id, Kind: kind, Content: content, Scope: Scope{ProjectID: "p"}, Priority: priority}
}

func TestDeterministicCompactorPreservesMidstreamErrorAndToolPair(t *testing.T) {
	large := strings.Repeat("normal output\n", 4_000) + "COMPILER_ERROR E1234 at internal/team/run.go exit status 1\n" + strings.Repeat("normal output\n", 4_000)
	items, edges := ToolEvidenceItems(
		ToolCallEvidence{ID: "call-1", Tool: "bash", Command: "go test ./...", Scope: Scope{ProjectID: "p"}},
		ToolResultEvidence{ID: "result-1", ToolCallID: "call-1", Output: large, Scope: Scope{ProjectID: "p"}},
	)
	result, err := (DeterministicCompactor{}).Compact(context.Background(), CompactionRequest{Items: items, Edges: edges, TargetTokens: 30_000})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "COMPILER_ERROR E1234") {
		t.Fatalf("middle error was lost")
	}
	if got := strings.Join(result.PreservedItemIDs, ","); !strings.Contains(got, "call-1") || !strings.Contains(got, "result-1") {
		t.Fatalf("tool pair not preserved: %v", result.PreservedItemIDs)
	}
}

func TestCompactorDoesNotReverseFailedVerification(t *testing.T) {
	items := []ContextItem{
		compactionItem("verify-fail", ContextVerification, "FAIL: go test ./... exit status 1", PriorityCritical),
		compactionItem("verify-pass", ContextVerification, "PASS: go test ./...", PriorityHigh),
		compactionItem("retry-1", ContextToolCall, "tool: bash\ncommand: go test ./...", PriorityHigh),
		compactionItem("retry-2", ContextToolCall, "tool: bash\ncommand: go test ./...", PriorityHigh),
	}
	result, err := (DeterministicCompactor{}).Compact(context.Background(), CompactionRequest{Items: items, TargetTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "FAIL") {
		t.Fatalf("failed verification missing: %q", result.Content)
	}
	if !strings.Contains(result.Content, "PASS") {
		t.Fatalf("successful verification missing: %q", result.Content)
	}
}

func TestCompactorFailsRatherThanTruncatingRequiredEvidence(t *testing.T) {
	item := compactionItem("must", ContextError, strings.Repeat("E500 important failure ", 100), PriorityCritical)
	_, err := (DeterministicCompactor{}).Compact(context.Background(), CompactionRequest{Items: []ContextItem{item}, TargetTokens: 10})
	if err != ErrCompactionBudget {
		t.Fatalf("error = %v, want ErrCompactionBudget", err)
	}
}

type invalidLLMCompactor struct{}

func (invalidLLMCompactor) Compact(context.Context, CompactionRequest) (CompactionResult, error) {
	return CompactionResult{Content: "looks good", OutputTokens: 2}, nil
}

func TestValidatedCompactorFallsBackWhenLLMDoesNotReturnStructuredIDs(t *testing.T) {
	item := compactionItem("must", ContextRequirement, "keep this requirement", PriorityCritical)
	result, err := (ValidatedCompactor{Delegate: invalidLLMCompactor{}}).Compact(context.Background(), CompactionRequest{Items: []ContextItem{item}, TargetTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !result.UsedFallback || !strings.Contains(result.Strategy, "fallback") {
		t.Fatalf("expected deterministic fallback, got %#v", result)
	}
}

func TestToolEvidenceItemsAssignIDsAndPairResult(t *testing.T) {
	items, edges := ToolEvidenceItems(ToolCallEvidence{Tool: "bash", Command: "go test", Scope: Scope{ProjectID: "p"}}, ToolResultEvidence{Output: "ok", Scope: Scope{ProjectID: "p"}})
	if len(items) != 2 || items[0].ID == "" || items[1].ID == "" || len(edges) != 1 || edges[0].ToID != items[0].ID {
		t.Fatalf("invalid generated evidence: items=%#v edges=%#v", items, edges)
	}
}

func TestToolEvidenceItemsRenderRequiredMetadata(t *testing.T) {
	exitCode := 1
	started := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	ended := started.Add(time.Second)
	items, _ := ToolEvidenceItems(ToolCallEvidence{ID: "call", Tool: "bash", Command: "go test ./...", WorkingDir: "/repo", StartedAt: started, Scope: Scope{ProjectID: "p"}}, ToolResultEvidence{ID: "result", ToolCallID: "call", ExitCode: &exitCode, EndedAt: ended, StderrSummary: "compile failed", Stdout: "output", StdoutHead: "out", StdoutTail: "put", MatchedErrors: []string{"E1234"}, ArtifactPaths: []string{"out/log.txt"}, ModifiedFiles: []string{"internal/a.go"}, Verification: "failed", Scope: Scope{ProjectID: "p"}})
	content := items[1].Content
	for _, want := range []string{"working_dir: /repo", "exit_code: 1", "started_at:", "ended_at:", "stderr_summary:", "stdout_head:", "stdout_tail:", "artifact_paths:", "modified_files:", "verification: failed"} {
		if !strings.Contains(content, want) {
			t.Errorf("missing %q from tool evidence: %s", want, content)
		}
	}
}

func TestValidateCompactionRejectsResolvedOpenQuestion(t *testing.T) {
	item := compactionItem("question", ContextOpenQuestion, "Should release proceed?", PriorityNormal)
	err := ValidateCompaction(CompactionRequest{Items: []ContextItem{item}, TargetTokens: 100}, CompactionResult{Content: "[question open_question]\nresolved", OutputTokens: 5, PreservedItemIDs: []string{"question"}})
	if err == nil || !strings.Contains(err.Error(), "unresolved question") {
		t.Fatalf("error = %v, want unresolved question validation error", err)
	}
}

func TestValidateCompactionRejectsSupersededItem(t *testing.T) {
	item := compactionItem("old", ContextDecision, "use old API", PriorityNormal)
	item.SupersededBy = "new"
	err := ValidateCompaction(CompactionRequest{Items: []ContextItem{item}, TargetTokens: 100}, CompactionResult{Content: "[old decision]\nuse old API", OutputTokens: 5, PreservedItemIDs: []string{"old"}})
	if err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("error = %v, want superseded validation error", err)
	}
}
