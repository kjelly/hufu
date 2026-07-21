package team

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

func TestStructuredSummary_RenderMarkdownAndParse(t *testing.T) {
	summary := StructuredSummary{
		Goal:                "Implement structured compaction in hufu CLI",
		Constraints:         []string{"User correction: Do not drop tool call/result pairs"},
		CompletedTasks:      []string{"Task 1: Design data structures"},
		InProgressTasks:     []string{"Task 2: Implement persistence"},
		BlockedTasks:        []string{"Task 3: External dependency"},
		KeyDecisions:        []string{"Decision: Use 13 structured sections"},
		ErrorsAndFixes:      []string{"Error: JSON parsing failed -> Fix: Fall back to markdown parser"},
		FilesRead:           []string{"internal/team/coordinator_session.go"},
		FilesModified:       []string{"internal/team/compaction.go"},
		ArtifactsProduced:   []string{"workspace/compaction_history.json"},
		VerificationResults: []string{"PASS: go test ./internal/team/..."},
		OpenQuestions:       []string{"Should compaction threshold be configurable?"},
		NextActions:         []string{"Run complete test suite"},
	}

	md := summary.RenderMarkdown()

	requiredHeadings := []string{
		"## Goal",
		"## Constraints",
		"## Completed Tasks",
		"## In-progress Tasks",
		"## Blocked Tasks",
		"## Key Decisions",
		"## Errors and Fixes",
		"## Files Read",
		"## Files Modified",
		"## Artifacts Produced",
		"## Verification Results",
		"## Open Questions",
		"## Next Actions",
	}

	for _, heading := range requiredHeadings {
		if !strings.Contains(md, heading) {
			t.Errorf("rendered markdown missing required section heading %q", heading)
		}
	}

	parsed := ParseStructuredSummary(md)

	if !strings.Contains(parsed.Goal, "Implement structured compaction") {
		t.Errorf("parsed Goal mismatch: got %q", parsed.Goal)
	}
	if len(parsed.Constraints) == 0 || !strings.Contains(parsed.Constraints[0], "tool call/result") {
		t.Errorf("parsed Constraints mismatch: got %v", parsed.Constraints)
	}
	if len(parsed.CompletedTasks) == 0 || !strings.Contains(parsed.CompletedTasks[0], "Task 1") {
		t.Errorf("parsed CompletedTasks mismatch: got %v", parsed.CompletedTasks)
	}
	if len(parsed.FilesRead) == 0 || parsed.FilesRead[0] != "internal/team/coordinator_session.go" {
		t.Errorf("parsed FilesRead mismatch: got %v", parsed.FilesRead)
	}
	if len(parsed.FilesModified) == 0 || parsed.FilesModified[0] != "internal/team/compaction.go" {
		t.Errorf("parsed FilesModified mismatch: got %v", parsed.FilesModified)
	}
}

func TestParseStructuredSummary_JSON(t *testing.T) {
	jsonRaw := `{
		"goal": "Refactor compaction",
		"constraints": ["Must preserve failures"],
		"completed_tasks": ["Parsed JSON"],
		"in_progress_tasks": [],
		"blocked_tasks": [],
		"key_decisions": ["Use JSON schema"],
		"errors_and_fixes": ["Syntax error -> Fixed syntax"],
		"files_read": ["main.go"],
		"files_modified": ["sidecar.go"],
		"artifacts_produced": [],
		"verification_results": ["PASS: json test"],
		"open_questions": [],
		"next_actions": ["Commit code"]
	}`

	parsed := ParseStructuredSummary(jsonRaw)
	if parsed.Goal != "Refactor compaction" {
		t.Errorf("expected goal 'Refactor compaction', got %q", parsed.Goal)
	}
	if len(parsed.Constraints) != 1 || parsed.Constraints[0] != "Must preserve failures" {
		t.Errorf("constraints mismatch: %v", parsed.Constraints)
	}

	codeBlockJSON := "```json\n" + jsonRaw + "\n```"
	parsedCB := ParseStructuredSummary(codeBlockJSON)
	if parsedCB.Goal != "Refactor compaction" {
		t.Errorf("codeblock JSON parse expected goal 'Refactor compaction', got %q", parsedCB.Goal)
	}
}

