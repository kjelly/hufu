package team

import (
	"fmt"

	"github.com/kjelly/hufu/internal/cost"
)

func costEventRunID(event cost.Event) string {
	switch event.Kind {
	case cost.EventPriceSnapshotResolved:
		return event.Price.RunID
	case cost.EventReservationCommitted:
		return event.Reservation.RunID
	case cost.EventSettled:
		return event.Settlement.RunID
	case cost.EventBudgetWarning:
		return event.Warning.RunID
	case cost.EventBudgetDenied:
		return event.Denied.RunID
	default:
		return ""
	}
}

func costEventTaskID(event cost.Event) string {
	switch event.Kind {
	case cost.EventReservationCommitted:
		return event.Reservation.TaskID
	case cost.EventSettled:
		return event.Settlement.TaskID
	case cost.EventBudgetDenied:
		return event.Denied.TaskID
	default:
		return ""
	}
}

func costEventAttempt(event cost.Event) int {
	switch event.Kind {
	case cost.EventReservationCommitted:
		return event.Reservation.OccurrenceAttempt
	case cost.EventSettled:
		return event.Settlement.OccurrenceAttempt
	case cost.EventBudgetDenied:
		return event.Denied.OccurrenceAttempt
	default:
		return 0
	}
}

func costEventsFromRunEvents(events []RunEvent) ([]cost.Event, error) {
	result := make([]cost.Event, 0)
	for index, event := range events {
		switch EventType(event.Type) {
		case EventCostPriceSnapshotResolved, EventCostReservationCommitted, EventCostSettled, EventCostBudgetWarning, EventCostBudgetDenied:
			decoded, err := cost.DecodeEvent(event.Type, event.Payload)
			if err != nil {
				return nil, fmt.Errorf("decode cost event %d (%s): %w", index, event.Type, err)
			}
			if costEventRunID(decoded) != event.RunID || costEventTaskID(decoded) != event.TaskID || costEventAttempt(decoded) != event.Attempt {
				return nil, fmt.Errorf("cost event %d (%s) envelope does not match payload", index, event.Type)
			}
			result = append(result, decoded)
		}
	}
	return result, nil
}
