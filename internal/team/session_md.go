package team

import (
	"fmt"
	"os"

	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anomalyco/hufu/internal/utils"
)

const sessionMDFile = "chat_history.md"

// MaxSessionHistoryFiles bounds how many archived chat-history transcripts
// (see ArchiveSessionMD) PruneSessionHistory keeps.
const MaxSessionHistoryFiles = 20

func SessionMDPath(workspace string) string {
	return filepath.Join(workspace, sessionMDFile)
}

func LoadSessionMD(workspace string) string {
	data, err := os.ReadFile(SessionMDPath(workspace))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func SaveSessionMD(workspace string, content string) error {
	return AtomicWriteFile(SessionMDPath(workspace), []byte(content), 0o644)
}

func GenerateSessionMD(sd *SessionData, teamName string) string {
	if sd == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Session — %s\n\n", teamName)
	fmt.Fprintf(&b, "**Started:** %s  \n", sd.CreatedAt)
	fmt.Fprintf(&b, "**Last activity:** %s  \n", sd.UpdatedAt)
	fmt.Fprintf(&b, "**Rounds:** %d  \n", sd.Rounds)
	fmt.Fprintf(&b, "**Exchanges:** %d\n\n", len(sd.Entries))
	b.WriteString("---\n\n")
	start := 0
	if len(sd.Entries) > maxSessionEntries {
		start = len(sd.Entries) - maxSessionEntries
		fmt.Fprintf(&b, "*... %d older exchanges omitted*\n\n", start)
	}
	for _, entry := range sd.Entries[start:] {
		role := "🧑 User"
		if entry.Role == "assistant" {
			role = "🤖 Coordinator"
		}
		content := utils.TruncateRunes(entry.Content, 1000)
		fmt.Fprintf(&b, "### %s", role)
		if entry.Timestamp != "" {
			fmt.Fprintf(&b, " (%s)", entry.Timestamp)
		}
		b.WriteString("\n\n")
		b.WriteString(content)
		b.WriteString("\n\n")
	}
	return b.String()
}

func ArchiveSessionMD(workspace string) (string, error) {
	mdContent := LoadSessionMD(workspace)
	if mdContent == "" {
		return "", nil
	}

	histDir := filepath.Join(workspace, historyDirName)
	if err := os.MkdirAll(histDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create history directory: %w", err)
	}

	ts := time.Now().Format("2006-01-02")
	shortDesc := "session"
	lines := strings.SplitN(mdContent, "\n", 5)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "#")
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "**") {
			slug := strings.ToLower(line)
			slug = strings.ReplaceAll(slug, " ", "-")
			slug = strings.ReplaceAll(slug, "/", "-")
			slug = strings.ReplaceAll(slug, "—", "-")
			// The title line is typically "Session — <team>": the em-dash is
			// surrounded by spaces, so the two replacements above each turn
			// a neighboring space into "-", stacking into "---" before this
			// collapses runs of dashes back down to one.
			for strings.Contains(slug, "--") {
				slug = strings.ReplaceAll(slug, "--", "-")
			}
			if len(slug) > 50 {
				slug = slug[:50]
			}
			slug = strings.Trim(slug, "-. ")
			if slug != "" {
				shortDesc = slug
			}
			break
		}
	}

	filename := fmt.Sprintf("%s-%s.md", ts, shortDesc)
	path := filepath.Join(histDir, filename)
	if err := os.WriteFile(path, []byte(mdContent), 0o644); err != nil {
		return "", fmt.Errorf("failed to write history file: %w", err)
	}

	if err := removeFileIfExists(SessionMDPath(workspace)); err != nil {
		return path, fmt.Errorf("failed to remove session md: %w", err)
	}
	if err := removeFileIfExists(filepath.Join(workspace, sessionFile)); err != nil {
		return path, fmt.Errorf("failed to remove session json: %w", err)
	}
	return path, nil
}

// PruneSessionHistory keeps at most keep of the archived chat-history
// transcripts written by ArchiveSessionMD, deleting the oldest first.
// Unlike history/*-stm.md snapshots — which ExtractLTMFromHistory distills
// into ltm.md and then deletes — these are raw transcripts with no
// structured extraction path, so nothing else ever removes them; left
// alone they accumulate in the workspace forever.
func PruneSessionHistory(workspace string, keep int) {
	if keep < 0 {
		return
	}
	histDir := filepath.Join(workspace, historyDirName)
	entries, err := os.ReadDir(histDir)
	if err != nil {
		return
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), "-stm.md") {
			continue
		}
		files = append(files, e.Name())
	}
	if len(files) <= keep {
		return
	}
	// Filenames are date-prefixed ("YYYY-MM-DD-<slug>.md"), so lexical order
	// is chronological order.
	sort.Strings(files)
	for _, name := range files[:len(files)-keep] {
		_ = os.Remove(filepath.Join(histDir, name))
	}
}
