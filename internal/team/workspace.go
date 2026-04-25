package team

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	inboxDir   = "inbox"
	outboxDir  = "outbox"
	sharedDir  = "shared"
	statusDir  = "status"
	historyDir = "history"
)

func EnsureWorkspaceDirs(workspace string) error {
	for _, dir := range []string{inboxDir, outboxDir, sharedDir, statusDir, historyDir} {
		if err := os.MkdirAll(filepath.Join(workspace, dir), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func writeInbox(workspace, agentName, content string) error {
	dir := filepath.Join(workspace, inboxDir, agentName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ts := time.Now().Format("20060102-150405")
	path := filepath.Join(dir, fmt.Sprintf("task-%s.md", ts))
	return os.WriteFile(path, []byte(content), 0o644)
}

func readOutbox(workspace, agentName string) ([]string, error) {
	dir := filepath.Join(workspace, outboxDir, agentName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var results []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		results = append(results, string(data))
	}
	return results, nil
}

func writeOutbox(workspace, agentName, content string) error {
	dir := filepath.Join(workspace, outboxDir, agentName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ts := time.Now().Format("20060102-150405")
	path := filepath.Join(dir, fmt.Sprintf("result-%s.md", ts))
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