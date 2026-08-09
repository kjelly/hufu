package team

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
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
	viewResult := fantasy.ToolResultPart{ToolCallID: "c1", Output: fantasy.ToolResultOutputContentText{Text: "ok"}}
	failResult := fantasy.ToolResultPart{ToolCallID: "c2", Output: fantasy.ToolResultOutputContentText{Text: "exit status 1: build failed"}}

	msgs := []fantasy.Message{
		fantasy.NewUserMessage("Build feature X"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{callView}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{callEdit}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{viewResult}},
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
	writeRes := fantasy.ToolResultPart{ToolCallID: "write", Output: fantasy.ToolResultOutputContentText{Text: "ok"}}
	downloadRes := fantasy.ToolResultPart{ToolCallID: "download", Output: fantasy.ToolResultOutputContentText{Text: "ok"}}

	msgs := []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{callView}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{callWrite}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{callDownload}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{res}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{writeRes}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{downloadRes}},
	}

	read, modified, artifacts := extractFileOperations(msgs)
	if len(read) != 1 || read[0] != "internal/team/coordinator.go" {
		t.Fatalf("unexpected read files: %#v", read)
	}
	if len(modified) != 2 {
		t.Fatalf("unexpected modified files: %#v", modified)
	}
	if len(artifacts) != 0 {
		t.Fatalf("unexpected artifacts: %#v", artifacts)
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

func TestAdjustBoundaryToPreserveToolPairs_PreventsOrphanedResults(t *testing.T) {
	// Scenario: c2 call@1, orphan result c1@2, c2 result@3
	callC2 := fantasy.ToolCallPart{ToolCallID: "c2", ToolName: "edit", Input: `{"file_path":"b.go"}`}
	orphanC1 := fantasy.ToolResultPart{ToolCallID: "c1", Output: fantasy.ToolResultOutputContentText{Text: "old result"}}
	resC2 := fantasy.ToolResultPart{ToolCallID: "c2", Output: fantasy.ToolResultOutputContentText{Text: "ok"}}

	msgs := []fantasy.Message{
		fantasy.NewUserMessage("Start"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{callC2}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{orphanC1}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{resC2}},
		fantasy.NewUserMessage("Next"),
	}

	// Cut index 2 (between c2 call and c2 result) splits c2!
	if isToolPairBoundaryClean(msgs, 2) {
		t.Errorf("cutIdx 2 should NOT be clean because c2 call is in head and c2 result is in tail")
	}

	adjusted := AdjustBoundaryToPreserveToolPairs(msgs, 2)
	if adjusted == 2 {
		t.Errorf("AdjustBoundaryToPreserveToolPairs(msgs, 2) should not remain 2")
	}
	if !isToolPairBoundaryClean(msgs, adjusted) {
		t.Errorf("adjusted boundary %d must be clean", adjusted)
	}
}

func TestCrossCompactionVerificationFailureFormat(t *testing.T) {
	prevSummary := &StructuredSummary{
		Goal:                "Goal",
		VerificationResults: []string{"Verification failure: exit status 2"},
	}

	summary := EnforceCompactionInvariants(&StructuredSummary{Goal: "Goal"}, prevSummary, "Goal", nil)

	found := false
	for _, v := range summary.VerificationResults {
		if strings.Contains(v, "exit status 2") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected VerificationResults to preserve 'Verification failure: exit status 2', got: %v", summary.VerificationResults)
	}
}

func TestCompactionRecord_PrevRecordIDLinkage(t *testing.T) {
	dir := t.TempDir()

	rec1 := CompactionRecord{
		ID:           "compact_1",
		Timestamp:    time.Now(),
		TokensBefore: 1000,
		TokensAfter:  200,
		SourceRange:  CompactionRange{StartIndex: 0, EndIndex: 5, MsgCount: 6},
		Summary:      StructuredSummary{Goal: "Goal 1"},
	}
	rec2 := CompactionRecord{
		ID:           "compact_2",
		Timestamp:    time.Now(),
		TokensBefore: 800,
		TokensAfter:  150,
		SourceRange:  CompactionRange{StartIndex: 6, EndIndex: 12, MsgCount: 7},
		Summary:      StructuredSummary{Goal: "Goal 2"},
	}

	if err := SaveCompactionRecord(dir, rec1); err != nil {
		t.Fatalf("failed to save rec1: %v", err)
	}
	if err := SaveCompactionRecord(dir, rec2); err != nil {
		t.Fatalf("failed to save rec2: %v", err)
	}

	history, err := LoadCompactionHistory(dir)
	if err != nil {
		t.Fatalf("failed to load history: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 records, got %d", len(history))
	}
}

func TestCompactMessages_SourceRangeIsAbsolute(t *testing.T) {
	session := &TeamSession{
		Workspace: t.TempDir(),
		Dir:       t.TempDir(),
		Config:    agent.TeamConfig{Name: "test-team", GoalMode: "exploratory"},
	}
	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}
	ctx := context.Background()

	msgs1 := []fantasy.Message{
		fantasy.NewUserMessage("first pass"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "ack"}}},
	}
	_ = c.compactMessages(ctx, msgs1, 0, []int{1, 1})

	history, err := LoadCompactionHistory(session.Workspace)
	if err != nil {
		t.Fatalf("LoadCompactionHistory failed: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1 compaction record after first compaction, got %d", len(history))
	}
	if history[0].SourceRange.StartIndex != 0 || history[0].SourceRange.MsgCount != 2 {
		t.Fatalf("unexpected first source range: %#v", history[0].SourceRange)
	}

	nextOffset := history[0].SourceRange.EndIndex + 1
	msgs2 := []fantasy.Message{
		fantasy.NewUserMessage("second pass"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "ack"}}},
	}
	_ = c.compactMessages(ctx, msgs2, nextOffset, []int{1, 1})

	history, err = LoadCompactionHistory(session.Workspace)
	if err != nil {
		t.Fatalf("LoadCompactionHistory failed: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 compaction records after second compaction, got %d", len(history))
	}
	if history[1].SourceRange.StartIndex != nextOffset {
		t.Fatalf("expected second source range start %d, got %d", nextOffset, history[1].SourceRange.StartIndex)
	}
	if history[1].SourceRange.EndIndex < history[1].SourceRange.StartIndex {
		t.Fatalf("invalid second source range: %#v", history[1].SourceRange)
	}
}