func TestAdjustBoundaryToPreserveToolPairs(t *testing.T) {
	call1 := fantasy.ToolCallPart{ToolCallID: "call_1", ToolName: "view", Input: `{"file_path":"foo.go"}`}
	res1 := fantasy.ToolResultPart{ToolCallID: "call_1", Output: fantasy.ToolResultOutputContentText{Text: "content"}}

	call2 := fantasy.ToolCallPart{ToolCallID: "call_2", ToolName: "write", Input: `{"file_path":"bar.go"}`}
	res2 := fantasy.ToolResultPart{ToolCallID: "call_2", Output: fantasy.ToolResultOutputContentText{Text: "ok"}}

	msgs := []fantasy.Message{
		fantasy.NewUserMessage("Start task"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{call1}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{res1}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{call2}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{res2}},
		fantasy.NewUserMessage("Next turn"),
	}

	// Case 1: cutIdx = 2 points after call1 but before res1.
	// AdjustBoundaryToPreserveToolPairs should shift cutIdx to 3 to include res1.
	adjusted := AdjustBoundaryToPreserveToolPairs(msgs, 2)
	if adjusted != 3 {
		t.Fatalf("expected cutIdx adjusted from 2 to 3, got %d", adjusted)
	}

	// Case 2: cutIdx = 3 points after res1 and before call2. Should remain 3.
	adjustedClean := AdjustBoundaryToPreserveToolPairs(msgs, 3)
	if adjustedClean != 3 {
		t.Fatalf("expected clean cutIdx 3 to remain 3, got %d", adjustedClean)
	}

	// Case 3: cutIdx = 4 points after call2 but before res2. Should shift to 5.
	adjusted2 := AdjustBoundaryToPreserveToolPairs(msgs, 4)
	if adjusted2 != 5 {
		t.Fatalf("expected cutIdx adjusted from 4 to 5, got %d", adjusted2)
	}
}

func TestCompactionInvariants(t *testing.T) {
	origGoal := "Build feature X"

	callView := fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "view", Input: `{"file_path":"src/main.go"}`}
	callEdit := fantasy.ToolCallPart{ToolCallID: "c2", ToolName: "edit", Input: `{"file_path":"src/main.go"}`}
	failResult := fantasy.ToolResultPart{ToolCallID: "c2", Output: fantasy.ToolResultOutputContentText{Text: "exit status 1: build failed"}}

	msgs := []fantasy.Message{
		fantasy.NewUserMessage("Build feature X"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{callView}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{callEdit}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{failResult}},
		fantasy.NewUserMessage("User correction: make sure to use package v2 instead"),
	}

	summary := EnforceCompactionInvariants(&StructuredSummary{}, nil, origGoal, msgs)

	// Invariant 2: Goal
	if summary.Goal != origGoal {
		t.Errorf("Invariant 2 failed: expected goal %q, got %q", origGoal, summary.Goal)
	}

	// Invariant 3: User correction
	foundCorr := false
	for _, c := range summary.Constraints {
		if strings.Contains(c, "package v2") {
			foundCorr = true
			break
		}
	}
	if !foundCorr {
		t.Errorf("Invariant 3 failed: user correction missing from constraints: %v", summary.Constraints)
	}

	// Invariant 4: Failed verification
	foundFail := false
	for _, v := range summary.VerificationResults {
		if strings.Contains(v, "build failed") || strings.Contains(v, "exit status 1") {
			foundFail = true
			break
		}
	}
	if !foundFail {
		t.Errorf("Invariant 4 failed: verification failure missing from results: %v", summary.VerificationResults)
	}

	// Invariant 5: Files Read and Modified
	if len(summary.FilesRead) == 0 || summary.FilesRead[0] != "src/main.go" {
		t.Errorf("Invariant 5 failed: FilesRead missing src/main.go: %v", summary.FilesRead)
	}
	if len(summary.FilesModified) == 0 || summary.FilesModified[0] != "src/main.go" {
		t.Errorf("Invariant 5 failed: FilesModified missing src/main.go: %v", summary.FilesModified)
	}

	// Invariant 6: Next compaction receives previous summary
	prevSummary := &StructuredSummary{
		Goal:           origGoal,
		KeyDecisions:   []string{"Decision: architecture approach A"},
		CompletedTasks: []string{"Task 0: Initial setup"},
		FilesRead:      []string{"README.md"},
	}

	newSummary := EnforceCompactionInvariants(&StructuredSummary{Goal: origGoal}, prevSummary, origGoal, msgs)

	if len(newSummary.KeyDecisions) == 0 || newSummary.KeyDecisions[0] != "Decision: architecture approach A" {
		t.Errorf("Invariant 6 failed: KeyDecisions from prevSummary dropped: %v", newSummary.KeyDecisions)
	}
	if len(newSummary.CompletedTasks) == 0 || newSummary.CompletedTasks[0] != "Task 0: Initial setup" {
		t.Errorf("Invariant 6 failed: CompletedTasks from prevSummary dropped: %v", newSummary.CompletedTasks)
	}
	// FilesRead should merge README.md and src/main.go
	if len(newSummary.FilesRead) < 2 {
		t.Errorf("Invariant 6 failed: FilesRead union incomplete: %v", newSummary.FilesRead)
	}
}

