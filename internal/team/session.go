package team

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const sessionFile = "session.json"
const historyDirName = "history"

type SessionEntry struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp,omitempty"`
}

type SessionData struct {
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
	Rounds    int            `json:"rounds"`
	Entries   []SessionEntry `json:"entries"`
}

func LoadSession(workspace string) *SessionData {
	data, err := os.ReadFile(filepath.Join(workspace, sessionFile))
	if err != nil {
		return nil
	}
	var session SessionData
	if err := json.Unmarshal(data, &session); err != nil {
		return nil
	}
	return &session
}

func SaveSession(workspace string, session *SessionData) error {
	session.UpdatedAt = time.Now().Format(time.RFC3339)
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(workspace, sessionFile), data, 0o644)
}

func NewSession() *SessionData {
	now := time.Now().Format(time.RFC3339)
	return &SessionData{
		CreatedAt: now,
		UpdatedAt: now,
		Rounds:    0,
		Entries:   []SessionEntry{},
	}
}

func (s *SessionData) AddEntry(role, content string) {
	s.Entries = append(s.Entries, SessionEntry{
		Role:      role,
		Content:   content,
		Timestamp: time.Now().Format(time.RFC3339),
	})
}

func (s *SessionData) ContextSummary() string {
	if len(s.Entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Previous session context (%d exchanges, %d rounds, since %s):\n\n",
		len(s.Entries), s.Rounds, s.CreatedAt))
	for i, entry := range s.Entries {
		if i >= 20 {
			remaining := len(s.Entries) - i
			b.WriteString(fmt.Sprintf("... (%d earlier exchanges omitted)\n", remaining))
			break
		}
		content := entry.Content
		if len(content) > 500 {
			content = content[:500] + "..."
		}
		b.WriteString(fmt.Sprintf("[%s] %s\n", entry.Role, content))
		b.WriteString("\n")
	}
	return b.String()
}

func ArchiveSession(workspace string, summary string) error {
	histDir := filepath.Join(workspace, historyDirName)
	if err := os.MkdirAll(histDir, 0o755); err != nil {
		return fmt.Errorf("failed to create history directory: %w", err)
	}

	ts := time.Now().Format("2006-01-02")
	shortDesc := "session"
	lines := strings.SplitN(summary, "\n", 2)
	if len(lines) > 0 {
		first := strings.TrimSpace(lines[0])
		first = strings.TrimPrefix(first, "# ")
		first = strings.TrimSpace(first)
		if first != "" {
			slug := strings.ToLower(first)
			slug = strings.ReplaceAll(slug, " ", "-")
			slug = strings.ReplaceAll(slug, "/", "-")
			if len(slug) > 40 {
				slug = slug[:40]
			}
			slug = strings.Trim(slug, "-.")
			shortDesc = slug
		}
	}

	filename := fmt.Sprintf("%s-%s.md", ts, shortDesc)
	path := filepath.Join(histDir, filename)

	if err := os.WriteFile(path, []byte(summary), 0o644); err != nil {
		return fmt.Errorf("failed to write history file: %w", err)
	}

	oldSession := filepath.Join(workspace, sessionFile)
	if err := os.Remove(oldSession); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove old session: %w", err)
	}

	return nil
}

func HasSession(workspace string) bool {
	_, err := os.Stat(filepath.Join(workspace, sessionFile))
	return err == nil
}