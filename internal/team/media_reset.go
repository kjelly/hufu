package team

import (
	"context"
	"encoding/json/v2"
	"reflect"
	"strings"

	"github.com/kjelly/hufu/internal/media"
)

// Existing task_created reset events advance a lifecycle, not media ownership.
// Their flags alone are never authority: the visible original occurrence and
// frozen policy must independently authorize the unchanged input contract.
type mediaTaskReset struct {
	Resume         bool       `json:"reset_for_resume"`
	Retry          bool       `json:"reset_for_retry"`
	PreviousStatus TaskStatus `json:"previous_status"`
	Status         TaskStatus `json:"status"`
	Revision       int        `json:"occurrence_revision"`
	Retries        int        `json:"retries"`
	DispatchID     string     `json:"dispatch_id"`
}

func decodeMediaTaskReset(event RunEvent) (mediaTaskReset, bool, error) {
	var reset mediaTaskReset
	if err := json.Unmarshal(event.Payload, &reset); err != nil {
		return reset, false, err
	}
	if !reset.Resume && !reset.Retry {
		return reset, false, nil
	}
	if event.Type != string(EventTaskCreated) || reset.Resume == reset.Retry || reset.Status != TaskPending ||
		reset.PreviousStatus == "" || reset.Revision <= 0 || reset.Retries < 0 || reset.DispatchID != "" {
		return reset, false, media.Reject(media.BindingConflict, "invalid media lifecycle reset")
	}
	if reset.Resume && (!isInterruptedStatus(reset.PreviousStatus) || reset.PreviousStatus == TaskPending ||
		reset.PreviousStatus == TaskPlanned || reset.PreviousStatus == TaskProtocolIncomplete) {
		return reset, false, media.Reject(media.AccessDenied, "media resume reset is outside interrupted work")
	}
	return reset, true, nil
}

func verifyMediaTaskReset(event RunEvent, prior []RunEvent) error {
	reset, exists, err := decodeMediaTaskReset(event)
	if err != nil || !exists {
		return media.Reject(media.BindingConflict, "media reset has no valid lifecycle declaration")
	}
	items, err := ReplayTodoList(prior)
	if err != nil {
		return err
	}
	previous := todoItemByID(items, event.TaskID)
	// Decode the complete incoming contract through the ordinary reducer in
	// isolation. This is not an admitted creation; ownership is proven below.
	projected, err := ReplayTodoList([]RunEvent{event})
	if err != nil {
		return err
	}
	next := todoItemByID(projected, event.TaskID)
	if previous == nil || next == nil || previous.MediaBinding == nil || !strings.EqualFold(event.Actor, previous.Agent) ||
		previous.Status != reset.PreviousStatus || previous.OccurrenceRevision < 0 || reset.Revision <= previous.OccurrenceRevision ||
		reset.Revision != previous.OccurrenceRevision+1 || !reflect.DeepEqual(previous.MediaBinding, next.MediaBinding) ||
		previous.MediaInputDigest != next.MediaInputDigest {
		return media.Reject(media.AccessDenied, "media reset differs from its visible original occurrence")
	}
	if err := verifyMediaTaskOwner(previous, prior); err != nil {
		return err
	}
	expectedRetries := previous.Retries
	if reset.Retry {
		expectedRetries++
	}
	if expectedRetries < 0 || reset.Retries != expectedRetries {
		return media.Reject(media.BindingConflict, "media reset changed its attempt counter")
	}
	before, err := decisionTaskInputDigest(previous)
	if err != nil {
		return err
	}
	after, err := decisionTaskInputDigest(next)
	if err != nil || before != after {
		return media.Reject(media.BindingConflict, "media reset changed its immutable execution contract")
	}
	policy, err := mediaProviderPolicyFromLineage(prior)
	if err != nil {
		return err
	}
	if policy.Media == nil || policy.Media.Hash != previous.MediaBinding.PolicyHash {
		return media.Reject(media.EgressDenied, "media reset has no current frozen input policy")
	}
	if err := media.CheckBundle(previous.MediaBinding.Refs, policy.Media.Policy.Image); err != nil {
		return err
	}
	return nil
}

// A lifecycle reset can only continue the single atomic creation/binding pair.
// Later reset declarations are not allowed to supply either half of that pair.
func verifyMediaTaskOwner(item *TodoItem, prior []RunEvent) error {
	created := false
	for index, event := range prior {
		if event.TaskID != item.ID || event.Type != string(EventTaskCreated) {
			continue
		}
		_, reset, err := decodeMediaTaskReset(event)
		if err != nil {
			return err
		}
		if reset {
			continue
		}
		var original mediaCreatedOccurrence
		if err := json.Unmarshal(event.Payload, &original); err != nil {
			return err
		}
		if created || event.RunID != item.MediaBinding.RunID || event.BranchID != item.MediaBinding.BranchID ||
			!reflect.DeepEqual(original.Binding, item.MediaBinding) || original.Digest != item.MediaInputDigest || index+1 >= len(prior) {
			return media.Reject(media.AccessDenied, "media reset has no unique original owner")
		}
		boundEvent := prior[index+1]
		if boundEvent.Type != string(EventTaskMediaBound) || boundEvent.TaskID != item.ID {
			return media.Reject(media.AccessDenied, "media reset has no atomic original binding")
		}
		bound, err := decodeTaskMediaEvent(boundEvent)
		if err != nil {
			return err
		}
		if bound.OccurrenceRevision != original.Revision || !reflect.DeepEqual(&bound.Binding, item.MediaBinding) {
			return media.Reject(media.BindingConflict, "media reset changed its original binding")
		}
		created = true
	}
	if !created {
		return media.Reject(media.AccessDenied, "media reset has no original creation")
	}
	return nil
}

// appendMediaTaskReset binds the reset to the same verified journal head used
// for validation. A concurrent writer requires an ordinary recovery retry;
// it cannot turn an old proof into a new owner or a different contract.
func (c *Coordinator) appendMediaTaskReset(ctx context.Context, event RunEvent) (RunEvent, error) {
	view, err := c.mediaLineage(ctx)
	if err != nil {
		return RunEvent{}, err
	}
	event.RunID, event.BranchID = c.executionRunID, c.activeBranchID()
	if err := verifyMediaTaskReset(event, view.events); err != nil {
		return RunEvent{}, err
	}
	journal, ok := c.EventJournal().(ConditionalEventJournal)
	if !ok {
		return RunEvent{}, media.Reject(media.DurabilityRequired, "media reset requires a conditional journal")
	}
	return journal.AppendAtHead(ctx, event, view.head)
}