func TestExtractFileOperationsUsesRealToolArgumentKeys(t *testing.T) {
	callView := fantasy.ToolCallPart{
		ToolCallID: "read",
		ToolName:   "view",
		Input:      `{"file_path":"internal/team/coordinator.go"}`,
	}
	callWrite := fantasy.ToolCallPart{
		ToolCallID: "write",
		ToolName:   "write",
		Input:      `{"file_path":"internal/team/compaction.go"}`,
	}
	callDownload := fantasy.ToolCallPart{
		ToolCallID: "download",
		ToolName:   "download",
		Input:      `{"file_path":"workspace/out.bin"}`,
	}
	res := fantasy.ToolResultPart{ToolCallID: "read", Output: fantasy.ToolResultOutputContentText{Text: "ok"}}

	msgs := []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{callView}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{callWrite}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{callDownload}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{res}},
	}

	read, modified, artifacts := extractFileOperations(msgs)
	if len(read) != 1 || read[0] != "internal/team/coordinator.go" {
		t.Fatalf("unexpected read files: %#v", read)
	}
	if len(modified) != 2 {
		t.Fatalf("unexpected modified files: %#v", modified)
	}
	if len(artifacts) != 2 {
		t.Fatalf("unexpected artifacts: %#v", artifacts)
	}
	if artifacts[0] != "internal/team/compaction.go" || artifacts[1] != "workspace/out.bin" {
		t.Fatalf("unexpected artifact entries: %#v", artifacts)
	}
}

func TestCompactionRecord_Persistence(t *testing.T) {
	dir := t.TempDir()

	summary := StructuredSummary{
		Goal:           "Test Persistence",
		CompletedTasks: []string{"Step 1"},
	}

	record := CompactionRecord{
		ID:           "test_compaction_1",
		Timestamp:    time.Now(),
		TokensBefore: 1200,
		TokensAfter:  300,
		SourceRange: CompactionRange{
			StartIndex: 0,
			EndIndex:   10,
			MsgCount:   11,
		},
		Summary: summary,
	}

	// Save record
	if err := SaveCompactionRecord(dir, record); err != nil {
		t.Fatalf("SaveCompactionRecord failed: %v", err)
	}

	// Verify file created
	histPath := filepath.Join(dir, CompactionHistoryFile)
	if _, err := os.Stat(histPath); err != nil {
		t.Fatalf("compaction_history.json file not created: %v", err)
	}

	// Load history
	history, err := LoadCompactionHistory(dir)
	if err != nil {
		t.Fatalf("LoadCompactionHistory failed: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1 compaction record, got %d", len(history))
	}

	got := history[0]
	if got.ID != record.ID {
		t.Errorf("record ID mismatch: got %q, want %q", got.ID, record.ID)
	}
	if got.TokensBefore != 1200 || got.TokensAfter != 300 {
		t.Errorf("tokens mismatch: got %d -> %d, want 1200 -> 300", got.TokensBefore, got.TokensAfter)
	}
	if got.SourceRange.MsgCount != 11 {
		t.Errorf("source range msg count mismatch: got %d, want 11", got.SourceRange.MsgCount)
	}
	if got.Summary.Goal != "Test Persistence" {
		t.Errorf("summary goal mismatch: got %q", got.Summary.Goal)
	}

	// GetLatestCompactionSummary
	latest := GetLatestCompactionSummary(dir)
	if latest == nil || latest.Goal != "Test Persistence" {
		t.Fatalf("GetLatestCompactionSummary failed: %v", latest)
	}

	// Delete compaction history
	if err := DeleteCompactionHistory(dir); err != nil {
		t.Fatalf("DeleteCompactionHistory failed: %v", err)
	}
	if GetLatestCompactionSummary(dir) != nil {
		t.Fatalf("expected nil summary after deletion")
	}
}

