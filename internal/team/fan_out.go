package team

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// maxFanOutRows is a hard safety valve against a malformed or unexpectedly
// huge TSV turning one dispatch into an unbounded number of tasks. A normal
// workset item count is expected to stay far below this.
const maxFanOutRows = 2000

var fanOutPlaceholderPattern = regexp.MustCompile(`\{([a-zA-Z0-9_]+)\}`)

// expandFanOutTasks replaces every task with a non-nil FanOut with one task
// per data row of its TSV source, substituting {column} placeholders in
// GoalTemplate with that row's value; tasks without FanOut pass through
// unchanged, in their original position. A malformed source, an
// out-of-workspace source, or a template placeholder with no matching header
// column fails the whole dispatch (no partial expansion), so a coordinator's
// mistake is caught immediately rather than producing some correct and some
// silently wrong tasks.
func (c *Coordinator) expandFanOutTasks(tasks []TaskDef) ([]TaskDef, error) {
	hasFanOut := false
	for _, t := range tasks {
		if t.FanOut != nil {
			hasFanOut = true
			break
		}
	}
	if !hasFanOut {
		return tasks, nil
	}
	workspace := ""
	if c != nil && c.session != nil {
		workspace = c.session.Workspace
	}
	expanded := make([]TaskDef, 0, len(tasks))
	for i, t := range tasks {
		if t.FanOut == nil {
			expanded = append(expanded, t)
			continue
		}
		if usesStructuredWorkset(t) {
			rows, err := c.expandStructuredFanOutTask(t)
			if err != nil {
				return nil, fmt.Errorf("tasks[%d].fan_out: %w", i, err)
			}
			expanded = append(expanded, rows...)
			continue
		}
		rows, err := expandFanOutTask(workspace, t)
		if err != nil {
			return nil, fmt.Errorf("tasks[%d].fan_out: %w", i, err)
		}
		expanded = append(expanded, rows...)
	}
	return expanded, nil
}

func expandFanOutTask(workspace string, task TaskDef) ([]TaskDef, error) {
	spec := task.FanOut
	source := strings.TrimSpace(spec.Source)
	template := spec.GoalTemplate
	if source == "" {
		return nil, fmt.Errorf("source is required")
	}
	if strings.TrimSpace(template) == "" {
		return nil, fmt.Errorf("goal_template is required")
	}
	absSource, err := resolveWorkspaceRelativePath(workspace, source)
	if err != nil {
		return nil, err
	}
	header, rows, err := readFanOutTSV(absSource)
	if err != nil {
		return nil, err
	}
	columnIndex := make(map[string]int, len(header))
	for i, name := range header {
		columnIndex[name] = i
	}
	for _, match := range fanOutPlaceholderPattern.FindAllStringSubmatch(template, -1) {
		name := match[1]
		if _, ok := columnIndex[name]; !ok {
			return nil, fmt.Errorf("goal_template placeholder {%s} does not match any header column in %s (columns: %s)", name, source, strings.Join(header, ", "))
		}
	}
	if len(rows) > maxFanOutRows {
		return nil, fmt.Errorf("%s has %d data row(s), exceeding the fan-out limit of %d", source, len(rows), maxFanOutRows)
	}

	expanded := make([]TaskDef, 0, len(rows))
	for rowIndex, row := range rows {
		if len(row) != len(header) {
			return nil, fmt.Errorf("%s row %d has %d field(s), want %d matching the header", source, rowIndex+1, len(row), len(header))
		}
		goal := fanOutPlaceholderPattern.ReplaceAllStringFunc(template, func(m string) string {
			name := m[1 : len(m)-1]
			return row[columnIndex[name]]
		})
		rowTask := task
		rowTask.FanOut = nil
		rowTask.Goal = goal
		expanded = append(expanded, rowTask)
	}
	return expanded, nil
}

// resolveWorkspaceRelativePath applies the same sandbox rule this package
// already uses for context_files (validateSharedContextFiles in
// workspace.go): a non-empty relative path with no ".." escape, resolved
// inside workspace, naming an existing regular file.
func resolveWorkspaceRelativePath(workspace, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("source %q must be a relative path beneath the team workspace", rel)
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("source %q escapes the team workspace", rel)
	}
	abs := filepath.Join(workspace, clean)
	relBack, err := filepath.Rel(workspace, abs)
	if err != nil || relBack == ".." || strings.HasPrefix(relBack, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("source %q escapes the team workspace", rel)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("source %q is not an existing file: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("source %q must name a regular file", rel)
	}
	return abs, nil
}

// readFanOutTSV reads a tab-separated file whose first non-blank line is a
// header (an optional leading '#' on the first column is stripped for
// compatibility with older deterministic manifests). Every other non-blank
// line must have the same field count as the header; readFanOutTSV does not
// skip further '#'-prefixed lines, since a generated manifest's only comment
// line is the header itself.
func readFanOutTSV(path string) (header []string, rows [][]string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if header == nil {
			fields[0] = strings.TrimPrefix(fields[0], "#")
			header = fields
			continue
		}
		rows = append(rows, fields)
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	if header == nil {
		return nil, nil, fmt.Errorf("%s has no header row", path)
	}
	return header, rows, nil
}
