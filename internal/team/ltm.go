package team

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var safeNameRegex = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

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

func LTMPath(workspace, teamName string) string {
	// Sanitize teamName to prevent path traversal and invalid characters
	safeName := safeNameRegex.ReplaceAllString(teamName, "-")
	safeName = strings.TrimPrefix(safeName, "-")
	safeName = strings.TrimSuffix(safeName, "-")
	if safeName == "" {
		safeName = "default"
	}
	return filepath.Join(workspace, fmt.Sprintf("ltm-%s.md", safeName))
}

func LoadLTM(workspace, teamName string) string {
	data, err := os.ReadFile(LTMPath(workspace, teamName))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func SaveLTM(workspace, teamName, content string) error {
	return os.WriteFile(LTMPath(workspace, teamName), []byte(content), 0o644)
}

func InitLTM(workspace, teamName string) error {
	path := LTMPath(workspace, teamName)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, []byte(""), 0o644)
}

func TruncateLTM(content string) string {
	runes := []rune(content)
	if len(runes) <= maxLTMChars {
		return content
	}
	return string(runes[len(runes)-maxLTMChars:])
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
			sec := ClassifyLTMEntry(e, source)
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
func ExtractLTMFromHistory(workspace, teamName string) {
	histDir := filepath.Join(workspace, historyDirName)
	entries, err := os.ReadDir(histDir)
	if err != nil {
		return
	}

	ltm := LoadLTM(workspace, teamName)
	anyContent := false

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "-stm.md") {
			continue
		}
		path := filepath.Join(histDir, e.Name())
		data, readErr := os.ReadFile(path)
		if readErr == nil {
			content := strings.TrimSpace(string(data))
			if content != "" {
				ltm = extractLTMFromContent(content, ltm)
				anyContent = true
			}
			os.Remove(path)
		}
	}

	if !anyContent {
		return
	}

	if err := SaveLTM(workspace, teamName, TruncateLTM(PruneLTM(ltm))); err != nil {
		fmt.Printf("warning: LTM extraction from history failed: %v\n", err)
	}
}

// ClassifyLTMEntry classifies an STM entry into an LTM section based on content and source.
// This function is exported for use by both ltm.go and coordinator.go.
func ClassifyLTMEntry(entry string, source string) string {
	lower := strings.ToLower(entry)
	hasFileExtension := strings.Contains(lower, ".go") || strings.Contains(lower, ".yaml") ||
		strings.Contains(lower, ".yml") || strings.Contains(lower, ".md") ||
		strings.Contains(lower, ".json") || strings.Contains(lower, ".sh") ||
		strings.Contains(lower, ".py") || strings.Contains(lower, ".js") ||
		strings.Contains(lower, ".ts") || strings.Contains(lower, ".tsx") ||
		strings.Contains(lower, ".css") || strings.Contains(lower, ".html") ||
		strings.Contains(lower, ".sql") || strings.Contains(lower, ".toml") ||
		strings.Contains(lower, ".lock") || strings.Contains(lower, ".sum")
	hasPathStructure := strings.Count(lower, "/") >= 2 ||
		strings.Contains(lower, "internal/") || strings.Contains(lower, "pkg/") ||
		strings.Contains(lower, "cmd/") || strings.Contains(lower, "src/") ||
		strings.Contains(lower, "lib/") || strings.Contains(lower, "app/")
	hasFilePath := hasFileExtension || hasPathStructure

	if source == "finding" && hasFilePath {
		return ltmSectionFiles
	}

	if strings.Contains(lower, "always") || strings.Contains(lower, "never") ||
		strings.Contains(lower, "must ") || strings.Contains(lower, "should ") ||
		strings.Contains(lower, "convention") || strings.Contains(lower, "rule") ||
		strings.Contains(lower, "standard") || strings.Contains(lower, "guideline") ||
		strings.Contains(lower, "every time") ||
		strings.Contains(entry, "慣例") || strings.Contains(entry, "規範") ||
		strings.Contains(entry, "必須") || strings.Contains(entry, "不可") ||
		strings.Contains(entry, "應該") || strings.Contains(entry, "每次") {
		return ltmSectionConventions
	}

	if strings.Contains(lower, "pattern") || strings.Contains(lower, "approach") ||
		strings.Contains(lower, "strategy") || strings.Contains(lower, "workflow") ||
		strings.Contains(lower, "pipeline") || strings.Contains(lower, "template") ||
		strings.Contains(entry, "模式") || strings.Contains(entry, "做法") ||
		strings.Contains(entry, "流程") || strings.Contains(entry, "步驟") {
		return ltmSectionPatterns
	}

	if (source == "error" && strings.Contains(lower, "fix")) ||
		strings.Contains(lower, "solved") || strings.Contains(lower, "resolved") ||
		strings.Contains(lower, "workaround") || strings.Contains(lower, "solution") ||
		strings.Contains(entry, "修復") || strings.Contains(entry, "解決") ||
		strings.Contains(entry, "問題") || strings.Contains(entry, "錯誤") ||
		strings.Contains(entry, "失敗") || strings.Contains(entry, "繞過") {
		return ltmSectionIssues
	}

	if source == "decision" ||
		strings.Contains(entry, "決策") || strings.Contains(entry, "架構") ||
		strings.Contains(entry, "選擇") || strings.Contains(entry, "採用") ||
		strings.Contains(entry, "改用") || strings.Contains(entry, "遷移") {
		return ltmSectionArchitecture
	}

	if strings.Contains(lower, "tool") || strings.Contains(lower, "command") ||
		strings.Contains(lower, "script") || strings.Contains(lower, "cli ") ||
		strings.Contains(lower, "run ") || strings.Contains(lower, "install ") ||
		strings.Contains(lower, "build ") || strings.Contains(lower, "test ") ||
		strings.Contains(entry, "指令") || strings.Contains(entry, "命令") ||
		strings.Contains(entry, "工具") || strings.Contains(entry, "腳本") {
		return ltmSectionTools
	}

	switch source {
	case "finding":
		return ltmSectionPatterns
	case "error":
		return ltmSectionIssues
	case "decision":
		return ltmSectionArchitecture
	default:
		return ltmSectionPatterns
	}
}
