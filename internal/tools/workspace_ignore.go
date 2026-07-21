//go:build linux || darwin
// +build linux darwin

package tools

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Hufu workspace execution records (session history, LLM/audit/STM logs,
// task journals) are the model's own past conversations. Letting grep/glob
// return them by default pollutes the coordinator context with large,
// self-referential tool results. Both tools therefore exclude these files
// unless the search is explicitly rooted inside the workspace directory.
//
// A file is only treated as a workspace record when it sits beneath a
// directory whose name matches the configured workspace directory name
// (default "workspace"), so identically named files in user projects are
// unaffected.

// defaultWorkspaceDirName is used when ToolConfig.WorkspaceName is unset.
const defaultWorkspaceDirName = "workspace"

// workspaceRecordDirs are directory fragments (relative to the workspace
// directory) that only ever contain hufu execution records.
var workspaceRecordDirs = []string{"logs/llm", "logs/audit", "logs/stm"}

// workspaceRecordNames are exact base names of hufu execution record files.
var workspaceRecordNames = []string{
	"session_history.json",
	"session.json",
	"task_journal.jsonl",
	"compaction_history.json",
	"llm.log",
}

// workspaceRecordNameGlobs are base-name patterns of hufu execution record
// files.
var workspaceRecordNameGlobs = []string{"audit-*.jsonl", "stm_r*.md"}

// workspaceDirName returns the directory name used to detect workspace
// execution records for the given tool configuration.
func workspaceDirName(cfg ToolConfig) string {
	if cfg.WorkspaceName != "" {
		return cfg.WorkspaceName
	}
	return defaultWorkspaceDirName
}

// pathHasComponent reports whether any path component of path equals name.
func pathHasComponent(path, name string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == name {
			return true
		}
	}
	return false
}

// isWorkspaceRecordPath reports whether path points at a hufu workspace
// execution record: a record-shaped file located beneath a directory named
// wsName.
func isWorkspaceRecordPath(path, wsName string) bool {
	if wsName == "" {
		wsName = defaultWorkspaceDirName
	}
	parts := strings.Split(filepath.ToSlash(path), "/")
	wsIdx := -1
	for i, part := range parts {
		if part == wsName {
			wsIdx = i
			break
		}
	}
	if wsIdx < 0 || wsIdx >= len(parts)-1 {
		return false
	}

	rest := "/" + strings.Join(parts[wsIdx+1:], "/")
	for _, dir := range workspaceRecordDirs {
		if strings.Contains(rest, "/"+dir+"/") {
			return true
		}
	}

	base := parts[len(parts)-1]
	for _, name := range workspaceRecordNames {
		if base == name {
			return true
		}
	}
	for _, pattern := range workspaceRecordNameGlobs {
		if matched, err := filepath.Match(pattern, base); err == nil && matched {
			return true
		}
	}
	return false
}

// workspaceRecordRgGlobs returns negative ripgrep --glob patterns that
// exclude hufu workspace execution records beneath a directory named wsName.
func workspaceRecordRgGlobs(wsName string) []string {
	if wsName == "" {
		wsName = defaultWorkspaceDirName
	}
	prefix := "!**/" + wsName + "/**/"
	globs := make([]string, 0, len(workspaceRecordDirs)+len(workspaceRecordNames)+len(workspaceRecordNameGlobs))
	for _, dir := range workspaceRecordDirs {
		globs = append(globs, prefix+dir+"/**")
	}
	for _, name := range workspaceRecordNames {
		globs = append(globs, prefix+name)
	}
	for _, pattern := range workspaceRecordNameGlobs {
		globs = append(globs, prefix+pattern)
	}
	return globs
}

// grepOutputPathRe extracts the file path prefix from a grep/rg output line
// of the form "path:lineno:content" (matches) or "path-lineno-content"
// (context lines).
var grepOutputPathRe = regexp.MustCompile(`^(.+?)[-:]\d+[-:]`)

// filterWorkspaceRecordLines drops grep output lines whose file path is a
// hufu workspace execution record. It is used by the plain-grep fallback,
// which cannot express the exclusion as ripgrep globs.
func filterWorkspaceRecordLines(output, wsName string) string {
	lines := strings.Split(output, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if m := grepOutputPathRe.FindStringSubmatch(line); m != nil && isWorkspaceRecordPath(m[1], wsName) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
