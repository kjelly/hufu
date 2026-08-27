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
	if got := strings.Join(result.PreservedItemIDs, ","); !strings.Contains(got, canonicalToolEvidenceID("call-1")) || !strings.Contains(got, canonicalToolEvidenceID("result-1")) {
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

func TestToolEvidenceItemsCanonicalizeSecretCallIdentity(t *testing.T) {
	secret := "explicit-call-secret-value"
	callID := "call-token=" + secret
	resultID := "result-secret=" + secret
	call := ToolCallEvidence{ID: callID, Tool: "bash", Command: "go test", Scope: Scope{ProjectID: "p"}}
	input := ToolResultEvidence{ID: resultID, ToolCallID: callID, Output: "ok", Scope: Scope{ProjectID: "p"}}

	items, edges, err := ToolEvidenceItemsChecked(call, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || len(edges) != 1 {
		t.Fatalf("invalid evidence: items=%#v edges=%#v", items, edges)
	}
	for _, value := range []string{items[0].ID, items[1].ID, edges[0].FromID, edges[0].ToID, items[0].Content, items[1].Content} {
		if strings.Contains(value, secret) {
			t.Fatalf("raw secret leaked through evidence: %q", value)
		}
	}
	if edges[0].FromID != items[1].ID || edges[0].ToID != items[0].ID {
		t.Fatalf("edge does not pair canonical item IDs: items=%#v edge=%#v", items, edges[0])
	}
	wantToolCallID := "tool_call_id: " + items[0].ID
	if !strings.Contains(items[1].Content, wantToolCallID) {
		t.Fatalf("rendered result does not use canonical call ID %q: %q", wantToolCallID, items[1].Content)
	}
	compacted, err := (DeterministicCompactor{}).Compact(context.Background(), CompactionRequest{Items: items, Edges: edges, TargetTokens: 1_000})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range append(append(append(compacted.SourceItemIDs, compacted.PreservedItemIDs...), compacted.OmittedItemIDs...), compacted.MissingItemIDs...) {
		if strings.Contains(id, secret) {
			t.Fatalf("raw secret leaked through compaction ID list: %q", id)
		}
	}

	repeatedItems, repeatedEdges, err := ToolEvidenceItemsChecked(call, input)
	if err != nil {
		t.Fatal(err)
	}
	if repeatedItems[0].ID != items[0].ID || repeatedItems[1].ID != items[1].ID || repeatedEdges[0].FromID != edges[0].FromID || repeatedEdges[0].ToID != edges[0].ToID || repeatedItems[1].Content != items[1].Content {
		t.Fatalf("canonical identity is not deterministic: first=%#v/%#v repeated=%#v/%#v", items, edges, repeatedItems, repeatedEdges)
	}
}

func TestToolEvidenceItemsCanonicalizeSecretResultIdentity(t *testing.T) {
	secret := "explicit-result-secret-value"
	call := ToolCallEvidence{ID: "call-1", Tool: "bash", Command: "go test", Scope: Scope{ProjectID: "p"}}
	items, edges, err := ToolEvidenceItemsChecked(call, ToolResultEvidence{ID: "result-api-key=" + secret, ToolCallID: call.ID, Output: "ok", Scope: Scope{ProjectID: "p"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{items[0].ID, items[1].ID, edges[0].FromID, edges[0].ToID, items[0].Content, items[1].Content} {
		if strings.Contains(value, secret) {
			t.Fatalf("raw result secret leaked through evidence: %q", value)
		}
	}
}

func TestToolEvidenceItemsCanonicalizeOpaqueASCIIIdentity(t *testing.T) {
	callID := "sk-opaque-7A9b2C"
	resultID := "result-opaque-7A9b2C"
	call := ToolCallEvidence{ID: callID, Tool: "bash", Command: "go test", Scope: Scope{ProjectID: "p"}}
	result := ToolResultEvidence{ID: resultID, ToolCallID: callID, Output: "ok", Scope: Scope{ProjectID: "p"}}

	items, edges, err := ToolEvidenceItemsChecked(call, result)
	if err != nil {
		t.Fatal(err)
	}
	wantCallID := canonicalToolEvidenceID(callID)
	wantResultID := canonicalToolEvidenceID(resultID)
	if items[0].ID != wantCallID || items[1].ID != wantResultID {
		t.Fatalf("opaque IDs were not canonically hashed: items=%#v", items)
	}
	if edges[0].FromID != wantResultID || edges[0].ToID != wantCallID {
		t.Fatalf("opaque ID edge linkage was not canonical: edge=%#v", edges[0])
	}
	if !strings.Contains(items[1].Content, "tool_call_id: "+wantCallID) {
		t.Fatalf("rendered result lost canonical call ID: %q", items[1].Content)
	}
	compacted, err := (DeterministicCompactor{}).Compact(context.Background(), CompactionRequest{Items: items, Edges: edges, TargetTokens: 1_000})
	if err != nil {
		t.Fatal(err)
	}
	values := []string{items[0].ID, items[1].ID, edges[0].FromID, edges[0].ToID}
	values = append(values, compacted.SourceItemIDs...)
	values = append(values, compacted.PreservedItemIDs...)
	values = append(values, compacted.OmittedItemIDs...)
	values = append(values, compacted.MissingItemIDs...)
	for _, value := range values {
		if strings.Contains(value, callID) || strings.Contains(value, resultID) {
			t.Fatalf("raw opaque ID leaked through evidence or compaction IDs: %q", value)
		}
	}
	if strings.Contains(compacted.Content, callID) || strings.Contains(compacted.Content, resultID) {
		t.Fatalf("raw opaque ID leaked through compacted rendered content: %q", compacted.Content)
	}
	if strings.Contains(items[0].Content, callID) || strings.Contains(items[0].Content, resultID) || strings.Contains(items[1].Content, callID) || strings.Contains(items[1].Content, resultID) {
		t.Fatalf("raw opaque ID leaked through rendered evidence: items=%#v", items)
	}

	repeatedItems, repeatedEdges, err := ToolEvidenceItemsChecked(call, result)
	if err != nil {
		t.Fatal(err)
	}
	if repeatedItems[0].ID != items[0].ID || repeatedItems[1].ID != items[1].ID || repeatedItems[1].Content != items[1].Content || repeatedEdges[0].FromID != edges[0].FromID || repeatedEdges[0].ToID != edges[0].ToID || repeatedEdges[0].Relation != edges[0].Relation {
		t.Fatalf("opaque ID canonicalization is not deterministic: first=%#v/%#v repeated=%#v/%#v", items, edges, repeatedItems, repeatedEdges)
	}
}

func TestToolEvidenceItemsRejectMismatchedResultToolCallID(t *testing.T) {
	callID := "sk-opaque-7A9b2C"
	items, edges, err := ToolEvidenceItemsChecked(
		ToolCallEvidence{ID: callID, Tool: "bash", Command: "go test"},
		ToolResultEvidence{ID: "result-1", ToolCallID: "sk-opaque-other", Output: "ok"},
	)
	if err != ErrToolEvidenceToolCallMismatch {
		t.Fatalf("error = %v, want %v", err, ErrToolEvidenceToolCallMismatch)
	}
	if items != nil || edges != nil {
		t.Fatalf("mismatched evidence returned a partial pair: items=%#v edges=%#v", items, edges)
	}
}

func TestToolEvidenceItemsBoundHugeUTF8IDsAsOpaqueCanonicalIDs(t *testing.T) {
	callID := strings.Repeat("識", 500)
	resultID := strings.Repeat("結", 500)
	call := ToolCallEvidence{ID: callID, Tool: "bash", Command: "go test", Scope: Scope{ProjectID: "p"}}
	result := ToolResultEvidence{ID: resultID, ToolCallID: callID, Output: "ok", Scope: Scope{ProjectID: "p"}}
	items, edges, err := ToolEvidenceItemsChecked(call, result)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{items[0].ID, items[1].ID, edges[0].FromID, edges[0].ToID} {
		if !strings.HasPrefix(id, "tool-id:") || len(id) != len("tool-id:")+64 || strings.ContainsAny(id, "識結") {
			t.Fatalf("ID was not bounded to an opaque ASCII value: %q", id)
		}
	}
	if edges[0].FromID != items[1].ID || edges[0].ToID != items[0].ID || !strings.Contains(items[1].Content, "tool_call_id: "+items[0].ID) {
		t.Fatalf("canonical pair linkage was not preserved: items=%#v edge=%#v", items, edges[0])
	}
	repeatedItems, repeatedEdges, err := ToolEvidenceItemsChecked(call, result)
	if err != nil || repeatedItems[0].ID != items[0].ID || repeatedItems[1].ID != items[1].ID || repeatedEdges[0].FromID != edges[0].FromID || repeatedEdges[0].ToID != edges[0].ToID {
		t.Fatalf("huge UTF-8 identity is not deterministic: first=%#v/%#v repeated=%#v/%#v err=%v", items, edges, repeatedItems, repeatedEdges, err)
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

func TestToolEvidenceItemsMinimumPolicyPreservesMandatoryEnvelope(t *testing.T) {
	minimum := ToolResultMandatoryMinimum()
	policy := ToolOutputPolicy{MaxBytes: minimum.Bytes, MaxRunes: minimum.Runes, MaxTokens: minimum.Tokens, DiagnosticLines: 1, DiagnosticTokens: minimum.Tokens}
	items, _, err := ToolEvidenceItemsChecked(
		ToolCallEvidence{ID: "x", Tool: "x", Command: "x", WorkingDir: "x"},
		ToolResultEvidence{ID: "r", ToolCallID: "x", Verification: "passed", OutputPolicy: policy, Output: "optional output"},
	)
	if err != nil {
		t.Fatalf("minimum-valid evidence rejected: %v", err)
	}
	content := items[1].Content
	for _, want := range []string{"tool: x", "tool_call_id: " + canonicalToolEvidenceID("x"), "command: x", "working_dir: x", "verification: passed"} {
		if !strings.Contains(content, want) {
			t.Fatalf("mandatory provenance %q missing from %q", want, content)
		}
	}
	if len(content) > policy.MaxBytes || len([]rune(content)) > policy.MaxRunes || estimateOutputTokens(content) > policy.MaxTokens {
		t.Fatalf("minimum evidence exceeds caps: bytes=%d runes=%d tokens=%d", len(content), len([]rune(content)), estimateOutputTokens(content))
	}
}

func TestToolEvidenceItemsFailsClosedForOversizedUTF8MandatoryProvenance(t *testing.T) {
	minimum := ToolResultMandatoryMinimum()
	policy := ToolOutputPolicy{MaxBytes: minimum.Bytes, MaxRunes: minimum.Runes, MaxTokens: minimum.Tokens, DiagnosticLines: 1, DiagnosticTokens: minimum.Tokens}
	longID := strings.Repeat("識", 40)
	longCommand := strings.Repeat("コマンド ", 40)
	items, edges, err := ToolEvidenceItemsChecked(
		ToolCallEvidence{ID: longID, Tool: "x", Command: longCommand, WorkingDir: "x"},
		ToolResultEvidence{ID: "result", ToolCallID: longID, Verification: "passed", Command: longCommand, OutputPolicy: policy, Output: "must not displace provenance"},
	)
	if err == nil || !strings.Contains(err.Error(), ErrToolEvidenceMandatoryCapacity.Error()) {
		t.Fatalf("error = %v, want mandatory-capacity failure", err)
	}
	if items != nil || edges != nil {
		t.Fatalf("failed evidence returned items or edges: items=%#v edges=%#v", items, edges)
	}
}

func TestCompactToolOutputWithPolicyRedactsDiagnosticsAndHonorsFramedCaps(t *testing.T) {
	policy := ToolOutputPolicy{MaxBytes: 180, MaxRunes: 90, MaxTokens: 22, DiagnosticLines: 2, DiagnosticTokens: 8}
	output := strings.Repeat("正常な出力 ", 200) + "\nwarning: harmless\n" + strings.Repeat("noise ", 20) + "\nERROR password=super-secret E1234\n" + strings.Repeat("tail ", 100)
	got := CompactToolOutputWithPolicy(output, policy)
	if len(got) > policy.MaxBytes || len([]rune(got)) > policy.MaxRunes || estimateOutputTokens(got) > policy.MaxTokens {
		t.Fatalf("normalized output exceeds caps: bytes=%d runes=%d tokens=%d output=%q", len(got), len([]rune(got)), estimateOutputTokens(got), got)
	}
	if strings.Contains(got, "super-secret") {
		t.Fatal("secret survived redaction")
	}
	if !strings.Contains(got, "E1234") {
		t.Fatalf("prioritized diagnostic was lost: %q", got)
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
