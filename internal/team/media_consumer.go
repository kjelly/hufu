package team

import (
	"encoding/json/v2"
	"reflect"

	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/media"
)

// MediaConsumerBoundPayload describes one complete logical provider request.
// It never replaces the immutable task input or grants another occurrence reads.
type MediaConsumerBoundPayload struct {
	Version              int                       `json:"version"`
	Hash                 string                    `json:"hash"`
	RunID                string                    `json:"run_id"`
	TaskID               string                    `json:"task_id"`
	Agent                string                    `json:"agent"`
	ProviderInvocationID string                    `json:"provider_invocation_id"`
	OccurrenceRevision   int                       `json:"occurrence_revision"`
	Attempt              int                       `json:"attempt"`
	DispatchID           string                    `json:"dispatch_id"`
	PolicyHash           string                    `json:"policy_hash"`
	InputBindingHash     string                    `json:"input_binding_hash,omitempty"`
	Target               execution.ExecutionTarget `json:"target"`
	ModelFanout          MediaProviderFanoutGrant  `json:"model_fanout,omitzero"`
	References           []media.HistoryReference  `json:"references"`
}

func (p MediaConsumerBoundPayload) digest() (string, error) { p.Hash = ""; return media.Digest(p) }

func (p MediaConsumerBoundPayload) refs() []media.Ref {
	refs := make([]media.Ref, len(p.References))
	for index, reference := range p.References {
		refs[index] = reference.Ref
	}
	return refs
}

func decodeMediaConsumerEvent(event RunEvent) (MediaConsumerBoundPayload, error) {
	var p MediaConsumerBoundPayload
	if event.SchemaVersion < eventStoreSchemaVersion || len(event.Payload) > 64<<10 {
		return p, media.Reject(media.BindingConflict, "invalid image consumer envelope")
	}
	if err := json.Unmarshal(event.Payload, &p, json.RejectUnknownMembers(true)); err != nil {
		return p, media.Reject(media.BindingConflict, "invalid image consumer payload")
	}
	hash, err := p.digest()
	if err != nil || p.Version != media.Version || hash != p.Hash || p.RunID != event.RunID || p.TaskID != event.TaskID || p.Agent != event.Actor ||
		p.ProviderInvocationID == "" || len(p.ProviderInvocationID) > 256 || p.OccurrenceRevision < 1 || p.Attempt < 1 || p.DispatchID == "" || len(p.DispatchID) > 256 || p.Target.Validate() != nil || len(p.References) == 0 || len(p.References) > media.MaxCount {
		return p, media.Reject(media.BindingConflict, "image consumer identity or hash mismatch")
	}
	for _, reference := range p.References {
		if err := reference.Validate(); err != nil {
			return p, err
		}
	}
	return p, nil
}

func verifyMediaConsumer(event RunEvent, p MediaConsumerBoundPayload, prior []RunEvent) error {
	items, err := ReplayTodoList(prior)
	if err != nil {
		return err
	}
	item := todoItemByID(items, event.TaskID)
	if item == nil || item.Status != TaskInProgress || item.Agent != event.Actor || currentResultCollection(item) != nil {
		return media.Reject(media.AccessDenied, "image consumer has no executing canonical owner")
	}
	target, err := verifyNativeMediaProviderOwner(event, MediaProviderAdmittedPayload{OccurrenceRevision: p.OccurrenceRevision, Attempt: p.Attempt, DispatchID: p.DispatchID, ModelFanout: p.ModelFanout}, item, prior)
	if err != nil {
		return err
	}
	if target != p.Target || item.MediaBinding == nil && p.InputBindingHash != "" || item.MediaBinding != nil && p.InputBindingHash != item.MediaBinding.Hash {
		return media.Reject(media.BindingConflict, "image consumer differs from its canonical input or target")
	}
	policy, err := mediaProviderPolicyFromLineage(prior)
	if err != nil {
		return err
	}
	if policy.Media == nil || policy.Media.Hash != p.PolicyHash {
		return media.Reject(media.EgressDenied, "image consumer has no frozen media policy")
	}
	producers := make(map[string]ToolMediaPublishedPayload)
	for index, source := range prior {
		if source.Type != string(EventToolMediaPublished) {
			continue
		}
		producer, err := decodeToolMediaPublication(source)
		if err != nil {
			return err
		}
		if err := verifyToolMediaPublication(source, producer, prior[:index]); err != nil {
			return err
		}
		producers[producer.Hash] = producer
	}
	if err := verifyMediaConsumerReferences(p, item, producers); err != nil {
		return err
	}
	return media.CheckBundle(p.refs(), policy.Media.Policy.Image)
}

func verifyMediaConsumerReferences(p MediaConsumerBoundPayload, item *TodoItem, producers map[string]ToolMediaPublishedPayload) error {
	var initial []media.HistoryReference
	toolParts := make(map[string][]int)
	for _, reference := range p.References {
		if producer, ok := producers[reference.BindingHash]; ok {
			if err := verifyToolMediaReference(reference, producer); err != nil {
				return err
			}
			if producer.TaskID != p.TaskID || producer.Agent != p.Agent || producer.Snapshot.RunID != p.RunID || producer.Snapshot.PolicyHash != p.PolicyHash ||
				producer.OccurrenceRevision != p.OccurrenceRevision || producer.Attempt != p.Attempt || producer.DispatchID != p.DispatchID || producer.ModelFanout != p.ModelFanout {
				return media.Reject(media.AccessDenied, "tool image source belongs to another worker occurrence")
			}
			toolParts[producer.Hash] = append(toolParts[producer.Hash], reference.Source.PartIndex)
		} else {
			initial = append(initial, reference)
		}
	}
	if len(toolParts) == 0 {
		return media.Reject(media.AccessDenied, "image consumer has no admitted tool source")
	}
	if item.MediaBinding == nil && len(initial) != 0 || item.MediaBinding != nil && len(initial) != len(item.MediaBinding.Selections) {
		return media.Reject(media.BindingConflict, "image consumer changed its initial ordered input")
	}
	for index, reference := range initial {
		binding := item.MediaBinding
		if reference.Source != binding.Selections[index] || reference.Ref != binding.Refs[index] || reference.PolicyHash != binding.PolicyHash ||
			reference.TaskID != "" && (reference.TaskID != item.ID || reference.BindingHash != binding.Hash) {
			return media.Reject(media.AccessDenied, "image consumer borrowed another task input")
		}
	}
	for hash, indexes := range toolParts {
		var expected []int
		for _, occurrence := range producers[hash].Snapshot.Occurrences {
			expected = append(expected, occurrence.PartIndex)
		}
		if !reflect.DeepEqual(indexes, expected) {
			return media.Reject(media.BindingConflict, "image consumer changed ordered tool occurrences")
		}
	}
	return nil
}
