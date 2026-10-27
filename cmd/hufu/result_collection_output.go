package main

import (
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

// Keep output content-free: durable work, prompts and model responses remain
// in the artifact store, not in status JSON or reports.
type jsonResultCollection struct {
	Target        string                     `json:"execution_target,omitempty"`
	State         team.ResultCollectionState `json:"state"`
	TurnsUsed     int                        `json:"turns_used"`
	TurnLimit     int                        `json:"turn_limit"`
	FailureReason team.RepairFailureReason   `json:"failure_reason,omitempty"`
}

func resultCollectionSummaries(item *team.TodoItem) []jsonResultCollection {
	if item == nil {
		return nil
	}
	receipts := item.ExecutionReceipts
	if len(receipts) == 0 && item.ExecutionReceipt != nil {
		receipts = []team.ExecutionReceipt{*item.ExecutionReceipt}
	}
	var summaries []jsonResultCollection
	for _, receipt := range receipts {
		if checkpoint := receipt.ResultCollection; checkpoint != nil {
			summaries = append(summaries, jsonResultCollection{Target: checkpoint.ExecutionTarget.String(), State: checkpoint.State, TurnsUsed: checkpoint.TurnsUsed, TurnLimit: checkpoint.TurnLimit, FailureReason: checkpoint.FailureReason})
		}
	}
	return summaries
}

func writeResultCollectionReport(builder *strings.Builder, todos []*team.TodoItem) {
	wroteHeader := false
	for _, item := range todos {
		for _, summary := range resultCollectionSummaries(item) {
			if !wroteHeader {
				builder.WriteString("## Result Collection\n\n| Task | Target | State | Turns | Failure |\n|---|---|---|---|---|\n")
				wroteHeader = true
			}
			fmt.Fprintf(builder, "| %s | %s | %s | %d/%d | %s |\n", reportTableValue(item.ID, 40), reportTableValue(summary.Target, 120), summary.State, summary.TurnsUsed, summary.TurnLimit, summary.FailureReason)
		}
	}
	if wroteHeader {
		builder.WriteString("\n")
	}
}
