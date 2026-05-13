package team

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
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
	for _, dir := range []string{tasksDir, statusDir} {
		if err := os.RemoveAll(filepath.Join(workspace, dir)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func writeTaskFile(workspace, teamName, agentName, timestamp, status, task, result string) error {
	validStatuses := map[string]bool{"working": true, "done": true, "error": true}
	if !validStatuses[status] {
		return fmt.Errorf("invalid task status %q: must be working, done, or error", status)
	}

	dir := filepath.Join(workspace, tasksDir, teamName, agentName)
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

	content := fmt.Sprintf("# Agent Task: %s\n\n**Status:** %s\n**Updated:** %s\n%s\n---\n\n## Task Description\n\n%s\n\n---\n\n## Result\n\n%s\n",
		agentName, status, now, completedLine, task, resultSection)

	path := filepath.Join(dir, timestamp+".md")
	return os.WriteFile(path, []byte(content), 0o644)
}

func writeStatus(workspace, agentName, status, task string) error {
	dir := filepath.Join(workspace, statusDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data := fmt.Sprintf("status: %s\ntask: %s\ntime: %s\n", status, task, time.Now().Format(time.RFC3339))
	path := filepath.Join(dir, agentName+".yml")
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
	dir := filepath.Join(workspace, llmLogsDir, teamName, agentName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "llm.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(entry)
}
