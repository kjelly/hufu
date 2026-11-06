package team

import (
	"reflect"
	"strings"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
)

func verifyToolMediaPublication(event RunEvent, payload ToolMediaPublishedPayload, prior []RunEvent) error {
	if len(prior) == 0 || prior[len(prior)-1].Type != string(EventMediaInputAdmitted) {
		return media.Reject(media.DurabilityRequired, "tool media lacks its atomic source snapshot")
	}
	snapshot, err := decodeMediaInputEvent(prior[len(prior)-1])
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(snapshot, payload.Snapshot) {
		return media.Reject(media.BindingConflict, "tool media snapshot differs from its producer")
	}
	items, err := ReplayTodoList(prior)
	if err != nil {
		return err
	}
	item := todoItemByID(items, event.TaskID)
	if item == nil || item.Status != TaskInProgress || currentResultCollection(item) != nil {
		return media.Reject(media.AccessDenied, "tool media has no executing canonical owner")
	}
	if payload.ActionInvocationID == "" {
		if !strings.EqualFold(item.Agent, event.Actor) {
			return media.Reject(media.AccessDenied, "tool media producer differs from its worker owner")
		}
		if _, err := verifyNativeMediaProviderOwner(event, MediaProviderAdmittedPayload{OccurrenceRevision: payload.OccurrenceRevision, Attempt: payload.Attempt,
			DispatchID: payload.DispatchID, ModelFanout: payload.ModelFanout}, item, prior); err != nil {
			return err
		}
	} else if item.Action == nil || !strings.EqualFold(item.Agent, payload.Agent) || payload.OccurrenceRevision != item.OccurrenceRevision || payload.DispatchID != item.DispatchID || payload.ModelFanout.present() {
		return media.Reject(media.AccessDenied, "tool media producer differs from its native action occurrence")
	}
	policy, err := mediaProviderPolicyFromLineage(prior)
	if err != nil {
		return err
	}
	if policy.Media == nil || policy.Media.Hash != snapshot.PolicyHash {
		return media.Reject(media.EgressDenied, "tool media has no frozen media policy")
	}
	if err := snapshot.Validate(policy.Media.Policy.Image); err != nil {
		return err
	}
	if payload.ActionInvocationID != "" {
		return verifyMCPActionMediaSource(event, payload, item, policy, prior)
	}
	if authorization := item.DynamicToolAuthorization; authorization != nil {
		for _, target := range authorization.Targets {
			if target.Name == payload.ToolName {
				if target.DescriptorSHA256 != payload.DescriptorSHA256 {
					return media.Reject(media.AccessDenied, "tool media descriptor differs from its frozen target")
				}
				return nil
			}
		}
	}
	return media.Reject(media.AccessDenied, "tool image producer is outside the frozen tool authorization")
}

func verifyToolMediaReference(reference media.HistoryReference, producer ToolMediaPublishedPayload) error {
	if err := reference.Validate(); err != nil {
		return err
	}
	snapshot := producer.Snapshot
	if reference.TaskID != producer.TaskID || reference.BindingHash != producer.Hash || reference.PolicyHash != snapshot.PolicyHash ||
		reference.Source.SnapshotID != snapshot.ID || reference.Source.SnapshotHash != snapshot.Hash || reference.Source.TurnID != snapshot.TurnID || reference.Source.PartIndex >= len(snapshot.Input.Parts) {
		return media.Reject(media.AccessDenied, "tool image reference differs from its canonical producer")
	}
	part := snapshot.Input.Parts[reference.Source.PartIndex]
	if part.Type != "image" || part.MediaID != reference.Ref.ID || !snapshotHasImage(snapshot, reference.Ref) {
		return media.Reject(media.IntegrityMismatch, "tool image reference differs from its published content")
	}
	return nil
}

func validateToolMediaCarriers(messages []fantasy.Message, producers map[string]ToolMediaPublishedPayload) error {
	for _, message := range messages {
		for _, part := range message.Content {
			result, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !ok {
				continue
			}
			carrier, err := agent.DecodeToolMediaMetadata(result.ClientMetadata)
			if err != nil {
				return err
			}
			if carrier == nil {
				continue
			}
			var producer ToolMediaPublishedPayload
			for _, block := range carrier.Parts {
				if block.Type == "image" {
					producer = producers[block.Image.BindingHash]
					break
				}
			}
			if producer.Hash == "" || len(carrier.Parts) != len(producer.Snapshot.Input.Parts) {
				return media.Reject(media.AccessDenied, "tool media carrier has no complete canonical producer")
			}
			for index, block := range carrier.Parts {
				original := producer.Snapshot.Input.Parts[index]
				if block.Type != original.Type || block.Text != original.Text {
					return media.Reject(media.BindingConflict, "tool media carrier changed canonical content order")
				}
				if block.Type == "image" {
					if block.Image.Source.PartIndex != index {
						return media.Reject(media.BindingConflict, "tool media carrier changed canonical image order")
					}
					if err := verifyToolMediaReference(block.Image, producer); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}
