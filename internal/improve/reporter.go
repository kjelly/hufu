package improve

import (
	"encoding/json"
	"fmt"
	"strings"
)

func Markdown(report *Report) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Agent Team Improvement Report")
	fmt.Fprintf(&b, "\n- **Team**: %s\n- **Workspace**: %s\n- **Run ID**: %s\n- **Generated**: %s\n- **Source**: %s\n", report.Team, report.Workspace, report.Metrics.RunID, report.GeneratedAt, report.Source)
	fmt.Fprintf(&b, "\n## Metrics\n\n| Metric | Value |\n|---|---:|\n| Tasks | %d |\n| Done | %d |\n| Error | %d |\n| Planned | %d |\n| Attempts | %d |\n| Retried tasks | %d |\n", report.Metrics.TotalTasks, report.Metrics.Done, report.Metrics.Error, report.Metrics.Planned, report.Metrics.TotalAttempts, report.Metrics.RetriedTasks)
	if len(report.Findings) == 0 {
		fmt.Fprintln(&b, "\n## Findings\n\nNo rule-triggered findings for this run.")
		return b.String()
	}
	fmt.Fprintln(&b, "\n## Findings")
	for i, finding := range report.Findings {
		fmt.Fprintf(&b, "\n### %d. [%s] %s\n\n", i+1, strings.ToUpper(finding.Severity), finding.Metric)
		fmt.Fprintf(&b, "- **Layer / target**: %s / %s\n- **Observed**: %s\n- **Evidence**: %s\n- **Confidence**: %s\n- **Suggestion**: %s\n- **Rule**: `%s`\n", finding.Layer, finding.Target, finding.Value, finding.Evidence, finding.Confidence, finding.Suggestion, finding.SourceRule)
	}
	return b.String()
}

func JSON(report *Report) ([]byte, error) { return json.MarshalIndent(report, "", "  ") }
