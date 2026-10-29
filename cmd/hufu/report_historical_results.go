package main

import (
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

func writeHistoricalResultReport(b *strings.Builder, summary team.ContextManifestSummary) {
	if summary.HistoricalGrantsError != "" {
		fmt.Fprintf(b, "- **Historical grant status:** unavailable (%s)\n", reportSafeMetadata(summary.HistoricalGrantsError, 240))
	}
	for _, grant := range summary.HistoricalGrants {
		fmt.Fprintf(b, "- **Historical grant:** worker `%s`, result `%s`, cycle `%s`, %s; presentation %d bytes, sha256 `%s`\n",
			reportSafeMetadata(grant.Grant.Recipient, 120), grant.Grant.ResultID, reportSafeMetadata(grant.CycleID, 120),
			grant.State, grant.Grant.PresentationBytes, grant.Grant.PresentationSHA256)
	}
	for _, delivery := range summary.HistoricalResults {
		if delivery.Attribution == nil {
			continue
		}
		state := "prepared"
		if delivery.ModelCalled {
			state = "dispatched"
		}
		fmt.Fprintf(b, "- **Historical delivery:** task `%s`, worker `%s`, attempt %d, request `%s`, %s; result `%s`, %d tokens\n",
			reportSafeMetadata(delivery.TaskID, 120), reportSafeMetadata(delivery.Agent, 120), delivery.Attempt,
			reportSafeMetadata(delivery.RequestID, 120), state, delivery.Attribution.Source.ResultID, delivery.Tokens)
	}
}
