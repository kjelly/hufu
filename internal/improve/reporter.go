package improve

import (
	"encoding/json"
	"fmt"
	"strings"
)

func Markdown(report *Report) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Agent Team Improvement Report")
	fmt.Fprintf(&b, "\n- **Team**: %s\n- **Workspace**: %s\n- **Runs**: %s\n- **Generated**: %s\n- **Source**: %s\n", report.Team, report.Workspace, strings.Join(report.RunIDs, ", "), report.GeneratedAt, report.Source)
	if len(report.TeamRevisions) > 0 {
		fmt.Fprintf(&b, "- **Team revisions**: %s\n", strings.Join(report.TeamRevisions, ", "))
	}
	fmt.Fprintf(&b, "\n## Metrics\n\n| Metric | Value |\n|---|---:|\n| Runs | %d |\n| Tasks | %d |\n| Done | %d |\n| Error | %d |\n| Planned | %d |\n| Attempts | %d |\n| Retried tasks | %d |\n| Total tokens | %d |\n| Tool calls | %d |\n| Tool errors | %d |\n", report.Metrics.RunCount, report.Metrics.TotalTasks, report.Metrics.Done, report.Metrics.Error, report.Metrics.Planned, report.Metrics.TotalAttempts, report.Metrics.RetriedTasks, report.Metrics.TotalTokens, report.Metrics.ToolCalls, report.Metrics.ToolErrors)

	fmt.Fprint(&b, "\n## Trend by Run\n\n")
	fmt.Fprintln(&b, "| Run | Tasks | Done | Error | Retried | Tokens | Team revision |")
	fmt.Fprintln(&b, "|---|---:|---:|---:|---:|---:|---|")
	for _, point := range report.Trend {
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %s |\n", point.RunID, point.Metrics.TotalTasks, point.Metrics.Done, point.Metrics.Error, point.Metrics.RetriedTasks, point.Metrics.TotalTokens, fallbackMarkdown(point.TeamRevision, "legacy/unspecified"))
	}

	fmt.Fprint(&b, "\n## Telemetry Breakdowns\n\n")
	writeGroupTable(&b, "By Agent", report.Groups.ByAgent)
	writeGroupTable(&b, "By Task Type", report.Groups.ByTaskType)
	writeGroupTable(&b, "By Model", report.Groups.ByModel)
	fmt.Fprintln(&b, "### By Skill\n\nA task associated with multiple skills appears in each matching skill group.")
	writeGroupRows(&b, report.Groups.BySkill)

	if len(report.Findings) == 0 {
		fmt.Fprintln(&b, "\n## Findings\n\nNo rule-triggered findings for the selected runs.")
		return b.String()
	}
	fmt.Fprintln(&b, "\n## Findings")
	for i, finding := range report.Findings {
		fmt.Fprintf(&b, "\n### %d. [%s] %s\n\n", i+1, strings.ToUpper(finding.Severity), finding.Metric)
		fmt.Fprintf(&b, "- **Layer / target**: %s / %s\n- **Observed**: %s\n- **Evidence**: %s\n- **Confidence**: %s\n- **Suggestion**: %s\n- **Rule**: `%s`\n", finding.Layer, finding.Target, finding.Value, finding.Evidence, finding.Confidence, finding.Suggestion, finding.SourceRule)
	}
	return b.String()
}

func writeGroupTable(b *strings.Builder, heading string, groups []GroupMetric) {
	fmt.Fprintf(b, "### %s\n\n", heading)
	writeGroupRows(b, groups)
}

func writeGroupRows(b *strings.Builder, groups []GroupMetric) {
	fmt.Fprintln(b, "| Group | Tasks | Done | Error | Retried | Attempts | Tokens |")
	fmt.Fprintln(b, "|---|---:|---:|---:|---:|---:|---:|")
	for _, group := range groups {
		fmt.Fprintf(b, "| %s | %d | %d | %d | %d | %d | %d |\n", group.Key, group.TotalTasks, group.Done, group.Error, group.RetriedTasks, group.TotalAttempts, group.TotalTokens)
	}
}

func fallbackMarkdown(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func JSON(report *Report) ([]byte, error) { return json.MarshalIndent(report, "", "  ") }