func TestExtractFileOperations_RealToolSchema(t *testing.T) {
	callView := fantasy.ToolCallPart{
		ToolCallID: "c_view",
		ToolName:   "view",
		Input:      `{"file_path":"src/view.go"}`,
	}
	callWrite := fantasy.ToolCallPart{
		ToolCallID: "c_write",
		ToolName:   "write",
		Input:      `{"file_path":"src/newfile.go", "content":"package main"}`,
	}
	callEdit := fantasy.ToolCallPart{
		ToolCallID: "c_edit",
		ToolName:   "edit",
		Input:      `{"file_path":"src/editfile.go"}`,
	}
	callMultiEdit := fantasy.ToolCallPart{
		ToolCallID: "c_multiedit",
		ToolName:   "multiedit",
		Input:      `{"file_path":"src/multi.go"}`,
	}
	callDownload := fantasy.ToolCallPart{
		ToolCallID: "c_download",
		ToolName:   "download",
		Input:      `{"file_path":"downloads/file.zip", "url":"http://example.com/file.zip"}`,
	}

	msgs := []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{callView}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{callWrite}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{callEdit}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{callMultiEdit}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{callDownload}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "c_view", Output: fantasy.ToolResultOutputContentText{Text: "ok"}}}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "c_write", Output: fantasy.ToolResultOutputContentText{Text: "ok"}}}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "c_edit", Output: fantasy.ToolResultOutputContentText{Text: "ok"}}}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "c_multiedit", Output: fantasy.ToolResultOutputContentText{Text: "ok"}}}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "c_download", Output: fantasy.ToolResultOutputContentText{Text: "ok"}}}},
	}

	read, modified, artifacts := extractFileOperations(msgs)

	if len(read) != 1 || read[0] != "src/view.go" {
		t.Errorf("expected read files ['src/view.go'], got %v", read)
	}
	expectedMod := []string{"src/newfile.go", "src/editfile.go", "src/multi.go", "downloads/file.zip"}
	if len(modified) != len(expectedMod) {
		t.Fatalf("expected %d modified files, got %d: %v", len(expectedMod), len(modified), modified)
	}
	for i, m := range expectedMod {
		if modified[i] != m {
			t.Errorf("modified[%d]: expected %q, got %q", i, m, modified[i])
		}
	}
	expectedArt := []string(nil)
	if len(artifacts) != len(expectedArt) {
		t.Fatalf("expected %d artifacts, got %d: %v", len(expectedArt), len(artifacts), artifacts)
	}
	for i, a := range expectedArt {
		if artifacts[i] != a {
			t.Errorf("artifacts[%d]: expected %q, got %q", i, a, artifacts[i])
		}
	}
}

