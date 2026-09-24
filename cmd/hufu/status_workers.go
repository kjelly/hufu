package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	tuipkg "github.com/kjelly/hufu/internal/tui"
)

// writeWorkerAttempts renders the worker hub for `hufu status --workers`.
// Every value is rendered as plain text; unknown data is shown as unknown.
func writeWorkerAttempts(out io.Writer, hub inspectpkg.WorkerAttempts, verbose bool) {
	_, _ = fmt.Fprintf(out, "\nWorkers:    %d attempt(s) · %d isolated · %d conflict(s) · %d fallback(s)\n",
		hub.Summary.Attempts, hub.Summary.IsolatedAttemptsTotal, hub.Summary.AttemptWorkspaceConflictsTotal, hub.Summary.WorkerFallbacksTotal)
	if len(hub.Attempts) == 0 {
		return
	}
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "TASK\tAGENT\tATT\tSTATE\tACTIVITY\tTARGET\tWORKSPACE\tAGE")
	for _, attempt := range hub.Attempts {
		_, _ = fmt.Fprintf(table, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
			safeOverviewValue(attempt.TaskID), safeOverviewValue(valueOrUnknown(attempt.Agent)), attempt.Attempt,
			safeOverviewValue(valueOrUnknown(attempt.TaskStatus)), attempt.Activity,
			safeOverviewValue(valueOrUnknown(attempt.ExecutionTarget)), attempt.WorkspaceMode, workerAttemptAge(attempt))
	}
	_ = table.Flush()
	if !verbose {
		return
	}
	for _, attempt := range hub.Attempts {
		_, _ = fmt.Fprintf(out, "\n%s attempt %d (occurrence attempt %d)\n", safeOverviewValue(attempt.TaskID), attempt.Attempt, attempt.OccurrenceAttempt)
		for _, line := range workerAttemptDetailLines(attempt) {
			_, _ = fmt.Fprintf(out, "  %s\n", line)
		}
	}
}

// workerAttemptDetailLines are the --verbose fields of one attempt.
func workerAttemptDetailLines(attempt inspectpkg.WorkerAttemptView) []string {
	candidate := "none"
	if attempt.CandidateIndex != nil {
		candidate = fmt.Sprint(*attempt.CandidateIndex)
	}
	usage := "unknown"
	if attempt.Usage != nil {
		usage = fmt.Sprintf("%d tokens (%d in, %d out)", attempt.Usage.TotalTokens, attempt.Usage.InputTokens, attempt.Usage.OutputTokens)
	}
	world := "none"
	if attempt.AttemptWorldID != "" {
		world = attempt.AttemptWorldID + " (" + valueOrUnknown(attempt.AttemptWorldState) + ")"
	}
	return []string{
		"route: " + safeOverviewValue(valueOr(attempt.RouteName, "none")) + " · candidate: " + candidate +
			fmt.Sprintf(" · retries: %d · fallbacks: %d", attempt.RetryCount, attempt.FallbackCount),
		"usage: " + usage,
		"result contract: " + safeOverviewValue(valueOr(attempt.ResultContractID, "none")) + " · validation: " + safeOverviewValue(valueOrUnknown(attempt.ResultValidation)),
		"failure: " + safeOverviewValue(valueOr(attempt.FailureClass, "none")),
		"world: " + safeOverviewValue(world),
		"identity: key " + attempt.AttemptKey + " · started by " + safeOverviewValue(attempt.StartedEventID) +
			" · run " + safeOverviewValue(valueOrUnknown(attempt.RunID)) + " · invocation " + safeOverviewValue(valueOrUnknown(attempt.InvocationRunID)),
	}
}

func workerAttemptAge(attempt inspectpkg.WorkerAttemptView) string {
	if attempt.DurationMillis <= 0 {
		return "unknown"
	}
	return (time.Duration(attempt.DurationMillis) * time.Millisecond).Round(time.Second).String()
}

func valueOrUnknown(value string) string { return valueOr(value, "unknown") }

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// tuiAttemptDetails converts the canonical attempt projection to the TUI's
// own type, keeping internal/tui independent of internal/inspect.
func tuiAttemptDetails(attempts []inspectpkg.WorkerAttemptView) []tuipkg.OperatorAttemptDetail {
	details := make([]tuipkg.OperatorAttemptDetail, 0, len(attempts))
	for _, attempt := range attempts {
		detail := tuipkg.OperatorAttemptDetail{
			TaskID: attempt.TaskID, Attempt: attempt.Attempt, Target: attempt.ExecutionTarget,
			Workspace: attempt.WorkspaceMode, Activity: attempt.Activity, Status: attempt.TaskStatus,
			Duration: time.Duration(attempt.DurationMillis) * time.Millisecond, Fallbacks: attempt.FallbackCount,
			FailureClass: attempt.FailureClass,
		}
		if attempt.Usage != nil {
			detail.Tokens, detail.TokensKnown = attempt.Usage.TotalTokens, true
		}
		details = append(details, detail)
	}
	return details
}
