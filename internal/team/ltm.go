package team

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const ltmFile = "ltm.md"

const maxLTMChars = 6000

const maxEntriesPerLTMSection = 10

const (
	ltmSectionConventions  = "# 專案慣例"
	ltmSectionArchitecture = "# 架構決策"
	ltmSectionPatterns     = "# 常見模式"
	ltmSectionIssues       = "# 已知問題與解法"
	ltmSectionFiles        = "# 關鍵檔案"
	ltmSectionTools        = "# 工具與指令"
)

var ltmSectionOrder = []string{ltmSectionConventions, ltmSectionArchitecture, ltmSectionPatterns, ltmSectionIssues, ltmSectionFiles, ltmSectionTools}

var ltmSectionDefaults = map[string]string{
	ltmSectionConventions:  "Use this section to record project conventions (coding style, naming rules, workflows).",
	ltmSectionArchitecture: "Use this section to record architecture decisions (component layout, technology choices, data flow).",
	ltmSectionPatterns:     "Use this section to record recurring patterns (common request flows, error handling, integration points).",
	ltmSectionIssues:       "Use this section to record known issues and their solutions or workarounds.",
	ltmSectionFiles:        "Use this section to record key files and their purpose.",
	ltmSectionTools:        "Use this section to record commonly used tools, commands, and scripts.",
}

func formatLTMEntry(content string) string {
	ts := time.Now().Format("2006-01-02")
	shortContent := content
	if len([]rune(shortContent)) > 200 {
		shortContent = string([]rune(shortContent)[:200]) + "..."
	}
	return fmt.Sprintf("- [%s] %s", ts, shortContent)
}

func appendLTMEntry(content string, entry string, sectionTitle string) string {
	return appendSTMEntry(content, entry, sectionTitle)
}

func PruneLTM(content string) string {
	sections := ParseSTMSections(content)
	if len(sections) == 0 {
		return content
	}
	var pruned []STMSection
	for _, s := range sections {
		if len(s.Entries) > maxEntriesPerLTMSection {
			s.Entries = s.Entries[:maxEntriesPerLTMSection]
		}
		s.Entries = deduplicateLTREntries(s.Entries)
		if len(s.Entries) > 0 {
			pruned = append(pruned, s)
		}
	}
	return FormatSTMSections(pruned)
}

func deduplicateLTREntries(entries []string) []string {
	if len(entries) <= 1 {
		return entries
	}
	seen := make(map[string]bool)
	var result []string
	for _, e := range entries {
		key := normalizeLTREntry(e)
		if !seen[key] {
			seen[key] = true
			result = append(result, e)
		}
	}
	return result
}

func normalizeLTREntry(entry string) string {
	s := strings.TrimSpace(entry)
	if idx := strings.Index(s, "] "); idx >= 0 {
		s = s[idx+2:]
	}
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '_' {
			return ' '
		}
		return r
	}, s)
	var b strings.Builder
	for _, word := range strings.Fields(s) {
		if len(word) > 3 {
			b.WriteString(word)
			b.WriteByte(' ')
		}
	}
	return strings.TrimSpace(b.String())
}

func LTMPath(teamDir string) string {
	return filepath.Join(teamDir, ltmFile)
}

func LoadLTM(teamDir string) string {
	data, err := os.ReadFile(LTMPath(teamDir))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func SaveLTM(teamDir string, content string) error {
	return os.WriteFile(LTMPath(teamDir), []byte(content), 0o644)
}

func TruncateLTM(content string) string {
	runes := []rune(content)
	if len(runes) <= maxLTMChars {
		return content
	}
	return string(runes[len(runes)-maxLTMChars:])
}

func InitLTM(teamDir string) error {
	path := LTMPath(teamDir)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, []byte(""), 0o644)
}

// extractLTMFromContent merges knowledge from one STM snapshot (stmContent)
// into existingLTM and returns the updated LTM string. Deduplication is
// handled later by PruneLTM → deduplicateLTREntries.
func extractLTMFromContent(stmContent, existingLTM string) string {
	for _, s := range ParseSTMSections(stmContent) {
		var source, defaultSection string
		switch s.Title {
		case stmSectionDecisions:
			source, defaultSection = "decision", ltmSectionArchitecture
		case stmSectionFindings:
			source, defaultSection = "finding", ltmSectionPatterns
		case stmSectionErrors:
			source, defaultSection = "error", ltmSectionIssues
		default:
			continue
		}
		for _, e := range s.Entries {
			sec := classifyLTMEntry(e, source)
			if sec == "" {
				sec = defaultSection
			}
			existingLTM = appendSTMEntry(existingLTM, formatLTMEntry(stripSTMListItem(e)), sec)
		}
	}
	return existingLTM
}

// ExtractLTMFromHistory reads every history/*-stm.md file, extracts knowledge
// into ltm.md, then deletes the history files. Called on --new startup so that
// accumulated session snapshots are distilled into long-term memory.
func ExtractLTMFromHistory(workspace, teamDir string) {
	histDir := filepath.Join(workspace, historyDirName)
	entries, err := os.ReadDir(histDir)
	if err != nil {
		return
	}

	ltm := LoadLTM(teamDir)
	anyContent := false

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "-stm.md") {
			continue
		}
		path := filepath.Join(histDir, e.Name())
		data, readErr := os.ReadFile(path)
		content := strings.TrimSpace(string(data))
		if readErr == nil && content != "" {
			ltm = extractLTMFromContent(content, ltm)
			anyContent = true
		}
		os.Remove(path)
	}

	if !anyContent {
		return
	}

	if err := SaveLTM(teamDir, TruncateLTM(PruneLTM(ltm))); err != nil {
		fmt.Printf("warning: LTM extraction from history failed: %v\n", err)
	}
}