func TestExtractFileOperations_RequiresSuccessfulResult(t *testing.T) {
	call := func(id, path string) fantasy.Message {
		return fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: id, ToolName: "write", Input: `{"file_path":"` + path + `"}`}}}
	}
	msgs := []fantasy.Message{
		call("success", "created.go"),
		call("failed", "failed.go"),
		call("missing", "missing.go"),
		call("overwrite", "existing.go"),
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "success", Output: fantasy.ToolResultOutputContentText{Text: "ok"}}}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "failed", Output: fantasy.ToolResultOutputContentError{Error: fmt.Errorf("cancelled")}}}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "overwrite", Output: fantasy.ToolResultOutputContentText{Text: "updated existing file"}}}},
	}
	_, modified, artifacts := extractFileOperations(msgs)
	if got, want := strings.Join(modified, ","), "created.go,existing.go"; got != want {
		t.Fatalf("modified files = %q, want %q", got, want)
	}
	if len(artifacts) != 0 {
		t.Fatalf("tool calls must not infer artifact creation: %#v", artifacts)
	}
}

func TestIsVerificationFailure_IgnoresSuccessfulStatusText(t *testing.T) {
	for _, text := range []string{"exit status 0", "no tests failed", "0 errors", "failure count: 0"} {
		if isVerificationFailure(text) {
			t.Errorf("successful status %q marked as failure", text)
		}
	}
}

func TestIsVerificationFailure_DoesNotMaskFailureAfterSuccessLine(t *testing.T) {
	for _, text := range []string{
		"phase 1: 0 errors\nphase 2: build failed",
		"exit status 0\nverification failed: integration check",
	} {
		if !isVerificationFailure(text) {
			t.Errorf("mixed verification output %q was not marked as failure", text)
		}
	}
}

func TestCompactMessages_MergesTypedTaskResultFactsWithoutSidecar(t *testing.T) {
	session := &TeamSession{Workspace: t.TempDir(), Dir: t.TempDir(), Config: agent.TeamConfig{Name: "test", GoalMode: "exploratory"}}
	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 8, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "produce report"}})[0]
	if err := c.taskTracker.TodoList().SetTypedResult(item.ID, &TaskResult{
		Artifacts:     []ArtifactRef{{Path: "reports/final.md"}},
		FilesRead:     []FileRef{{Path: "README.md"}},
		FilesModified: []FileRef{{Path: "internal/team/compaction.go"}},
		Verification:  []VerificationResult{{Command: "test -f reports/final.md", ExitCode: 0}},
	}); err != nil {
		t.Fatal(err)
	}

	_ = c.compactMessages(context.Background(), []fantasy.Message{fantasy.NewUserMessage("produce a report")}, 0, []int{1})
	if got := c.Metrics().Compactions; got != 1 {
		t.Fatalf("compaction metric = %d, want 1", got)
	}
	summary := c.lastCompactionSummary
	if summary == nil {
		t.Fatal("expected compaction summary")
	}
	for _, check := range []struct {
		label  string
		values []string
		want   string
	}{
		{"artifacts", summary.ArtifactsProduced, "reports/final.md"},
		{"files read", summary.FilesRead, "README.md"},
		{"files modified", summary.FilesModified, "internal/team/compaction.go"},
		{"verification", summary.VerificationResults, "test -f reports/final.md"},
	} {
		found := false
		for _, value := range check.values {
			if strings.Contains(value, check.want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("typed %s missing %q from %#v", check.label, check.want, check.values)
		}
	}
}

