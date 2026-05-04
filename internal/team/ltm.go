package team

import (
	"os"
	"path/filepath"
	"strings"
)

const ltmFile = "ltm.md"

const maxLTMChars = 6000

func LTMPath(teamDir string) string {
	return filepath.Join(teamDir, ltmFile)
}

func LoadLTM(teamDir string) string {
	data, err := os.ReadFile(LTMPath(teamDir))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func SaveLTM(teamDir string, content string) error {
	return os.WriteFile(LTMPath(teamDir), []byte(content), 0o644)
}

func TruncateLTM(content string) string {
	runes := []rune(content)
	if len(runes) <= maxLTMChars {
		return content
	}
	return string(runes[len(runes)-maxLTMChars:])
}

func InitLTM(teamDir string) error {
	path := LTMPath(teamDir)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, []byte(""), 0o644)
}