type mockCompacter struct {
	response string
	err      error
}

func (m *mockCompacter) CompactStructured(ctx context.Context, conversationText, prevSummaryText, originalGoal string) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	return m.response, nil
}

func TestPerformStructuredCompaction_WithMockSidecar(t *testing.T) {
	mockJSON := `{
		"goal": "Mock Goal",
		"constraints": ["Mock Constraint"],
		"completed_tasks": ["Mock Task"],
		"in_progress_tasks": [],
		"blocked_tasks": [],
		"key_decisions": ["Mock Decision"],
		"errors_and_fixes": [],
		"files_read": ["mock.go"],
		"files_modified": [],
		"artifacts_produced": [],
		"verification_results": [],
		"open_questions": [],
		"next_actions": []
	}`

	mock := &mockCompacter{response: mockJSON}
	msgs := []fantasy.Message{fantasy.NewUserMessage("Mock Goal")}

	summary, err := PerformStructuredCompaction(context.Background(), mock, msgs, nil, "Mock Goal")
	if err != nil {
		t.Fatalf("PerformStructuredCompaction failed: %v", err)
	}

	if summary.Goal != "Mock Goal" {
		t.Errorf("expected goal 'Mock Goal', got %q", summary.Goal)
	}
	if len(summary.FilesRead) == 0 || summary.FilesRead[0] != "mock.go" {
		t.Errorf("expected files read ['mock.go'], got %v", summary.FilesRead)
	}
}

func TestStructuredSummary_UserCorrectionsAndSourceEntryIDs(t *testing.T) {
	summary := StructuredSummary{
		Goal:            "Test new fields",
		UserCorrections: []string{"Use v2 API"},
		SourceEntryIDs:  []string{"entry_1", "entry_2"},
	}

	md := summary.RenderMarkdown()
	if !strings.Contains(md, "## User Corrections") || !strings.Contains(md, "Use v2 API") {
		t.Errorf("rendered markdown missing User Corrections: %s", md)
	}
	if !strings.Contains(md, "## Source Entry IDs") || !strings.Contains(md, "entry_1") {
		t.Errorf("rendered markdown missing Source Entry IDs: %s", md)
	}

	parsed := ParseStructuredSummary(md)
	if len(parsed.UserCorrections) != 1 || parsed.UserCorrections[0] != "Use v2 API" {
		t.Errorf("parsed UserCorrections mismatch: %v", parsed.UserCorrections)
	}
	if len(parsed.SourceEntryIDs) != 2 || parsed.SourceEntryIDs[0] != "entry_1" {
		t.Errorf("parsed SourceEntryIDs mismatch: %v", parsed.SourceEntryIDs)
	}
}