func TestCompactMessages_MergesTypedTaskResultFacts_OnValidationFallback(t *testing.T) {
	session := &TeamSession{Workspace: t.TempDir(), Dir: t.TempDir(), Config: agent.TeamConfig{Name: "test", GoalMode: "exploratory"}}
	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 8, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}

	// Active task that will trigger ValidateStructuredSummary failure if missing from sidecar/candidate summary.
	activeItem := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "Task 1: Active background job"}})[0]
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(activeItem.ID, TaskInProgress, "", ""); err != nil {
		t.Fatal(err)
	}

	// Completed task with typed facts
	doneItem := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "Task 2: Produce report"}})[0]
	if err := c.taskTracker.TodoList().SetTypedResult(doneItem.ID, &TaskResult{
		Artifacts:     []ArtifactRef{{Path: "reports/final.md"}},
		FilesRead:     []FileRef{{Path: "README.md"}},
		FilesModified: []FileRef{{Path: "internal/team/compaction.go"}},
		Verification:  []VerificationResult{{Command: "test -f reports/final.md", ExitCode: 0}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(doneItem.ID, TaskDone, "", ""); err != nil {
		t.Fatal(err)
	}

	prevSummary := &StructuredSummary{
		Goal: "Initial user goal",
		CompletedTasks: []string{
			"Task 0: Initial setup",
		},
	}
	c.lastCompactionSummary = prevSummary

	// compactMessages with prevSummary set on c.
	// Sidecar is nil, so PerformStructuredCompaction yields candidate without activeItem in InProgressTasks.
	// ValidateStructuredSummary will fail because activeItem ("Task 1: Active background job") is missing.
	// Falling back to prevSummary must NOT drop the typed task result facts from doneItem!
	_ = c.compactMessages(context.Background(), []fantasy.Message{fantasy.NewUserMessage("Initial user goal")}, 0, []int{1})

	summary := c.lastCompactionSummary
	if summary == nil {
		t.Fatal("expected compaction summary")
	}

	for _, check := range []struct {
		label  string
		values []string
		want   string
	}{
		{"artifacts", summary.ArtifactsProduced, "reports/final.md"},
		{"files read", summary.FilesRead, "README.md"},
		{"files modified", summary.FilesModified, "internal/team/compaction.go"},
		{"verification", summary.VerificationResults, "test -f reports/final.md"},
	} {
		found := false
		for _, value := range check.values {
			if strings.Contains(value, check.want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("fallback summary typed %s missing %q from %#v", check.label, check.want, check.values)
		}
	}
	if err := ValidateStructuredSummary(summary, prevSummary, []fantasy.Message{fantasy.NewUserMessage("Initial user goal")}, []string{activeItem.ID}, nil); err != nil {
		t.Fatalf("deterministic fallback must validate after reconciling active tasks: %v", err)
	}
}

func TestFormatTaskVerification_TimedOutAndNegativeExitCode(t *testing.T) {
	tests := []struct {
		name         string
		taskID       string
		verification VerificationResult
		wantPrefix   string
		wantContains []string
	}{
		{
			name:         "timed out with exit code 0",
			taskID:       "1",
			verification: VerificationResult{Command: "go test", TimedOut: true, ExitCode: 0},
			wantPrefix:   "FAIL:",
			wantContains: []string{"task 1 verification", "\"go test\"", "timed out"},
		},
		{
			name:         "timed out with exit code -1",
			taskID:       "2",
			verification: VerificationResult{Command: "go test", TimedOut: true, ExitCode: -1},
			wantPrefix:   "FAIL:",
			wantContains: []string{"task 2 verification", "\"go test\"", "timed out", "exit status -1"},
		},
		{
			name:         "negative exit code without timeout",
			taskID:       "3",
			verification: VerificationResult{Command: "go test", TimedOut: false, ExitCode: -1},
			wantPrefix:   "FAIL:",
			wantContains: []string{"task 3 verification", "\"go test\"", "exit status -1"},
		},
		{
			name:         "positive exit code failure",
			taskID:       "4",
			verification: VerificationResult{Command: "go test", TimedOut: false, ExitCode: 1},
			wantPrefix:   "FAIL:",
			wantContains: []string{"task 4 verification", "\"go test\"", "exit status 1"},
		},
		{
			name:         "zero exit code success",
			taskID:       "5",
			verification: VerificationResult{Command: "go test", TimedOut: false, ExitCode: 0},
			wantPrefix:   "task 5 verification",
			wantContains: []string{"task 5 verification", "\"go test\"", "exit status 0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatTaskVerification(tt.taskID, tt.verification)
			if !strings.HasPrefix(got, tt.wantPrefix) {
				t.Errorf("formatTaskVerification() = %q, want prefix %q", got, tt.wantPrefix)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("formatTaskVerification() = %q, missing substring %q", got, want)
				}
			}
		})
	}
}

func TestIsVerificationFailure_TimeoutCancelledAndNegativeExitCode(t *testing.T) {
	failCases := []string{
		"task 1 verification \"go test\": timed out (exit status 0)",
		"task 1 verification \"go test\": timed out",
		"task 1 verification \"go test\": cancelled",
		"task 1 verification \"go test\": canceled",
		"task 1 verification \"go test\": exit status -1",
		"FAIL: task 1 verification \"go test\": timed out",
		"FAIL: task 1 verification \"go test\": exit status -1",
	}

	for _, text := range failCases {
		if !isVerificationFailure(text) {
			t.Errorf("expected isVerificationFailure(%q) = true, got false", text)
		}
	}
}

func TestMergeTypedTaskResultFacts_DoesNotMutatePrevSummary(t *testing.T) {
	prevSummary := &StructuredSummary{
		Goal:           "Initial Goal",
		CompletedTasks: []string{"Task 1: Setup"},
	}
	items := []*TodoItem{
		{
			ID:     "1",
			Desc:   "Task 1: Setup",
			Status: TaskDone,
			TypedResult: &TaskResult{
				Artifacts: []ArtifactRef{{Path: "report.md"}},
			},
		},
	}

	merged := mergeTypedTaskResultFacts(prevSummary, items)

	if len(prevSummary.ArtifactsProduced) != 0 {
		t.Fatalf("expected prevSummary.ArtifactsProduced to remain empty, got %v", prevSummary.ArtifactsProduced)
	}
	if len(merged.ArtifactsProduced) != 1 || merged.ArtifactsProduced[0] != "report.md" {
		t.Fatalf("expected merged summary to have report.md, got %v", merged.ArtifactsProduced)
	}
}

func TestMergeTypedTaskResultFacts_WritesFailedVerificationsToErrorsAndFixes(t *testing.T) {
	items := []*TodoItem{
		{
			ID:     "1",
			Desc:   "Task 1: Run tests",
			Status: TaskDone,
			TypedResult: &TaskResult{
				Verification: []VerificationResult{
					{Command: "go test ./...", TimedOut: true, ExitCode: 0},
					{Command: "go test ./pkg1", TimedOut: false, ExitCode: 1},
					{Command: "go test ./pkg2", TimedOut: false, ExitCode: -1},
					{Command: "go test ./pkg3", TimedOut: false, ExitCode: 0},
				},
			},
		},
	}

	summary := mergeTypedTaskResultFacts(&StructuredSummary{}, items)

	if len(summary.VerificationResults) != 4 {
		t.Fatalf("expected 4 verification results, got %d: %v", len(summary.VerificationResults), summary.VerificationResults)
	}

	if len(summary.ErrorsAndFixes) != 3 {
		t.Fatalf("expected 3 failed verifications in ErrorsAndFixes, got %d: %v", len(summary.ErrorsAndFixes), summary.ErrorsAndFixes)
	}

	for _, ef := range summary.ErrorsAndFixes {
		if strings.Contains(ef, "pkg3") {
			t.Errorf("successful verification pkg3 incorrectly included in ErrorsAndFixes: %v", summary.ErrorsAndFixes)
		}
	}
}

func TestMergeTypedTaskResultFacts_ValidationFallback_ReconcilesCompletedTask(t *testing.T) {
	session := &TeamSession{Workspace: t.TempDir(), Dir: t.TempDir(), Config: agent.TeamConfig{Name: "test", GoalMode: "exploratory"}}
	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 8, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}

	// Active task that will trigger ValidateStructuredSummary failure if missing from candidate summary.
	activeItem := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "Task 2: Active background job"}})[0]
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(activeItem.ID, TaskInProgress, "", ""); err != nil {
		t.Fatal(err)
	}

	// Item with failed verification
	doneItem := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "Task 1: Run build"}})[0]
	if err := c.taskTracker.TodoList().SetTypedResult(doneItem.ID, &TaskResult{
		Verification: []VerificationResult{{Command: "go build", TimedOut: false, ExitCode: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(doneItem.ID, TaskError, "", ""); err != nil {
		t.Fatal(err)
	}

	prevSummary := &StructuredSummary{
		Goal:           "Initial user goal",
		CompletedTasks: []string{"Task 1: Run build"},
	}
	c.lastCompactionSummary = prevSummary

	// Trigger compactMessages which falls back to prevSummary.
	_ = c.compactMessages(context.Background(), []fantasy.Message{fantasy.NewUserMessage("Initial user goal")}, 0, []int{1})

	summary := c.lastCompactionSummary
	if summary == nil {
		t.Fatal("expected compaction summary")
	}

	// prevSummary original pointer must NOT be modified
	if len(prevSummary.CompletedTasks) != 1 || prevSummary.CompletedTasks[0] != "Task 1: Run build" {
		t.Errorf("prevSummary was mutated! CompletedTasks = %v", prevSummary.CompletedTasks)
	}

	// In lastCompactionSummary, failed verification must be in VerificationResults and ErrorsAndFixes,
	// and Task 1: Run build must be reconciled out of CompletedTasks.
	if len(summary.VerificationResults) != 1 {
		t.Errorf("expected 1 verification result, got %v", summary.VerificationResults)
	}
	if len(summary.ErrorsAndFixes) != 1 {
		t.Errorf("expected 1 error/fix, got %v", summary.ErrorsAndFixes)
	}
	for _, completed := range summary.CompletedTasks {
		if strings.Contains(completed, "Task 1") {
			t.Errorf("reconciliation failed: completed task %q still present in CompletedTasks: %v", completed, summary.CompletedTasks)
		}
	}
}

func TestMergeTypedTaskResultFacts_FailedTaskIDDoesNotRemoveSubstringMatches(t *testing.T) {
	summary := mergeTypedTaskResultFacts(&StructuredSummary{
		CompletedTasks: []string{
			"Task 1: failed work",
			"Task 10: successful work",
			"Task 21: successful work",
			"Task 30: document contains 1 but is successful",
		},
	}, []*TodoItem{{ID: "1", Desc: "Task 1: failed work", Status: TaskError}})

	got := strings.Join(summary.CompletedTasks, "\n")
	if strings.Contains(got, "Task 1: failed work") {
		t.Fatalf("failed task was retained: %v", summary.CompletedTasks)
	}
	for _, want := range []string{"Task 10: successful work", "Task 21: successful work", "Task 30: document contains 1 but is successful"} {
		if !strings.Contains(got, want) {
			t.Errorf("unrelated completed task %q was removed: %v", want, summary.CompletedTasks)
		}
	}
}

func TestCompactMessages_InvalidSummaryDoesNotReplaceHistoryOrPersist(t *testing.T) {
	workspace := t.TempDir()
	c, err := NewCoordinator(&TeamSession{Workspace: workspace, Dir: workspace, Config: agent.TeamConfig{Name: "test", GoalMode: "exploratory"}}, "", "", nil, nil, nil, RoleModels{}, 8, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}

	// An empty user message gives both the candidate and deterministic fallback
	// no valid goal. The invalid candidate must not become session state or a
	// persisted compaction record.
	messages := []fantasy.Message{fantasy.NewUserMessage("")}
	got := c.compactMessages(context.Background(), messages, 0, []int{1})
	if len(got) != len(messages) || got[0].Role != messages[0].Role {
		t.Fatalf("invalid compaction should retain original messages, got %#v", got)
	}
	if c.lastCompactionSummary != nil {
		t.Fatalf("invalid compaction updated last summary: %#v", c.lastCompactionSummary)
	}
	history, err := LoadCompactionHistory(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 0 {
		t.Fatalf("invalid compaction persisted records: %#v", history)
	}
}

func TestCompactMessagesEmbedsVerifiedToolEvidence(t *testing.T) {
	workspace := t.TempDir()
	c, err := NewCoordinator(&TeamSession{Workspace: workspace, Dir: workspace, Config: agent.TeamConfig{Name: "test", GoalMode: "exploratory"}}, "", "", nil, nil, nil, RoleModels{}, 8, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	output := strings.Repeat("normal output\n", 4_000) + "COMPILER ERROR E1234: failure in internal/team/run.go\nexit status 1\n" + strings.Repeat("normal output\n", 4_000)
	messages := []fantasy.Message{
		fantasy.NewUserMessage("Fix the failing test"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "call-1", ToolName: "bash", Input: `{"command":"go test ./...","working_dir":"/repo"}`}}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "call-1", Output: fantasy.ToolResultOutputContentText{Text: output}}}},
	}
	got := c.compactMessages(context.Background(), messages, 0, []int{1, 1, 1})
	if len(got) != 1 {
		t.Fatalf("compacted messages = %d, want 1", len(got))
	}
	text, ok := fantasy.AsMessagePart[fantasy.TextPart](got[0].Content[0])
	if !ok {
		t.Fatalf("unexpected compacted part %#v", got[0].Content[0])
	}
	for _, want := range []string{"[Verified Compacted History]", "tool_call_id: call-1", "working_dir: /repo", "exit_code: 1", "COMPILER ERROR E1234"} {
		if !strings.Contains(text.Text, want) {
			t.Errorf("verified history missing %q", want)
		}
	}
}

func TestCompactMessagesRetainsOriginalWhenVerifiedBudgetCannotFit(t *testing.T) {
	workspace := t.TempDir()
	c, err := NewCoordinator(&TeamSession{Workspace: workspace, Dir: workspace, Config: agent.TeamConfig{Name: "test", GoalMode: "exploratory"}}, "", "", nil, nil, nil, RoleModels{}, 8, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	// The original user requirement is Tier 0/must-keep. It deliberately
	// exceeds verifiedHistoryBudgetTokens, so compaction must return the input
	// unchanged rather than create an unchecked summary.
	original := strings.Repeat("non-negotiable requirement ", verifiedHistoryBudgetTokens)
	messages := []fantasy.Message{fantasy.NewUserMessage(original)}
	got := c.compactMessages(context.Background(), messages, 0, []int{1})
	if len(got) != 1 || got[0].Role != messages[0].Role {
		t.Fatalf("fallback did not retain original message: %#v", got)
	}
	text, ok := fantasy.AsMessagePart[fantasy.TextPart](got[0].Content[0])
	if !ok || text.Text != original {
		t.Fatal("fallback altered original must-preserve history")
	}
}
