package team

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/utils"
)

const stmFile = "stm.md"

const maxSTMChars = 4000

// Keep enough entries to make concurrent task updates observable. Size is
// bounded by TruncateSTM, which preserves critical sections rather than
// silently dropping older entries at append time.
const maxEntriesPerSTMSection = 100

const (
	stmSectionProgress  = "# 進度"
	stmSectionFindings  = "# 發現"
	stmSectionDecisions = "# 決策"
	stmSectionErrors    = "# 錯誤與修復"
	stmSectionQuestions = "# 待解決"
)

var stmSectionOrder = []string{stmSectionProgress, stmSectionFindings, stmSectionDecisions, stmSectionErrors, stmSectionQuestions}

// stmWriters serializes read-modify-write updates per STM path, including
// updates made by distinct Coordinators in the same process.
var stmWriters sync.Map // map[string]*sync.Mutex

// STMWriter is the sole read-modify-write abstraction for stm.md. SaveSTM is
// retained for lifecycle operations; merging existing content must use Update.
type STMWriter struct {
	workspace string
	mu        *sync.Mutex
}

func NewSTMWriter(workspace string) *STMWriter {
	path := STMPath(workspace)
	mu, _ := stmWriters.LoadOrStore(path, &sync.Mutex{})
	return &STMWriter{workspace: workspace, mu: mu.(*sync.Mutex)}
}

func (w *STMWriter) Update(fn func(string) string) error {
	if w == nil || strings.TrimSpace(w.workspace) == "" {
		return fmt.Errorf("invalid STM workspace")
	}
	if fn == nil {
		return fmt.Errorf("STM update callback is required")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return SaveSTM(w.workspace, fn(LoadSTM(w.workspace)))
}

type STMSection struct {
	Title   string
	Entries []string
}

func ParseSTMSections(content string) []STMSection {
	if content == "" {
		return nil
	}
	var sections []STMSection
	var current *STMSection
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "# ") {
			if current != nil {
				sections = append(sections, *current)
			}
			current = &STMSection{Title: trimmed, Entries: nil}
			continue
		}
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			if current != nil {
				current.Entries = append(current.Entries, trimmed)
			}
		}
	}
	if current != nil {
		sections = append(sections, *current)
	}
	return sections
}

func FormatSTMSections(sections []STMSection) string {
	if len(sections) == 0 {
		return ""
	}
	var b strings.Builder
	for i, s := range sections {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(s.Title)
		for _, e := range s.Entries {
			b.WriteString("\n")
			b.WriteString(e)
		}
	}
	return b.String()
}

func appendSTMEntry(content string, entry string, sectionTitle string) string {
	sections := ParseSTMSections(content)

	targetIdx := -1
	for i, s := range sections {
		if s.Title == sectionTitle {
			targetIdx = i
			break
		}
	}

	if targetIdx == -1 {
		for i, s := range sections {
			for _, known := range stmSectionOrder {
				if s.Title == known {
					if known == sectionTitle {
						targetIdx = i
						break
					}
				}
			}
			if targetIdx != -1 {
				break
			}
		}
	}

	if targetIdx == -1 {
		sections = append(sections, STMSection{Title: sectionTitle, Entries: []string{entry}})
	} else {
		sections[targetIdx].Entries = append([]string{entry}, sections[targetIdx].Entries...)
		if len(sections[targetIdx].Entries) > maxEntriesPerSTMSection {
			sections[targetIdx].Entries = sections[targetIdx].Entries[:maxEntriesPerSTMSection]
		}
	}

	return FormatSTMSections(sections)
}

func formatSTMDoneEntry(agentName, taskDesc, summary string) string {
	shortDesc := utils.TruncateRunes(taskDesc, 80)
	shortSummary := summary
	if shortSummary != "" {
		shortSummary = ": " + utils.TruncateRunes(shortSummary, 120)
	}
	return strings.TrimSpace(fmt.Sprintf("- %s %s%s", agentName, shortDesc, shortSummary))
}

