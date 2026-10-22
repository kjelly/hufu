package inspect

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/kjelly/hufu/internal/auditverify"
	"github.com/kjelly/hufu/internal/team"
)

func routeAttempts(todo *team.TodoItem, task RouteTask, views []WorkerAttemptView, events []IndexedEvent, runID string) ([]RouteAttempt, error) {
	return routeAttemptsContext(context.Background(), todo, task, views, events, runID)
}

func routeAttemptsContext(ctx context.Context, todo *team.TodoItem, task RouteTask, views []WorkerAttemptView, events []IndexedEvent, runID string) ([]RouteAttempt, error) {
	var selected []WorkerAttemptView
	for _, view := range views {
		if view.TaskID == todo.ID && view.InvocationRunID == runID {
			selected = append(selected, view)
		}
	}
	branches := make(map[string]string, len(events))
	ordinals := make(map[string]int64, len(events))
	for _, event := range events {
		branches[event.Event.ID] = event.Event.BranchID
		ordinals[event.Event.ID] = event.Ordinal
	}
	slices.SortStableFunc(selected, func(a, b WorkerAttemptView) int {
		return cmp.Compare(ordinals[a.StartedEventID], ordinals[b.StartedEventID])
	})
	byStart := make(map[string][]team.ExecutionReceipt)
	var unbound []team.ExecutionReceipt
	for _, receipt := range todo.ExecutionReceipts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if receipt.RunID != runID || receipt.TaskID != todo.ID {
			continue
		}
		if err := routeReceiptTargetIntegrity(todo, receipt); err != nil {
			return nil, err
		}
		index := routeReceiptStart(receipt, selected)
		if index < 0 {
			unbound = append(unbound, receipt)
		} else {
			key := selected[index].StartedEventID
			byStart[key] = append(byStart[key], receipt)
		}
	}
	var rows []RouteAttempt
	for _, view := range selected {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		receipts := byStart[view.StartedEventID]
		if len(receipts) == 0 {
			// The primary is an admission fact, not proof that this attempt
			// reached that target. Do not turn it into an observed target.
			view.ExecutionTarget, view.Usage = "", nil
			view.DurationMillis = 0
			if !view.StartedAt.IsZero() && !view.FinishedAt.IsZero() && !view.FinishedAt.Before(view.StartedAt) {
				view.DurationMillis = view.FinishedAt.Sub(view.StartedAt).Milliseconds()
			}
			rows = append(rows, RouteAttempt{
				WorkerAttemptView: view, ExecutionKey: view.AttemptKey, SourceBranchID: branches[view.StartedEventID],
				RouteKind: task.RouteKind, RouteDigest: task.RouteDigest, AttemptClass: "unavailable",
				Counted: true,
			})
			continue
		}
		for _, receipt := range receipts {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			row, err := routeReceiptRow(view, receipt, task, branches[view.StartedEventID], events)
			if err != nil {
				return nil, err
			}
			rows = append(rows, row)
		}
	}
	for _, receipt := range unbound {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Legacy data can have a receipt without a dispatch-start event.
		// Keep the real execution, but do not invent a start or retry class.
		view := WorkerAttemptView{
			RunID: task.OriginRunID, InvocationRunID: runID, TaskID: todo.ID, Agent: todo.Agent,
			Attempt: receipt.Attempt, OccurrenceAttempt: receipt.OccurrenceAttempt,
			TaskStatus: string(todo.Status), Activity: WorkerActivityUnknown, RouteName: task.RouteName,
		}
		row, err := routeReceiptRow(view, receipt, task, "", events)
		if err != nil {
			return nil, err
		}
		if len(routeReceiptCandidates(receipt, selected)) > 1 {
			row.Counted, row.Winning = false, false
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func routeReceiptTargetIntegrity(todo *team.TodoItem, receipt team.ExecutionReceipt) error {
	route := todo.ExecutionRoute
	if route == nil || len(route.Candidates) == 0 {
		return nil
	}
	if receipt.CandidateIndex != nil {
		index := *receipt.CandidateIndex
		if index < 0 || index >= len(route.Candidates) ||
			!receipt.ExecutionTarget.IsZero() && receipt.ExecutionTarget != route.Candidates[index] {
			return fmt.Errorf("%w: receipt candidate contradicts its frozen route", ErrIntegrity)
		}
	}
	if !receipt.ExecutionTarget.IsZero() && !slices.Contains(route.Candidates, receipt.ExecutionTarget) {
		return fmt.Errorf("%w: receipt target is outside its frozen route", ErrIntegrity)
	}
	return nil
}

func routeReceiptStart(receipt team.ExecutionReceipt, views []WorkerAttemptView) int {
	matches := routeReceiptCandidates(receipt, views)
	if len(matches) == 1 {
		return matches[0]
	}
	// Re-dispatch can reset the in-dispatch number within the same run.
	// Only a unique persisted time interval disambiguates that situation.
	match := -1
	if receipt.StartedAt.IsZero() {
		return match
	}
	for _, i := range matches {
		start := views[i].StartedAt
		var end time.Time
		if i+1 < len(views) {
			end = views[i+1].StartedAt
		}
		if start.IsZero() || receipt.StartedAt.Before(start) || !end.IsZero() && !receipt.StartedAt.Before(end) {
			continue
		}
		if match >= 0 {
			return -1
		}
		match = i
	}
	return match
}

func routeReceiptCandidates(receipt team.ExecutionReceipt, views []WorkerAttemptView) []int {
	var matches []int
	for i, view := range views {
		if receipt.Attempt == view.Attempt && (receipt.OccurrenceAttempt == 0 || receipt.OccurrenceAttempt == view.OccurrenceAttempt) {
			matches = append(matches, i)
		}
	}
	return matches
}

func routeReceiptRow(view WorkerAttemptView, receipt team.ExecutionReceipt, task RouteTask, branch string, events []IndexedEvent) (RouteAttempt, error) {
	hash, err := auditverify.HashExecutionReceipt(receipt)
	if err != nil {
		return RouteAttempt{}, fmt.Errorf("%w: cannot hash execution receipt", ErrIntegrity)
	}
	view.ExecutionTarget = ""
	if receipt.ExecutionTarget.Validate() == nil {
		view.ExecutionTarget = receipt.ExecutionTarget.String()
	}
	if branch == "" {
		if anchor := findReceiptAnchor(events, receipt); anchor != nil {
			branch = anchor.Event.BranchID
		}
	}
	view.CandidateIndex = nil
	if receipt.CandidateIndex != nil {
		view.CandidateIndex = new(*receipt.CandidateIndex)
	}
	view.ResultContractID, view.ResultValidation = receipt.ResultContractID, string(receipt.ResultValidation)
	view.Usage = nil
	if receipt.Usage != nil {
		usage := *receipt.Usage
		view.Usage = &usage
	}
	if !receipt.StartedAt.IsZero() {
		view.StartedAt = receipt.StartedAt
	}
	if !receipt.FinishedAt.IsZero() {
		view.FinishedAt = receipt.FinishedAt
	}
	view.DurationMillis = 0
	if !view.StartedAt.IsZero() && !view.FinishedAt.IsZero() && !view.FinishedAt.Before(view.StartedAt) {
		view.DurationMillis = view.FinishedAt.Sub(view.StartedAt).Milliseconds()
	}
	class := "initial"
	switch {
	case view.StartedEventID == "":
		class = "unavailable"
	case receipt.FallbackFrom != nil || view.Attempt > 1 && view.RetryCount == 0 && view.FallbackCount > 0:
		class = "fallback"
	case view.RetryCount > 0 || view.OccurrenceAttempt > 1:
		class = "retry"
	}
	return RouteAttempt{
		WorkerAttemptView: view, ExecutionKey: view.AttemptKey + ":" + hash,
		ModelExecutionID: receipt.ModelExecutionID, ReceiptHash: hash, SourceBranchID: branch,
		RouteKind: task.RouteKind, RouteDigest: task.RouteDigest, AttemptClass: class,
		Winning: task.WinningReceiptHash != "" && task.WinningReceiptHash == hash,
		Counted: true,
	}, nil
}
