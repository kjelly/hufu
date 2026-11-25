package team

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// maxFanOutRows is a hard safety valve against a malformed or unexpectedly
// huge TSV turning one dispatch into an unbounded number of tasks. A normal
// workset item count is expected to stay far below this.
const maxFanOutRows = 2000

var fanOutPlaceholderPattern = regexp.MustCompile(`\{([a-zA-Z0-9_]+)\}`)

// expandFanOutTasks replaces every task with a non-nil FanOut with one task
// per data row of its TSV source, substituting {column} placeholders in
// GoalTemplate with that row's value. Scheduling indices refer to the
// original request, so dependents of a fan-out wait for all its children.
// A malformed or out-of-workspace source, or a template placeholder with no matching header
// column fails the whole dispatch (no partial expansion), so a coordinator's
// mistake is caught immediately rather than producing some correct and some
// silently wrong tasks.
func (c *Coordinator) expandFanOutTasks(ctx context.Context, tasks []TaskDef) ([]TaskDef, error) {
	if err := artifactReadContextError(ctx); err != nil {
		return nil, err
	}
	hasFanOut := false
	for _, t := range tasks {
		if t.FanOut != nil || t.EvidenceInventory.isExpansion() {
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
	offsets := make([]int, len(tasks)+1)
	for i, t := range tasks {
		offsets[i] = len(expanded)
		if t.EvidenceInventory.isExpansion() {
			if len(tasks) != 1 || len(t.EvidenceFrom) != 0 || len(t.DependsOn) != 0 || t.Pipeline || t.OnFailure != nil {
				return nil, fmt.Errorf("contract %q owns its fan-out and evidence_from; dispatch it alone without scheduling inputs", t.ContractID)
			}
			rows, err := c.expandEvidenceInventory(ctx, t)
			if err != nil {
				return nil, fmt.Errorf("tasks[%d].evidence-inventory: %w", i, err)
			}
			return rows, nil
		}
		if t.FanOut == nil {
			expanded = append(expanded, t)
			continue
		}
		if usesStructuredWorkset(t) {
			rows, err := c.expandStructuredFanOutTask(ctx, t)
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
	offsets[len(tasks)] = len(expanded)
	for i, task := range expandPipelineDeps(tasks) {
		start, end := offsets[i], offsets[i+1]
		if start == end {
			continue
		}
		deps := expandFanOutIndices(task.DependsOn, offsets, i)
		order := expandFanOutIndices(task.OrderAfter, offsets, i)
		var failureTarget *int
		if task.OnFailure != nil && *task.OnFailure != i {
			target := *task.OnFailure
			if target < 0 || target >= len(tasks) || offsets[target+1]-offsets[target] != 1 {
				return nil, fmt.Errorf("tasks[%d].on_failure: target %d must resolve to exactly one task", i, target)
			}
			failureTarget = new(offsets[target])
		}
		for j := start; j < end; j++ {
			expanded[j].DependsOn = slices.Clone(deps)
			expanded[j].OrderAfter = slices.Clone(order)
			expanded[j].Pipeline = false
			expanded[j].OnFailure = failureTarget
			if task.OnFailure != nil && *task.OnFailure == i {
				expanded[j].OnFailure = new(j)
			}
		}
	}
	return expanded, nil
}

func expandFanOutIndices(indices, offsets []int, original int) []int {
	var expanded []int
	for _, index := range indices {
		if index == original || index < 0 || index >= len(offsets)-1 {
			continue
		}
		for child := offsets[index]; child < offsets[index+1]; child++ {
			expanded = append(expanded, child)
		}
	}
	return expanded
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