func formatSTMErrorEntry(agentName, taskDesc, errMsg string) string {
	shortDesc := utils.TruncateRunes(taskDesc, 80)
	shortErr := utils.TruncateRunes(errMsg, 120)
	return strings.TrimSpace(fmt.Sprintf("- [FAILED] %s %s: %s", agentName, shortDesc, shortErr))
}

func formatSTMFinding(agentName, finding string) string {
	shortFinding := finding
	if len([]rune(shortFinding)) > 120 {
		shortFinding = string([]rune(shortFinding)[:120]) + "..."
	}
	return strings.TrimSpace(fmt.Sprintf("- %s: %s", agentName, shortFinding))
}

func formatSTMDecision(agentName, decision string) string {
	shortDecision := decision
	if len([]rune(shortDecision)) > 120 {
		shortDecision = string([]rune(shortDecision)[:120]) + "..."
	}
	return strings.TrimSpace(fmt.Sprintf("- %s: %s", agentName, shortDecision))
}

func filterSTMSectionsByRole(sections []STMSection, role string) []STMSection {
	if role == "" || role == "coordinator" || role == "orchestrator" {
		return sections
	}
	visible := make(map[string]bool)
	switch role {
	case "researcher", "explorer", "investigator":
		visible[stmSectionFindings] = true
		visible[stmSectionErrors] = true
	case "writer", "developer", "coder":
		visible[stmSectionProgress] = true
		visible[stmSectionFindings] = true
	case "reviewer", "tester", "qa":
		visible[stmSectionProgress] = true
		visible[stmSectionFindings] = true
		visible[stmSectionErrors] = true
	default:
		visible[stmSectionProgress] = true
		visible[stmSectionFindings] = true
	}
	// Decisions and open questions are cross-cutting knowledge: every role must
	// see them so agents don't re-make settled decisions or miss blockers.
	// Without this, e.g. a coder could not see a researcher's findings and would
	// redo work — the exact overlap this filter is meant to prevent.
	visible[stmSectionDecisions] = true
	visible[stmSectionQuestions] = true
	var filtered []STMSection
	for _, s := range sections {
		if visible[s.Title] {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

func filterLTMSectionsByPrompt(sections []STMSection, prompt string) []STMSection {
	if len(sections) == 0 || prompt == "" {
		return sections
	}
	promptLower := strings.ToLower(prompt)
	var filtered []STMSection
	for _, s := range sections {
		var relevant []string
		for _, e := range s.Entries {
			if strings.Contains(promptLower, strings.ToLower(utils.TruncateRunes(e, 80))) {
				relevant = append(relevant, e)
			}
		}
		if len(relevant) > 0 {
			if len(relevant) > 3 {
				relevant = relevant[:3]
			}
			filtered = append(filtered, STMSection{Title: s.Title, Entries: relevant})
		}
	}
	return filtered
}

func STMPath(workspace string) string {
	return filepath.Join(workspace, stmFile)
}

func LoadSTM(workspace string) string {
	data, err := os.ReadFile(STMPath(workspace))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func SaveSTM(workspace string, content string) error {
	return AtomicWriteFile(STMPath(workspace), []byte(utils.RedactSecrets(content)), 0o600)
}

func TruncateSTM(content string) string {
	runes := []rune(content)
	if len(runes) <= maxSTMChars {
		return content
	}
	sections := ParseSTMSections(content)
	if len(sections) == 0 {
		return truncateSTMTextWithAnchors(content, maxSTMChars)
	}

	// Preserve critical state before lower-priority narrative. Entries are
	// newest-first, so recent progress remains available within each section.
	priority := map[string]int{stmSectionErrors: 0, stmSectionDecisions: 1, stmSectionQuestions: 2, stmSectionFindings: 3, stmSectionProgress: 4}
	sectionPriority := func(title string) int {
		if value, ok := priority[title]; ok {
			return value
		}
		return len(priority) + 1
	}
	ordered := append([]STMSection(nil), sections...)
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if sectionPriority(ordered[j].Title) < sectionPriority(ordered[i].Title) {
				ordered[i], ordered[j] = ordered[j], ordered[i]
			}
		}
	}
	kept := make(map[string][]string, len(sections))
	used := 0
	for _, section := range ordered {
		for _, entry := range section.Entries {
			remaining := maxSTMChars - used - len([]rune(section.Title)) - 3
			if remaining <= 0 {
				break
			}
			candidate := entry
			if len([]rune(candidate)) > remaining {
				candidate = truncateSTMTextWithAnchors(candidate, remaining)
			}
			if candidate == "" {
				continue
			}
			kept[section.Title] = append(kept[section.Title], candidate)
			used += len([]rune(candidate)) + 1
		}
	}
	result := make([]STMSection, 0, len(sections))
	for _, section := range sections { // retain familiar deterministic order
		if entries := kept[section.Title]; len(entries) > 0 {
			result = append(result, STMSection{Title: section.Title, Entries: entries})
		}
	}
	return truncateSTMTextWithAnchors(FormatSTMSections(result), maxSTMChars)
}

var stmAnchorPattern = regexp.MustCompile("(?im)`[^`]+`|(?:^|\\s)(?:go|git|npm|pnpm|yarn|make|cargo|python|bash)\\s+[^\\n]+|(?:^|\\s)(?:\\.?\\.?/|/)[A-Za-z0-9_./:@=-]+|(?:error|failed|panic|fatal)[^\\n]*")

// truncateSTMTextWithAnchors retains exact commands, paths, and error text
// while shortening a single oversized section entry.
func truncateSTMTextWithAnchors(text string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	marker := "\n...[STM content truncated; anchors retained]...\n"
	budget := maxChars - len([]rune(marker))
	if budget <= 0 {
		return string(runes[:maxChars])
	}
	head, tail := budget/2, budget-budget/2
	parts := []string{string(runes[:head]), marker}
	for _, anchor := range stmAnchorPattern.FindAllString(text, -1) {
		anchor = strings.TrimSpace(anchor)
		candidate := strings.Join(append(parts, anchor), "\n") + "\n" + string(runes[len(runes)-tail:])
		if anchor == "" || strings.Contains(parts[0], anchor) || strings.Contains(string(runes[len(runes)-tail:]), anchor) || len([]rune(candidate)) > maxChars {
			continue
		}
		parts = append(parts, anchor)
	}
	out := strings.Join(append(parts, string(runes[len(runes)-tail:])), "\n")
	if outRunes := []rune(out); len(outRunes) > maxChars {
		return string(outRunes[:maxChars])
	}
	return out
}

func ArchiveSTM(workspace string) (string, error) {
	stmContent := LoadSTM(workspace)
	if stmContent == "" {
		return "", nil
	}

	histDir := filepath.Join(workspace, historyDirName)
	if err := os.MkdirAll(histDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create history directory: %w", err)
	}

	ts := time.Now().Format("20060102T150405")
	filename := fmt.Sprintf("%s-stm.md", ts)
	path := filepath.Join(histDir, filename)
	if err := AtomicWriteFile(path, []byte(stmContent), 0o644); err != nil {
		return "", fmt.Errorf("failed to archive stm.md: %w", err)
	}

	if err := removeFileIfExists(STMPath(workspace)); err != nil {
		return path, fmt.Errorf("failed to remove stm.md after archive: %w", err)
	}

	return path, nil
}

func InitSTM(workspace string) error {
	// Archive any existing stm.md to history/ before clearing. Workers only
	// see entries written in the current invocation; cross-session knowledge
	// accumulates in history/*-stm.md and is extracted to ltm.md on --new.
	if _, err := ArchiveSTM(workspace); err != nil {
		fmt.Printf("warning: failed to archive stm.md: %v\n", err)
	}
	return SaveSTM(workspace, "")
}
