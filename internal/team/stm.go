package team

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const stmFile = "stm.md"

const maxSTMChars = 4000

func STMPath(workspace string) string {
	return filepath.Join(workspace, stmFile)
}

func LoadSTM(workspace string) string {
	data, err := os.ReadFile(STMPath(workspace))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func SaveSTM(workspace string, content string) error {
	return os.WriteFile(STMPath(workspace), []byte(content), 0o644)
}

func TruncateSTM(content string) string {
	runes := []rune(content)
	if len(runes) <= maxSTMChars {
		return content
	}
	return string(runes[len(runes)-maxSTMChars:])
}

func ArchiveSTM(workspace string) (string, error) {
	stmContent := LoadSTM(workspace)
	if stmContent == "" {
		return "", nil
	}

	histDir := filepath.Join(workspace, historyDirName)
	if err := os.MkdirAll(histDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create history directory: %w", err)
	}

	ts := time.Now().Format("2006-01-02")
	filename := fmt.Sprintf("%s-stm.md", ts)
	path := filepath.Join(histDir, filename)
	if err := os.WriteFile(path, []byte(stmContent), 0o644); err != nil {
		return "", fmt.Errorf("failed to archive stm.md: %w", err)
	}

	if err := removeFileIfExists(STMPath(workspace)); err != nil {
		return path, fmt.Errorf("failed to remove stm.md after archive: %w", err)
	}

	return path, nil
}

func InitSTM(workspace string) error {
	path := STMPath(workspace)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, []byte(""), 0o644)
}
