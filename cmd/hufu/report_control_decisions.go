package main

import (
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

// writeControlDecisionsReport renders the runtime control decision summary
// (docs/architecture/decision-primitive.md §59). It writes nothing when no
// point ran in shadow or active mode.
func writeControlDecisionsReport(b *strings.Builder, summaries []team.ControlDecisionSummary) {
	if len(summaries) == 0 {
		return
	}
	b.WriteString("## Control Decisions\n\n")
	b.WriteString("| Point | Mode | Calls | Agreed / compared | Below threshold | Errors | Mean confidence | p50 / p95 (ms) | Applied (decision / safe / existing) |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, summary := range summaries {
		fmt.Fprintf(b, "| %s | %s | %d | %d / %d | %d | %d | %.3f | %d / %d | %d / %d / %d |\n",
			reportSafeMetadata(summary.Point, 80), reportSafeMetadata(summary.Mode, 40), summary.Calls,
			summary.Agreed, summary.Compared, summary.BelowThreshold, summary.Errors, summary.MeanConfidence,
			summary.P50MS, summary.P95MS, summary.AppliedPrimitive, summary.AppliedSafeDefault, summary.AppliedLegacy)
	}
	b.WriteString("\nConfidence is the decision model's raw probability, not a calibrated accuracy.\n\n")
}
