package team

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const sessionMDFile = "session.md"

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
	return os.WriteFile(SessionMDPath(workspace), []byte(content), 0o644)
}

func GenerateSessionMD(sd *SessionData, teamName string) string {
	if sd == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# Session — %s\n\n", teamName))
	b.WriteString(fmt.Sprintf("**Started:** %s  \n", sd.CreatedAt))
	b.WriteString(fmt.Sprintf("**Last activity:** %s  \n", sd.UpdatedAt))
	b.WriteString(fmt.Sprintf("**Rounds:** %d  \n", sd.Rounds))
	b.WriteString(fmt.Sprintf("**Exchanges:** %d\n\n", len(sd.Entries)))
	b.WriteString("---\n\n")
	for i, entry := range sd.Entries {
		if i >= maxSessionEntries {
			remaining := len(sd.Entries) - i
			b.WriteString(fmt.Sprintf("*... %d earlier exchanges omitted*\n\n", remaining))
			break
		}
		role := "🧑 User"
		if entry.Role == "assistant" {
			role = "🤖 Coordinator"
		}
		content := truncateString(entry.Content, 1000)
		b.WriteString(fmt.Sprintf("### %s", role))
		if entry.Timestamp != "" {
			b.WriteString(fmt.Sprintf(" (%s)", entry.Timestamp))
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
