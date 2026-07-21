package team

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"charm.land/fantasy"
)

const CompactionHistoryFile = "compaction_history.json"
const verificationFailurePrefix = "FAIL: "

// StructuredSummary contains the sections for structured compaction.
type StructuredSummary struct {
	Goal                string   `json:"goal"`
	Constraints         []string `json:"constraints"`
	UserCorrections     []string `json:"user_corrections,omitempty"`
	CompletedTasks      []string `json:"completed_tasks"`
	InProgressTasks     []string `json:"in_progress_tasks"`
	BlockedTasks        []string `json:"blocked_tasks"`
	KeyDecisions        []string `json:"key_decisions"`
	ErrorsAndFixes      []string `json:"errors_and_fixes"`
	FilesRead           []string `json:"files_read"`
	FilesModified       []string `json:"files_modified"`
	ArtifactsProduced   []string `json:"artifacts_produced"`
	VerificationResults []string `json:"verification_results"`
	OpenQuestions       []string `json:"open_questions"`
	NextActions         []string `json:"next_actions"`
	SourceEntryIDs      []string `json:"source_entry_ids,omitempty"`
}

// CompactionRange records the range of messages included in a compaction operation.
type CompactionRange struct {
	StartIndex int `json:"start_index"`
	EndIndex   int `json:"end_index"`
	MsgCount   int `json:"msg_count"`
}

// CompactionRecord represents a persisted compaction event with token usage and range metadata.
type CompactionRecord struct {
	ID           string            `json:"id"`
	Timestamp    time.Time         `json:"timestamp"`
	TokensBefore int               `json:"tokens_before"`
	TokensAfter  int               `json:"tokens_after"`
	SourceRange  CompactionRange   `json:"source_range"`
	Summary      StructuredSummary `json:"summary"`
}

// SidecarCompacter interface abstracts the LLM sidecar compact operation without creating package import cycles.
type SidecarCompacter interface {
	CompactStructured(ctx context.Context, conversationText, prevSummaryText, originalGoal string) (string, error)
}

// RenderMarkdown formats the StructuredSummary into markdown containing all 13 sections.
func (s *StructuredSummary) RenderMarkdown() string {
	if s == nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Goal\n")
	if strings.TrimSpace(s.Goal) != "" {
		sb.WriteString(strings.TrimSpace(s.Goal) + "\n\n")
	} else {
		sb.WriteString("(none)\n\n")
	}

	renderSection := func(title string, items []string) {
		sb.WriteString("## " + title + "\n")
		validItems := make([]string, 0, len(items))
		for _, item := range items {
			item = strings.TrimSpace(item)
			if item != "" && item != "(none)" && item != "None" {
				validItems = append(validItems, item)
			}
		}
		if len(validItems) == 0 {
			sb.WriteString("(none)\n\n")
			return
		}
		for _, item := range validItems {
			if !strings.HasPrefix(item, "- ") && !strings.HasPrefix(item, "* ") {
				sb.WriteString("- " + item + "\n")
			} else {
				sb.WriteString(item + "\n")
			}
		}
		sb.WriteString("\n")
	}

	renderSection("Constraints", s.Constraints)
	renderSection("User Corrections", s.UserCorrections)
	renderSection("Completed Tasks", s.CompletedTasks)
	renderSection("In-progress Tasks", s.InProgressTasks)
	renderSection("Blocked Tasks", s.BlockedTasks)
	renderSection("Key Decisions", s.KeyDecisions)
	renderSection("Errors and Fixes", s.ErrorsAndFixes)
	renderSection("Files Read", s.FilesRead)
	renderSection("Files Modified", s.FilesModified)
	renderSection("Artifacts Produced", s.ArtifactsProduced)
	renderSection("Verification Results", s.VerificationResults)
	renderSection("Open Questions", s.OpenQuestions)
	renderSection("Next Actions", s.NextActions)
	renderSection("Source Entry IDs", s.SourceEntryIDs)

	return strings.TrimSpace(sb.String())
}

var jsonCodeBlockRegex = regexp.MustCompile("(?s)```(?:json)?\\s*\\n?(.*?)\\n?```")

