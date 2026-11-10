package main

import (
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

func writeDependencyResultViewReport(b *strings.Builder, summary team.ContextManifestSummary) {
	if summary.Prepared > 0 {
		fmt.Fprintf(b, "- **Prepared requests (not dispatched):** %d\n", summary.Prepared)
	}
	for _, view := range summary.DependencyResultViews {
		if view.Attribution == nil {
			continue
		}
		state := "prepared"
		if view.ModelCalled {
			state = "dispatched"
		}
		fmt.Fprintf(b, "- **Dependency view:** task `%s`, agent `%s`, attempt %d, %s; mode `%s`, fields `%s`, policy sha256 `%s`, context sha256 `%s`\n",
			view.TaskID, view.Agent, view.Attempt, state, view.Attribution.Policy.Mode, strings.Join(view.Attribution.Policy.Fields, ","), view.Attribution.PolicySHA256, view.ContentSHA256)
		for _, source := range view.Attribution.Sources {
			fmt.Fprintf(b, "  - Source: branch `%s`, run `%s`, task `%s`, attempt %d, occurrence revision %d, event `%s`; event sha256 `%s`, full handoff sha256 `%s`\n",
				source.BranchID, source.RunID, source.TaskID, source.Attempt, source.OccurrenceRevision, source.SourceEventID, source.SourceEventSHA256, source.FullViewSHA256)
		}
	}
}
