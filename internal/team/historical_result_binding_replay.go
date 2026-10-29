package team

import (
	"encoding/json/v2"
	"reflect"
	"slices"
)

// A binding must originate in the first task_created contract and name the
// grant already visible at that point. A checkpoint or later lifecycle event
// cannot retroactively bind a task, even if it copies a real grant's metadata.
func validateHistoricalResultTaskBindingReplay(events []RunEvent, item *TodoItem) error {
	if err := validateHistoricalResultTaskBindingShape(item); err != nil {
		return err
	}
	if item.HistoricalResultBinding == nil {
		return nil
	}
	for index, event := range events {
		if event.Type != string(EventTaskCreated) || event.TaskID != item.ID {
			continue
		}
		var created struct {
			Binding *HistoricalResultTaskBinding `json:"historical_result_binding"`
		}
		if err := json.Unmarshal(event.Payload, &created); err != nil || !reflect.DeepEqual(created.Binding, item.HistoricalResultBinding) {
			return ErrHistoricalResultTaskBinding
		}
		ref, payload, err := historicalResultGrantProjection(events[:index])
		if err != nil || ref == nil || *ref != created.Binding.Grant {
			return ErrHistoricalResultTaskBinding
		}
		var ids []string
		for _, grant := range payload.Grants {
			if grant.Recipient == created.Binding.Recipient {
				ids = append(ids, grant.ResultID)
			}
		}
		if !slices.Equal(ids, created.Binding.ResultIDs) {
			return ErrHistoricalResultTaskBinding
		}
		return nil
	}
	return ErrHistoricalResultTaskBinding
}