// ParseStructuredSummary parses a text response (JSON or Markdown) into a StructuredSummary.
func ParseStructuredSummary(text string) StructuredSummary {
	text = strings.TrimSpace(text)
	if text == "" {
		return StructuredSummary{}
	}

	// Try extracting JSON from code blocks if present
	jsonText := text
	if match := jsonCodeBlockRegex.FindStringSubmatch(text); len(match) >= 2 {
		jsonText = strings.TrimSpace(match[1])
	}

	var summary StructuredSummary
	if err := json.Unmarshal([]byte(jsonText), &summary); err == nil {
		return summary
	}
	if err := json.Unmarshal([]byte(text), &summary); err == nil {
		return summary
	}

	// Fall back to Markdown section parsing
	return parseMarkdownSummary(text)
}

func parseMarkdownSummary(text string) StructuredSummary {
	var summary StructuredSummary
	lines := strings.Split(text, "\n")
	currentSection := ""
	var currentLines []string

	flushSection := func() {
		if currentSection == "" {
			return
		}
		content := strings.TrimSpace(strings.Join(currentLines, "\n"))
		currentLines = nil

		var items []string
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || line == "(none)" || line == "None" {
				continue
			}
			line = strings.TrimPrefix(line, "- ")
			line = strings.TrimPrefix(line, "* ")
			if line != "" {
				items = append(items, line)
			}
		}

		switch strings.ToLower(currentSection) {
		case "goal":
			summary.Goal = content
		case "constraints":
			summary.Constraints = items
		case "user corrections", "user correction":
			summary.UserCorrections = items
		case "completed tasks":
			summary.CompletedTasks = items
		case "in-progress tasks":
			summary.InProgressTasks = items
		case "blocked tasks":
			summary.BlockedTasks = items
		case "key decisions":
			summary.KeyDecisions = items
		case "errors and fixes":
			summary.ErrorsAndFixes = items
		case "files read":
			summary.FilesRead = items
		case "files modified":
			summary.FilesModified = items
		case "artifacts produced":
			summary.ArtifactsProduced = items
		case "verification results":
			summary.VerificationResults = items
		case "open questions":
			summary.OpenQuestions = items
		case "next actions":
			summary.NextActions = items
		case "source entry ids", "source entry id":
			summary.SourceEntryIDs = items
		}
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") || strings.HasPrefix(trimmed, "# ") {
			flushSection()
			heading := strings.TrimLeft(trimmed, "# ")
			heading = strings.TrimSpace(heading)
			currentSection = heading
		} else {
			currentLines = append(currentLines, line)
		}
	}
	flushSection()

	return summary
}

// Invariant 1: AdjustBoundaryToPreserveToolPairs guarantees that cutIdx never splits
// a message containing ToolCallPart from its matching message containing ToolResultPart.
func AdjustBoundaryToPreserveToolPairs(messages []fantasy.Message, cutIdx int) int {
	if cutIdx <= 0 || cutIdx >= len(messages) {
		return cutIdx
	}
	if isToolPairBoundaryClean(messages, cutIdx) {
		return cutIdx
	}

	for next := cutIdx + 1; next <= len(messages); next++ {
		if isToolPairBoundaryClean(messages, next) {
			return next
		}
	}

	for prev := cutIdx - 1; prev >= 0; prev-- {
		if isToolPairBoundaryClean(messages, prev) {
			return prev
		}
	}

	return 0
}

func isToolPairBoundaryClean(messages []fantasy.Message, cutIdx int) bool {
	if cutIdx <= 0 {
		return cutIdx == 0
	}
	if cutIdx > len(messages) {
		return false
	}

	pendingCallIDs := make(map[string]bool)
	for _, msg := range messages[:cutIdx] {
		for id := range extractToolCallIDs(msg) {
			pendingCallIDs[id] = true
		}
		for id := range extractToolResultCallIDs(msg) {
			delete(pendingCallIDs, id)
		}
	}

	return len(pendingCallIDs) == 0
}

func extractToolCallIDs(msg fantasy.Message) map[string]bool {
	ids := make(map[string]bool)
	for _, part := range msg.Content {
		if p, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok && p.ToolCallID != "" {
			ids[p.ToolCallID] = true
		}
	}
	return ids
}

func extractToolResultCallIDs(msg fantasy.Message) map[string]bool {
	ids := make(map[string]bool)
	for _, part := range msg.Content {
		if p, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok && p.ToolCallID != "" {
			ids[p.ToolCallID] = true
		}
	}
	return ids
}

