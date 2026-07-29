package team

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"path/filepath"
	"strings"
	"time"

	"github.com/anomalyco/hufu/internal/utils"
)

// removeFileIfExists removes path and returns nil when the file does not exist.
func removeFileIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

const sessionFile = "session.json"
const historyDirName = "history"
const maxSessionEntries = 40

type SessionEntry struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp,omitempty"`
}

type SessionData struct {
	CreatedAt                          string              `json:"created_at"`
	UpdatedAt                          string              `json:"updated_at"`
	Rounds                             int                 `json:"rounds"`
	ConversationHistorySourceOffset    int                 `json:"conversation_history_source_offset"`
	ConversationHistorySourceCounts    []int               `json:"conversation_history_source_counts"`
	ConversationHistorySourceRanges    [][]CompactionRange `json:"conversation_history_source_ranges,omitempty"`
	ConversationHistoryNextSourceIndex int                 `json:"conversation_history_next_source_index,omitempty"`
	Entries                            []SessionEntry      `json:"entries"`
	Tasks                              []*TodoItem         `json:"tasks,omitempty"`
	// RunResult is the canonical outcome of the latest execution. It is
	// persisted with the session so reloads, reports, and notifications retain
	// the same completed/partial/blocked/failed/cancelled semantics.
	RunResult                   *RunResult                   `json:"run_result,omitempty"`
	AcceptanceContractRevisions []AcceptanceContractRevision `json:"acceptance_contract_revisions,omitempty"`
	ContinuationCheckpoint      *ContinuationCheckpoint      `json:"continuation_checkpoint,omitempty"`
}

func LoadSession(workspace string) *SessionData {
	data, err := os.ReadFile(filepath.Join(workspace, sessionFile))
	if err != nil {
		return nil
	}
	var session SessionData
	if err := json.Unmarshal(data, &session); err != nil {
		fmt.Fprintf(os.Stderr, "warning: corrupt session file in %s: %v\n", workspace, err)
		return nil
	}
	return &session
}

func SaveSession(workspace string, session *SessionData) error {
	if session == nil {
		return errors.New("session is nil")
	}
	session.UpdatedAt = time.Now().Format(time.RFC3339)
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWriteFile(filepath.Join(workspace, sessionFile), []byte(utils.RedactSecrets(string(data))), 0o600)
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
	// Skip empty entries (e.g. a wrap-up continuation with no prompt) and
	// exact repeats of the previous entry: a failed turn leaves a dangling
	// user entry with no assistant reply, so redispatching the same prompt
	// would otherwise record the user message twice in chat history.
	if strings.TrimSpace(content) == "" {
		return
	}
	if n := len(s.Entries); n > 0 && s.Entries[n-1].Role == role && s.Entries[n-1].Content == content {
		return
	}
	s.Entries = append(s.Entries, SessionEntry{
		Role:      role,
		Content:   content,
		Timestamp: time.Now().Format(time.RFC3339),
	})
}

// cloneSession returns a copy of TeamSession with Workspace replaced.
// All other fields are shallow-copied (safe because they are read-only during task execution).
func cloneSession(orig *TeamSession, newWorkspace string) *TeamSession {
	clone := *orig
	clone.Workspace = newWorkspace
	return &clone
}

func (s *SessionData) ContextSummary() string {
	if len(s.Entries) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Previous session context (%d exchanges, %d rounds, since %s):\n\n",
		len(s.Entries), s.Rounds, s.CreatedAt)
	start := 0
	if len(s.Entries) > maxSessionEntries {
		start = len(s.Entries) - maxSessionEntries
		fmt.Fprintf(&b, "... (%d older exchanges omitted)\n\n", start)
	}
	for _, entry := range s.Entries[start:] {
		content := utils.TruncateRunes(entry.Content, 500)
		fmt.Fprintf(&b, "[%s] %s\n", entry.Role, content)
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
		firstLine := strings.TrimSpace(lines[0])
		if strings.HasPrefix(firstLine, "# ") {
			first := strings.TrimSpace(strings.TrimPrefix(firstLine, "# "))
			if first != "" {
				// Slugify: lowercase, replace spaces and special chars with hyphens
				slug := strings.ToLower(first)
				var b strings.Builder
				for _, r := range slug {
					if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
						b.WriteRune(r)
					} else {
						b.WriteRune('-')
					}
				}
				slug = b.String()
				// Replace multiple hyphens with single hyphen
				for strings.Contains(slug, "--") {
					slug = strings.ReplaceAll(slug, "--", "-")
				}
				if len(slug) > 40 {
					slug = slug[:40]
				}
				slug = strings.Trim(slug, "-.")
				if slug != "" {
					shortDesc = slug
				}
			}
		}
	}

	filename := fmt.Sprintf("%s-%s.md", ts, shortDesc)
	path := filepath.Join(histDir, filename)

	if err := os.WriteFile(path, []byte(summary), 0o644); err != nil {
		return fmt.Errorf("failed to write history file: %w", err)
	}

	if err := removeFileIfExists(filepath.Join(workspace, sessionFile)); err != nil {
		return fmt.Errorf("failed to remove old session: %w", err)
	}

	return nil
}

func HasSession(workspace string) bool {
	fi, err := os.Stat(filepath.Join(workspace, sessionFile))
	if err != nil {
		return false
	}
	return !fi.IsDir()
}