func TestValidateStructuredSummary_FiveChecks(t *testing.T) {
	validSummary := &StructuredSummary{
		Goal:              "Build system",
		Constraints:       []string{"User correction: use gRPC"},
		UserCorrections:   []string{"User correction: use gRPC"},
		CompletedTasks:    []string{"Task 1: Init repo"},
		InProgressTasks:   []string{"Task 2: Implement auth"},
		FilesModified:     []string{"auth.go"},
		ArtifactsProduced: []string{"bin/app"},
		SourceEntryIDs:    []string{"e1"},
	}

	// 1. Goal check
	emptyGoal := *validSummary
	emptyGoal.Goal = ""
	if err := ValidateStructuredSummary(&emptyGoal, nil, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "Goal") {
		t.Errorf("expected Goal validation failure, got: %v", err)
	}

	// 2. Active tasks check
	if err := ValidateStructuredSummary(validSummary, nil, nil, []string{"Task 3: Missing task"}, nil); err == nil || !strings.Contains(err.Error(), "ActiveTasks") {
		t.Errorf("expected ActiveTasks validation failure, got: %v", err)
	}
	if err := ValidateStructuredSummary(validSummary, nil, nil, []string{"Task 2: Implement auth"}, nil); err != nil {
		t.Errorf("unexpected failure for present active task: %v", err)
	}

	// 3. Artifacts traceable check
	prevWithArtifact := &StructuredSummary{
		Goal:          "Build system",
		FilesModified: []string{"secret.go"},
	}
	if err := ValidateStructuredSummary(validSummary, prevWithArtifact, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "ArtifactsTraceable") {
		t.Errorf("expected ArtifactsTraceable validation failure, got: %v", err)
	}

	// 4. User correction check
	msgsWithCorr := []fantasy.Message{
		fantasy.NewUserMessage("First prompt"),
		fantasy.NewUserMessage("User correction: must use HTTPS"),
	}
	if err := ValidateStructuredSummary(validSummary, nil, msgsWithCorr, nil, nil); err == nil || !strings.Contains(err.Error(), "UserCorrection") {
		t.Errorf("expected UserCorrection validation failure, got: %v", err)
	}

	// 5. Failed task not done check
	badSummary := *validSummary
	badSummary.CompletedTasks = []string{"Task 4: Failed migration"}
	if err := ValidateStructuredSummary(&badSummary, nil, nil, nil, []string{"Task 4: Failed migration"}); err == nil || !strings.Contains(err.Error(), "FailedTaskNotDone") {
		t.Errorf("expected FailedTaskNotDone validation failure, got: %v", err)
	}

	callEdit := fantasy.ToolCallPart{ToolCallID: "c2", ToolName: "edit", Input: `{"file_path":"db.go"}`}
	failResult := fantasy.ToolResultPart{ToolCallID: "c2", Output: fantasy.ToolResultOutputContentText{Text: "FAIL: migration script crashed"}}
	failMsgs := []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{callEdit}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{failResult}},
	}
	badSummary2 := *validSummary
	badSummary2.FilesModified = append(badSummary2.FilesModified, "db.go")
	badSummary2.CompletedTasks = []string{"migration script crashed"}
	if err := ValidateStructuredSummary(&badSummary2, nil, failMsgs, nil, nil); err == nil || !strings.Contains(err.Error(), "FailedTaskNotDone") {
		t.Errorf("expected FailedTaskNotDone validation failure for failed verification, got: %v", err)
	}
}

func TestValidateStructuredSummary_FallbackPreservesOldSummary(t *testing.T) {
	prevSummary := &StructuredSummary{
		Goal:            "Original Goal",
		CompletedTasks:  []string{"Task 1: Done"},
		InProgressTasks: []string{"Task 2: Pending"},
		UserCorrections: []string{"User correction: stay on Go 1.22"},
	}

	badSummary := &StructuredSummary{
		Goal:           "",
		CompletedTasks: []string{"Task 1: Done"},
	}

	mock := &mockCompacter{response: badSummary.RenderMarkdown()}
	msgs := []fantasy.Message{
		fantasy.NewUserMessage("Original Goal"),
	}

	result, err := PerformStructuredCompaction(context.Background(), mock, msgs, prevSummary, "Original Goal")
	if err != nil {
		t.Fatalf("PerformStructuredCompaction error: %v", err)
	}

	if result.Goal != prevSummary.Goal {
		t.Errorf("expected fallback goal %q, got %q", prevSummary.Goal, result.Goal)
	}
	if len(result.InProgressTasks) == 0 || result.InProgressTasks[0] != "Task 2: Pending" {
		t.Errorf("expected in progress tasks from prevSummary retained: %v", result.InProgressTasks)
	}
}