// PerformStructuredCompaction generates a StructuredSummary for messages, merging with prevSummary.
func PerformStructuredCompaction(ctx context.Context, sidecarCompacter SidecarCompacter, messages []fantasy.Message, prevSummary *StructuredSummary, originalGoal string) (*StructuredSummary, error) {
	if originalGoal == "" && prevSummary != nil {
		originalGoal = prevSummary.Goal
	}
	if originalGoal == "" {
		originalGoal = extractFirstUserMessageText(messages)
	}

	var summary StructuredSummary

	if sidecarCompacter != nil {
		convText := formatMessagesForCompaction(messages)
		var prevText string
		if prevSummary != nil {
			prevText = prevSummary.RenderMarkdown()
		}
		raw, err := sidecarCompacter.CompactStructured(ctx, convText, prevText, originalGoal)
		if err == nil && strings.TrimSpace(raw) != "" {
			summary = ParseStructuredSummary(raw)
		}
	}

	// Always run invariant enforcement to merge history and ensure no required items are dropped
	enforced := EnforceCompactionInvariants(&summary, prevSummary, originalGoal, messages)
	if valErr := ValidateStructuredSummary(enforced, prevSummary, messages, nil, nil); valErr != nil {
		log.Printf("warning: post-compaction validation failed (%v); retaining previous summary", valErr)
		if prevSummary != nil {
			return prevSummary, nil
		}
	}
	return enforced, nil
}

// EnforceCompactionInvariants guarantees all 7 invariants on the StructuredSummary.
func EnforceCompactionInvariants(summary *StructuredSummary, prevSummary *StructuredSummary, originalGoal string, messages []fantasy.Message) *StructuredSummary {
	if summary == nil {
		summary = &StructuredSummary{}
	}

	// Invariant 2: Preserve original user goal
	if strings.TrimSpace(summary.Goal) == "" || summary.Goal == "(none)" {
		if prevSummary != nil && strings.TrimSpace(prevSummary.Goal) != "" {
			summary.Goal = prevSummary.Goal
		} else {
			summary.Goal = originalGoal
		}
	}

	// Invariant 3: Preserve latest user correction
	corrections := extractUserCorrections(messages)
	if prevSummary != nil {
		for _, c := range prevSummary.Constraints {
			if strings.Contains(strings.ToLower(c), "user correction") || strings.Contains(strings.ToLower(c), "user feedback") {
				corrections = appendUnique(corrections, c)
			}
		}
		for _, uc := range prevSummary.UserCorrections {
			corrections = appendUnique(corrections, uc)
		}
	}
	for _, corr := range corrections {
		summary.Constraints = appendUnique(summary.Constraints, corr)
		summary.UserCorrections = appendUnique(summary.UserCorrections, corr)
	}

	// Invariant 4: Preserve failed verification
	failures := extractFailedVerifications(messages)
	if prevSummary != nil {
		for _, f := range prevSummary.VerificationResults {
			if isVerificationFailure(f) {
				failures = appendUnique(failures, f)
			}
		}
		for _, ef := range prevSummary.ErrorsAndFixes {
			summary.ErrorsAndFixes = appendUnique(summary.ErrorsAndFixes, ef)
		}
	}
	for _, fail := range failures {
		summary.VerificationResults = appendUnique(summary.VerificationResults, fail)
		summary.ErrorsAndFixes = appendUnique(summary.ErrorsAndFixes, fail)
	}

	// Invariant 5: Preserve artifact path & modified files
	readFiles, modFiles, artifacts := extractFileOperations(messages)
	for _, rf := range summary.FilesRead {
		readFiles = appendUnique(readFiles, rf)
	}
	for _, mf := range summary.FilesModified {
		modFiles = appendUnique(modFiles, mf)
	}
	for _, art := range summary.ArtifactsProduced {
		artifacts = appendUnique(artifacts, art)
	}
	if prevSummary != nil {
		for _, rf := range prevSummary.FilesRead {
			readFiles = appendUnique(readFiles, rf)
		}
		for _, mf := range prevSummary.FilesModified {
			modFiles = appendUnique(modFiles, mf)
		}
		for _, art := range prevSummary.ArtifactsProduced {
			artifacts = appendUnique(artifacts, art)
		}
	}
	summary.FilesRead = readFiles
	summary.FilesModified = modFiles
	summary.ArtifactsProduced = artifacts

	// Invariant 6: Carry over previous completed/in-progress/blocked/decisions/questions/actions/source_entry_ids
	if prevSummary != nil {
		for _, item := range prevSummary.CompletedTasks {
			summary.CompletedTasks = appendUnique(summary.CompletedTasks, item)
		}
		for _, item := range prevSummary.InProgressTasks {
			summary.InProgressTasks = appendUnique(summary.InProgressTasks, item)
		}
		for _, item := range prevSummary.BlockedTasks {
			summary.BlockedTasks = appendUnique(summary.BlockedTasks, item)
		}
		for _, item := range prevSummary.KeyDecisions {
			summary.KeyDecisions = appendUnique(summary.KeyDecisions, item)
		}
		for _, item := range prevSummary.OpenQuestions {
			summary.OpenQuestions = appendUnique(summary.OpenQuestions, item)
		}
		for _, item := range prevSummary.NextActions {
			summary.NextActions = appendUnique(summary.NextActions, item)
		}
		for _, item := range prevSummary.SourceEntryIDs {
			summary.SourceEntryIDs = appendUnique(summary.SourceEntryIDs, item)
		}
	}

	return summary
}

