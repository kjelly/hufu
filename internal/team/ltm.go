package team

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/utils"
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

// ltmDatePrefixRe matches a leading "- [YYYY-MM-DD]" (bullet optional) so
// entries re-ingested from STM or echoed back by the model don't accumulate
// stacked date prefixes.
var ltmDatePrefixRe = regexp.MustCompile(`^\s*(?:-\s*)?\[\d{4}-\d{2}-\d{2}\]\s*`)

func formatLTMEntry(content string) string {
	ts := time.Now().Format("2006-01-02")
	shortContent := strings.TrimSpace(content)
	for {
		stripped := ltmDatePrefixRe.ReplaceAllString(shortContent, "")
		if stripped == shortContent {
			break
		}
		shortContent = stripped
	}
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
	// Strip every stacked "[date] " prefix, not just the first, so entries
	// that accumulated double prefixes still collapse to the same key.
	for {
		stripped := ltmDatePrefixRe.ReplaceAllString(s, "")
		if stripped == s {
			break
		}
		s = stripped
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
	return os.WriteFile(LTMPath(workspace, teamName), []byte(utils.RedactSecrets(content)), 0o600)
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
	sections := ParseSTMSections(content)
	if len(sections) == 0 {
		// No recognizable "# section" structure (e.g. free-form text) — fall
		// back to a raw tail cut, dropping the partial first line so
		// truncation never leaves a torn entry.
		tail := string(runes[len(runes)-maxLTMChars:])
		if idx := strings.IndexByte(tail, '\n'); idx >= 0 && idx < len(tail)-1 {
			tail = tail[idx+1:]
		}
		return tail
	}
	// Sections render in a fixed order (Conventions, Architecture, Patterns,
	// Issues, Files, Tools); a raw tail cut on the whole rendered string
	// would wipe out entire early sections before ever touching a single
	// stale entry in a later one, which has nothing to do with importance.
	// Instead drop the oldest (last, since entries are newest-first) entry
	// from whichever section currently holds the most entries, repeating
	// until the render fits.
	render := func() string {
		var kept []STMSection
		for _, s := range sections {
			if len(s.Entries) > 0 {
				kept = append(kept, s)
			}
		}
		return FormatSTMSections(kept)
	}
	for len([]rune(render())) > maxLTMChars {
		largest := -1
		for i, s := range sections {
			if len(s.Entries) == 0 {
				continue
			}
			if largest == -1 || len(s.Entries) > len(sections[largest].Entries) {
				largest = i
			}
		}
		if largest == -1 {
			break
		}
		sections[largest].Entries = sections[largest].Entries[:len(sections[largest].Entries)-1]
	}
	return render()
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

// ClassifyLTMEntry maps an already typed STM source to an LTM section. Entry
// prose is deliberately ignored: deterministic code must not infer semantic
// categories from language-specific words or file-looking substrings.
func ClassifyLTMEntry(_ string, source string) string {
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

func ltmSectionForCategory(category string) string {
	switch strings.ToLower(strings.TrimSpace(category)) {
	case "decision", "architecture":
		return ltmSectionArchitecture
	case "convention", "requirement", "instruction":
		return ltmSectionConventions
	case "issue", "error", "lesson":
		return ltmSectionIssues
	case "artifact":
		return ltmSectionFiles
	case "verification":
		return ltmSectionTools
	case "pattern", "observation", "finding", "summary", "":
		return ltmSectionPatterns
	default:
		return ""
	}
}
