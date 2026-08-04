package team

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anomalyco/hufu/internal/utils"
)

const (
	tasksDir   = "tasks"
	sharedDir  = "shared"
	statusDir  = "status"
	historyDir = "history"
	logsDir    = "logs"
	llmLogsDir = "logs/llm"
	stmLogsDir = "logs/stm"
)

func EnsureWorkspaceDirs(workspace string) error {
	for _, dir := range []string{tasksDir, sharedDir, statusDir, historyDir, logsDir} {
		if err := os.MkdirAll(filepath.Join(workspace, dir), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func CleanRunDirs(workspace string) error {
	_, err := CleanRunDirsWithEvidence(workspace)
	return err
}

// CleanupPathState records the observable state of a path immediately before
// or after a cleanup operation. Entry names are included so the operation is
// auditable without persisting file contents.
type CleanupPathState struct {
	Path       string   `json:"path"`
	Exists     bool     `json:"exists"`
	EntryCount int      `json:"entry_count,omitempty"`
	Entries    []string `json:"entries,omitempty"`
}

// CleanupEvidence is persisted outside the removed run directories so a
// restart can prove what cleanup changed.
type CleanupEvidence struct {
	StartedAt   string             `json:"started_at"`
	CompletedAt string             `json:"completed_at,omitempty"`
	Workspace   string             `json:"workspace"`
	Before      []CleanupPathState `json:"before"`
	After       []CleanupPathState `json:"after"`
	Error       string             `json:"error,omitempty"`
}

func cleanupPathState(path string) CleanupPathState {
	state := CleanupPathState{Path: path}
	entries, err := os.ReadDir(path)
	if err != nil {
		return state
	}
	state.Exists = true
	state.EntryCount = len(entries)
	state.Entries = make([]string, 0, len(entries))
	for _, entry := range entries {
		state.Entries = append(state.Entries, entry.Name())
	}
	return state
}

// CleanRunDirsWithEvidence removes only run-owned directories and persists a
// before/after record under logs, which is outside the removed directories.
func CleanRunDirsWithEvidence(workspace string) (CleanupEvidence, error) {
	cleanWorkspace, err := canonicalWorkspacePath(workspace)
	if err != nil {
		return CleanupEvidence{}, fmt.Errorf("cleanup workspace: %w", err)
	}
	home, _ := os.UserHomeDir()
	if cleanWorkspace == string(filepath.Separator) || (home != "" && cleanWorkspace == canonicalPath(home)) {
		return CleanupEvidence{}, fmt.Errorf("refusing cleanup of protected workspace %q", cleanWorkspace)
	}
	evidence := CleanupEvidence{
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Workspace: cleanWorkspace,
	}
	for _, dir := range []string{tasksDir, statusDir} {
		evidence.Before = append(evidence.Before, cleanupPathState(filepath.Join(cleanWorkspace, dir)))
	}
	var cleanupErr error
	for _, dir := range []string{tasksDir, statusDir} {
		if err := os.RemoveAll(filepath.Join(cleanWorkspace, dir)); err != nil && !os.IsNotExist(err) {
			cleanupErr = err
			break
		}
	}
	for _, dir := range []string{tasksDir, statusDir} {
		evidence.After = append(evidence.After, cleanupPathState(filepath.Join(cleanWorkspace, dir)))
	}
	if cleanupErr != nil {
		evidence.Error = cleanupErr.Error()
	}
	evidence.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	data, marshalErr := json.MarshalIndent(evidence, "", "  ")
	if marshalErr == nil {
		data, marshalErr = utils.RedactJSON(data)
	}
	if marshalErr == nil {
		marshalErr = os.MkdirAll(filepath.Join(cleanWorkspace, logsDir), 0o755)
	}
	if marshalErr == nil {
		marshalErr = AtomicWriteFile(filepath.Join(cleanWorkspace, logsDir, "cleanup_evidence.json"), data, 0o600)
	}
	if cleanupErr != nil {
		return evidence, cleanupErr
	}
	if marshalErr != nil {
		return evidence, fmt.Errorf("persist cleanup evidence: %w", marshalErr)
	}
	return evidence, nil
}

func writeTaskFile(workspace, teamName, agentName, timestamp, status, task, result string) error {
	return writeTaskFileWithDetail(workspace, teamName, agentName, timestamp, status, task, result, "")
}

func writeTaskFileWithDetail(workspace, teamName, agentName, timestamp, status, task, result, detail string) error {
	return writeTaskFileWithFailureEvent(workspace, teamName, agentName, timestamp, status, task, result, detail, nil)
}

func writeTaskFileWithFailureEvent(workspace, teamName, agentName, timestamp, status, task, result, detail string, event *FailureEventPayload) error {
	validStatuses := map[string]bool{"working": true, "done": true, "error": true}
	if !validStatuses[status] {
		return fmt.Errorf("invalid task status %q: must be working, done, or error", status)
	}

	agentKey, err := canonicalAgentWorkspaceKey(agentName)
	if err != nil {
		return err
	}
	dir := filepath.Join(workspace, tasksDir, teamName, agentKey)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	now := time.Now().Format(time.RFC3339)

	resultSection := "(pending)"
	if result != "" {
		resultSection = result
	}

	completedLine := ""
	if status != "working" {
		completedLine = fmt.Sprintf("**Completed:** %s\n", now)
	}

	failureSection := ""
	if detail != "" {
		failureSection = fmt.Sprintf("\n---\n\n## Failure Detail\n\n%s\n", detail)
	}
	if rendered := RenderFailureMarkdown(event); rendered != "" {
		failureSection += fmt.Sprintf("\n---\n\n## Failure Event\n\n%s\n", rendered)
	}

	content := fmt.Sprintf("# Agent Task: %s\n\n**Status:** %s\n**Updated:** %s\n%s\n---\n\n## Task Description\n\n%s\n\n---\n\n## Result\n\n%s\n%s",
		agentName, status, now, completedLine, utils.RedactSecrets(task), utils.RedactSecrets(resultSection), utils.RedactSecrets(failureSection))

	path := filepath.Join(dir, timestamp+".md")
	return os.WriteFile(path, []byte(content), 0o644)
}

func writeStatus(workspace, agentName, status, task string) error {
	return writeStatusWithDetail(workspace, agentName, status, task, "")
}

func writeStatusWithDetail(workspace, agentName, status, task, detail string) error {
	return writeStatusWithFailureEvent(workspace, agentName, status, task, detail, nil)
}

func writeStatusWithFailureEvent(workspace, agentName, status, task, detail string, event *FailureEventPayload) error {
	agentKey, err := canonicalAgentWorkspaceKey(agentName)
	if err != nil {
		return err
	}
	dir := filepath.Join(workspace, statusDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data := fmt.Sprintf("status: %s\ntask: %s\ntime: %s\n", status, utils.RedactSecrets(task), time.Now().Format(time.RFC3339))
	if detail != "" {
		data += fmt.Sprintf("detail: %s\n", utils.RedactSecrets(detail))
	}
	if rendered := RenderFailureText(event); rendered != "" {
		data += "failure_event: |\n"
		for _, line := range strings.Split(rendered, "\n") {
			data += "  " + utils.RedactSecrets(line) + "\n"
		}
	}
	path := filepath.Join(dir, agentKey+".yml")
	return os.WriteFile(path, []byte(data), 0o644)
}

func writeShared(workspace, filename, content string) error {
	dir := filepath.Join(workspace, sharedDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644)
}

func readShared(workspace, filename string) (string, error) {
	data, err := os.ReadFile(filepath.Join(workspace, sharedDir, filename))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func writeLLMLog(workspace, teamName, agentName, entry string) {
	agentKey, err := canonicalAgentWorkspaceKey(agentName)
	if err != nil {
		return
	}
	dir := filepath.Join(workspace, llmLogsDir, teamName, agentKey)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "llm.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(utils.RedactSecrets(entry))
}

// canonicalAgentWorkspaceKey is the one filesystem identity used by all
// agent-owned workspace artifacts. Agent display names remain in file content
// and MCP authorization; only paths use this normalized key.
func canonicalAgentWorkspaceKey(agentName string) (string, error) {
	key := strings.ToLower(strings.TrimSpace(agentName))
	if key == "" || key == "." || key == ".." || filepath.IsAbs(key) || filepath.Base(key) != key || filepath.Clean(key) != key || strings.ContainsAny(key, `/\\`) || strings.ContainsRune(key, '\x00') {
		return "", fmt.Errorf("unsafe agent workspace key %q", agentName)
	}
	return key, nil
}

type taskHistoryEntry struct {
	name string
	path string
}

// taskHistoryEntries reads the canonical directory plus legacy directories
// whose names differ only by case. Writers never create those legacy paths,
// but merging them deterministically avoids hiding existing forensic records.
func taskHistoryEntries(workspace, teamName, agentName string) ([]taskHistoryEntry, error) {
	key, err := canonicalAgentWorkspaceKey(agentName)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(workspace, tasksDir, teamName)
	dirs := []string{key}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var legacy []string
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != key && strings.EqualFold(entry.Name(), key) {
			legacy = append(legacy, entry.Name())
		}
	}
	sort.Strings(legacy)
	dirs = append(dirs, legacy...)

	var files []taskHistoryEntry
	for _, dirName := range dirs {
		dirEntries, readErr := os.ReadDir(filepath.Join(root, dirName))
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue
			}
			return nil, readErr
		}
		for _, entry := range dirEntries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			files = append(files, taskHistoryEntry{name: entry.Name(), path: filepath.Join(root, dirName, entry.Name())})
		}
	}
	sort.SliceStable(files, func(i, j int) bool {
		if files[i].name != files[j].name {
			return files[i].name > files[j].name
		}
		return files[i].path < files[j].path
	})
	return files, nil
}