// CompactionValidationError represents a failure during post-compaction validation.
type CompactionValidationError struct {
	Check   string
	Message string
}

func (e *CompactionValidationError) Error() string {
	return fmt.Sprintf("compaction validation failed [%s]: %s", e.Check, e.Message)
}

// ValidateStructuredSummary performs deterministic checks on the post-compaction summary (§6.4).
// Checks:
// 1. Goal validation (summary goal must not be empty)
// 2. Active task IDs preserved (active task IDs must exist in summary task lists)
// 3. Modified artifacts traceable (modified files & artifacts in prevSummary or messages must be present)
// 4. Latest user correction present (user corrections in messages or prevSummary must be present)
// 5. Failed task not marked done (failed tasks or verifications must not be in CompletedTasks)
func ValidateStructuredSummary(summary *StructuredSummary, prevSummary *StructuredSummary, messages []fantasy.Message, activeTaskIDs []string, failedTaskIDs []string) error {
	if summary == nil {
		return &CompactionValidationError{Check: "Goal", Message: "summary is nil"}
	}
	if strings.TrimSpace(summary.Goal) == "" || summary.Goal == "(none)" {
		return &CompactionValidationError{Check: "Goal", Message: "summary goal is empty"}
	}

	// 1. Active task IDs preserved
	for _, activeID := range activeTaskIDs {
		activeID = strings.TrimSpace(activeID)
		if activeID == "" {
			continue
		}
		found := false
		allTasks := append([]string{}, summary.InProgressTasks...)
		allTasks = append(allTasks, summary.BlockedTasks...)
		allTasks = append(allTasks, summary.CompletedTasks...)
		for _, t := range allTasks {
			if strings.Contains(strings.ToLower(t), strings.ToLower(activeID)) {
				found = true
				break
			}
		}
		if !found {
			return &CompactionValidationError{
				Check:   "ActiveTasks",
				Message: fmt.Sprintf("active task ID %q missing from summary task lists", activeID),
			}
		}
	}

	// 2. Modified artifacts traceable
	_, msgModFiles, msgArtifacts := extractFileOperations(messages)
	var expectedArtifacts []string
	expectedArtifacts = append(expectedArtifacts, msgModFiles...)
	expectedArtifacts = append(expectedArtifacts, msgArtifacts...)
	if prevSummary != nil {
		expectedArtifacts = append(expectedArtifacts, prevSummary.FilesModified...)
		expectedArtifacts = append(expectedArtifacts, prevSummary.ArtifactsProduced...)
	}
	for _, expected := range expectedArtifacts {
		expected = strings.TrimSpace(expected)
		if expected == "" || expected == "(none)" {
			continue
		}
		found := false
		for _, actual := range append(summary.FilesModified, summary.ArtifactsProduced...) {
			if strings.EqualFold(actual, expected) {
				found = true
				break
			}
		}
		if !found {
			return &CompactionValidationError{
				Check:   "ArtifactsTraceable",
				Message: fmt.Sprintf("modified file or artifact %q is not traceable in summary", expected),
			}
		}
	}

	// 3. Latest user correction present
	userCorrections := extractUserCorrections(messages)
	if prevSummary != nil {
		userCorrections = append(userCorrections, prevSummary.UserCorrections...)
		for _, c := range prevSummary.Constraints {
			if strings.Contains(strings.ToLower(c), "user correction") || strings.Contains(strings.ToLower(c), "user feedback") {
				userCorrections = append(userCorrections, c)
			}
		}
	}
	if len(userCorrections) > 0 {
		latestCorr := userCorrections[len(userCorrections)-1]
		cleanCorr := strings.TrimPrefix(latestCorr, "User correction: ")
		found := false
		for _, c := range append(summary.Constraints, summary.UserCorrections...) {
			if strings.Contains(strings.ToLower(c), strings.ToLower(cleanCorr)) {
				found = true
				break
			}
		}
		if !found {
			return &CompactionValidationError{
				Check:   "UserCorrection",
				Message: fmt.Sprintf("latest user correction %q missing from summary", latestCorr),
			}
		}
	}

	// 4. Failed task not marked done
	for _, failedID := range failedTaskIDs {
		failedID = strings.TrimSpace(failedID)
		if failedID == "" {
			continue
		}
		for _, completed := range summary.CompletedTasks {
			if strings.Contains(strings.ToLower(completed), strings.ToLower(failedID)) {
				return &CompactionValidationError{
					Check:   "FailedTaskNotDone",
					Message: fmt.Sprintf("failed task %q is incorrectly marked as completed in summary", failedID),
				}
			}
		}
	}

	msgFailures := extractFailedVerifications(messages)
	if prevSummary != nil {
		for _, v := range prevSummary.VerificationResults {
			if isVerificationFailure(v) {
				msgFailures = append(msgFailures, v)
			}
		}
	}
	for _, fail := range msgFailures {
		cleanFail := fail
		for strings.HasPrefix(strings.ToLower(cleanFail), "fail:") {
			cleanFail = strings.TrimSpace(cleanFail[5:])
		}
		cleanFail = strings.TrimSpace(cleanFail)
		if cleanFail == "" {
			continue
		}
		for _, completed := range summary.CompletedTasks {
			if strings.EqualFold(completed, cleanFail) || strings.Contains(strings.ToLower(completed), strings.ToLower(cleanFail)) {
				return &CompactionValidationError{
					Check:   "FailedTaskNotDone",
					Message: fmt.Sprintf("failed verification %q is incorrectly marked as completed in summary", cleanFail),
				}
			}
		}
	}

	return nil
}

