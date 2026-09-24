package main

import (
	"fmt"
	"sort"
	"strings"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/team"
)

// writeExecutionRouteReport lists route-bound tasks with the target every
// attempt ran on and each fallback decision. It writes nothing for a run
// without execution routes.
func writeExecutionRouteReport(b *strings.Builder, todos []*team.TodoItem, metrics *team.RunMetrics) {
	var routed []*team.TodoItem
	for _, item := range todos {
		if item != nil && item.ExecutionRoute != nil {
			routed = append(routed, item)
		}
	}
	if len(routed) == 0 {
		return
	}
	b.WriteString("### Execution Routes\n\n")
	if metrics != nil && metrics.WorkerFallbacksTotal > 0 {
		fmt.Fprintf(b, "- **Fallbacks:** %d (%s)\n\n", metrics.WorkerFallbacksTotal, formatFallbacksByClass(metrics.WorkerFallbacksByClass))
	}
	b.WriteString("| ID | Route | Candidates | Attempts |\n")
	b.WriteString("|----|-------|------------|----------|\n")
	for _, item := range routed {
		candidates := make([]string, 0, len(item.ExecutionRoute.Candidates))
		for _, candidate := range item.ExecutionRoute.Candidates {
			candidates = append(candidates, candidate.String())
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n",
			reportTableValue(item.ID, 40), reportTableValue(item.ExecutionRoute.Name, 64),
			reportTableValue(strings.Join(candidates, " → "), 240), reportTableValue(formatRouteAttempts(item), 480))
	}
	b.WriteString("\n")
}

func formatRouteAttempts(item *team.TodoItem) string {
	receipts := item.ExecutionReceipts
	if len(receipts) == 0 && item.ExecutionReceipt != nil {
		receipts = []team.ExecutionReceipt{*item.ExecutionReceipt}
	}
	parts := make([]string, 0, len(receipts))
	for _, receipt := range receipts {
		if receipt.ExecutionTarget.IsZero() {
			continue
		}
		part := fmt.Sprintf("#%d %s", receipt.Attempt, receipt.ExecutionTarget)
		if receipt.FallbackFrom != nil {
			part += fmt.Sprintf(" (fallback after %s)", receipt.FallbackFailureClass)
		}
		if receipt.FallbackDeniedReason != "" {
			part += fmt.Sprintf(" (no fallback: %s)", receipt.FallbackDeniedReason)
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, "; ")
}

func formatFallbacksByClass(byClass map[team.ProviderFailureClass]int) string {
	classes := make([]string, 0, len(byClass))
	for class := range byClass {
		classes = append(classes, string(class))
	}
	sort.Strings(classes)
	parts := make([]string, 0, len(classes))
	for _, class := range classes {
		parts = append(parts, fmt.Sprintf("%s: %d", class, byClass[team.ProviderFailureClass(class)]))
	}
	return strings.Join(parts, ", ")
}

// writeWorkerAttemptReport renders the Workers section from the canonical
// attempt projection, the same one `hufu status --workers` and the TUI show.
func writeWorkerAttemptReport(b *strings.Builder, hub *inspectpkg.WorkerAttempts) {
	if hub == nil || len(hub.Attempts) == 0 {
		return
	}
	summary := hub.Summary
	b.WriteString("### Workers\n\n")
	fmt.Fprintf(b, "- **Attempts:** %d (isolated: %d, workspace conflicts: %d, orphan worlds removed: %d)\n",
		summary.Attempts, summary.IsolatedAttemptsTotal, summary.AttemptWorkspaceConflictsTotal, summary.AttemptWorldsOrphanRemoved)
	fmt.Fprintf(b, "- **Structured result validation failures:** %d\n\n", summary.StructuredResultValidationFailures)
	b.WriteString("| Task | Agent | Attempt | Status | Activity | Target | Workspace | Duration | Tokens | Fallbacks | Failure |\n")
	b.WriteString("|------|-------|---------|--------|----------|--------|-----------|----------|--------|-----------|---------|\n")
	for _, attempt := range hub.Attempts {
		tokens := "unknown"
		if attempt.Usage != nil {
			tokens = fmt.Sprint(attempt.Usage.TotalTokens)
		}
		fmt.Fprintf(b, "| %s | %s | %d | %s | %s | %s | %s | %s | %s | %d | %s |\n",
			reportTableValue(attempt.TaskID, 40), reportTableValue(valueOrUnknown(attempt.Agent), 64), attempt.Attempt,
			reportTableValue(valueOrUnknown(attempt.TaskStatus), 32), reportTableValue(attempt.Activity, 32),
			reportTableValue(valueOrUnknown(attempt.ExecutionTarget), 120), reportTableValue(attempt.WorkspaceMode, 16),
			workerAttemptAge(attempt), tokens, attempt.FallbackCount, reportTableValue(valueOr(attempt.FailureClass, "—"), 64))
	}
	b.WriteString("\n")
}