func appendUnique(slice []string, item string) []string {
	item = strings.TrimSpace(item)
	if item == "" || item == "(none)" || item == "None" {
		return slice
	}
	for _, existing := range slice {
		if strings.EqualFold(existing, item) {
			return slice
		}
	}
	return append(slice, item)
}

func extractFirstUserMessageText(messages []fantasy.Message) string {
	for _, msg := range messages {
		if msg.Role == fantasy.MessageRoleUser {
			for _, part := range msg.Content {
				if txt, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
					if !strings.HasPrefix(txt.Text, "[Structured Compacted History]") && !strings.HasPrefix(txt.Text, "[Compacted history]") {
						return strings.TrimSpace(txt.Text)
					}
				}
			}
		}
	}
	return ""
}

func extractUserCorrections(messages []fantasy.Message) []string {
	var latestCorrection string
	userCount := 0
	for _, msg := range messages {
		if msg.Role == fantasy.MessageRoleUser {
			userCount++
			if userCount > 1 { // Subsequent user turns are candidate corrections
				for _, part := range msg.Content {
					if txt, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
						t := strings.TrimSpace(txt.Text)
						if t != "" && !strings.HasPrefix(t, "[Structured Compacted History]") {
							if !strings.HasPrefix(t, "User correction:") {
								t = "User correction: " + t
							}
							latestCorrection = t
						}
					}
				}
			}
		}
	}
	if latestCorrection == "" {
		return nil
	}
	return []string{latestCorrection}
}

func extractFailedVerifications(messages []fantasy.Message) []string {
	var failures []string
	for _, msg := range messages {
		for _, part := range msg.Content {
			if trp, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				output, isErr := toolResultOutputText(trp.Output)
				outputLower := strings.ToLower(output)
				if isErr || strings.Contains(outputLower, "[failed]") || strings.Contains(outputLower, "verification failure") || strings.Contains(outputLower, "fail:") || strings.Contains(outputLower, "exit status") {
					preview := output
					if len([]rune(preview)) > 150 {
						preview = string([]rune(preview)[:150]) + "..."
					}
					failures = append(failures, fmt.Sprintf("%s%s", verificationFailurePrefix, preview))
				}
			}
		}
	}
	return failures
}

func extractFileOperations(messages []fantasy.Message) (readFiles, modifiedFiles, artifacts []string) {
	for _, msg := range messages {
		for _, part := range msg.Content {
			if tcp, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				name := strings.ToLower(tcp.ToolName)
				var args map[string]interface{}
				_ = json.Unmarshal([]byte(tcp.Input), &args)

				extractArg := func(keys ...string) string {
					for _, k := range keys {
						if val, ok := args[k].(string); ok && val != "" {
							return val
						}
					}
					return ""
				}

				switch name {
				case "view", "read_file":
					path := extractArg("file_path", "target_file", "path", "AbsolutePath", "SearchPath", "DirectoryPath")
					if path != "" {
						readFiles = appendUnique(readFiles, path)
					}
				case "grep", "glob", "ls":
					path := extractArg("path", "SearchPath", "DirectoryPath", "file_path")
					if path != "" {
						readFiles = appendUnique(readFiles, path)
					}
				case "download", "write", "edit", "multiedit", "write_to_file", "replace_file_content":
					path := extractArg("file_path", "target_file", "path", "TargetFile", "AbsolutePath")
					if path != "" {
						modifiedFiles = appendUnique(modifiedFiles, path)
						artifacts = appendUnique(artifacts, path)
					}
				}
			}
		}
	}
	return readFiles, modifiedFiles, artifacts
}

func isVerificationFailure(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	return strings.HasPrefix(lower, "fail:") || strings.HasPrefix(lower, "verification failure:") || strings.Contains(lower, "verification failed")
}

func formatMessagesForCompaction(messages []fantasy.Message) string {
	var sb strings.Builder
	for _, msg := range messages {
		fmt.Fprintf(&sb, "[%s]:\n", msg.Role)
		for _, part := range msg.Content {
			switch part.GetType() {
			case fantasy.ContentTypeText:
				if p, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
					sb.WriteString(p.Text + "\n")
				}
			case fantasy.ContentTypeToolCall:
				if p, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
					fmt.Fprintf(&sb, "ToolCall(%s, args: %s)\n", p.ToolName, p.Input)
				}
			case fantasy.ContentTypeToolResult:
				if p, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
					txt, _ := toolResultOutputText(p.Output)
					if len([]rune(txt)) > 500 {
						txt = string([]rune(txt)[:500]) + "..."
					}
					fmt.Fprintf(&sb, "ToolResult(%s): %s\n", p.ToolCallID, txt)
				}
			}
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// countTokensInText estimates tokens for text using the model-aware token counter.
// modelID selects the estimator family (e.g. "qwen3", "gpt-4o"); "" falls back to
// the registry default. This makes compaction token accounting model-aware so the
// tokens_before/after recorded in CompactionRecord reflect the same estimator used
// for context budgeting (§5.3).
func countTokensInText(modelID, text string) int {
	if modelID == "" {
		modelID = "default"
	}
	tokens, err := defaultCounter.CountText(context.Background(), modelID, text)
	if err != nil || tokens == 0 {
		runes := []rune(text)
		if len(runes) > 0 {
			return 1
		}
		return 0
	}
	return tokens
}

// countTokensInMessages estimates tokens for a message slice using the model-aware
// token counter. See countTokensInText for the modelID semantics.
func countTokensInMessages(modelID string, messages []fantasy.Message) int {
	if modelID == "" {
		modelID = "default"
	}
	tokens, err := defaultCounter.CountMessages(context.Background(), modelID, messages)
	if err != nil || tokens == 0 {
		if len(messages) > 0 {
			return 1
		}
		return 0
	}
	return tokens
}

// SaveCompactionRecord persists a CompactionRecord to workspace/compaction_history.json.
func SaveCompactionRecord(workspace string, record CompactionRecord) error {
	if workspace == "" {
		return nil
	}
	history, _ := LoadCompactionHistory(workspace)
	history = append(history, record)

	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal compaction history: %w", err)
	}

	path := filepath.Join(workspace, CompactionHistoryFile)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write compaction history: %w", err)
	}
	return nil
}

// LoadCompactionHistory loads past CompactionRecord entries from workspace.
func LoadCompactionHistory(workspace string) ([]CompactionRecord, error) {
	if workspace == "" {
		return nil, nil
	}
	path := filepath.Join(workspace, CompactionHistoryFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var history []CompactionRecord
	if err := json.Unmarshal(data, &history); err != nil {
		return nil, fmt.Errorf("unmarshal compaction history: %w", err)
	}
	return history, nil
}

// GetLatestCompactionSummary loads the most recent StructuredSummary from workspace history.
func GetLatestCompactionSummary(workspace string) *StructuredSummary {
	history, err := LoadCompactionHistory(workspace)
	if err != nil || len(history) == 0 {
		return nil
	}
	latest := history[len(history)-1].Summary
	return &latest
}

// DeleteCompactionHistory removes compaction_history.json from workspace.
func DeleteCompactionHistory(workspace string) error {
	if workspace == "" {
		return nil
	}
	path := filepath.Join(workspace, CompactionHistoryFile)
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
