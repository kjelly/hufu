package team

import (
	"context"
	"encoding/json/v2"
	"reflect"
	"slices"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/media"
)

type mediaCreatedOccurrence struct {
	Binding  *media.TaskBinding        `json:"media_binding"`
	Digest   string                    `json:"media_input_digest"`
	Agent    string                    `json:"agent"`
	Revision int                       `json:"occurrence_revision"`
	Target   execution.ExecutionTarget `json:"execution_target"`
}

// VerifyNativeMediaLineage audits a caller-verified branch lineage against
// frozen policies, durable input/binding events, private CAS and retained
// immutable conversation checkpoints. It does not consult session projections
// or a live coordinator and never repairs or writes evidence.
func VerifyNativeMediaLineage(ctx context.Context, workspace string, lineage []RunEvent) error {
	hasMedia, hasCheckpoints := false, false
	for _, event := range lineage {
		if event.Type == string(EventMediaInputAdmitted) || event.Type == string(EventTaskMediaBound) || event.Type == string(EventMediaProviderAdmitted) {
			hasMedia = true
		}
		if event.Type == string(EventTaskCreated) && len(event.Payload) > 0 {
			var created mediaCreatedOccurrence
			if err := json.Unmarshal(event.Payload, &created); err != nil {
				return err
			}
			if created.Binding != nil || created.Digest != "" {
				hasMedia = true
			}
		}
		if event.Type == compactionCheckpointEventType {
			hasCheckpoints = true
		}
	}
	if hasMedia {
		if err := verifyNativeMediaAdmissions(ctx, workspace, lineage); err != nil {
			return err
		}
	}
	if !hasCheckpoints {
		return nil
	}
	return verifyRetainedMediaCheckpoints(ctx, workspace, lineage, hasMedia)
}

type nativeMediaVerifier struct {
	policy   *MediaPolicySnapshot
	sources  map[string]media.InputSnapshot
	created  map[string]mediaCreatedOccurrence
	bound    map[string]bool
	verified map[string]media.Ref
	store    *FileArtifactStore
	prior    []RunEvent
}

func verifyNativeMediaAdmissions(ctx context.Context, workspace string, lineage []RunEvent) error {
	store, err := OpenFileArtifactStoreReadOnly(workspace, workspace)
	if err != nil {
		return err
	}
	verifier := nativeMediaVerifier{sources: make(map[string]media.InputSnapshot), created: make(map[string]mediaCreatedOccurrence), bound: make(map[string]bool), verified: make(map[string]media.Ref), store: store}
	for index, event := range lineage {
		if err := ctx.Err(); err != nil {
			return err
		}
		if event.Type == string(EventMediaInputAdmitted) {
			snapshot, err := decodeMediaInputEvent(event)
			if err != nil {
				return err
			}
			for _, occurrence := range snapshot.Occurrences {
				if occurrence.Origin == "mcp_tool" && (index+1 == len(lineage) || lineage[index+1].Type != string(EventToolMediaPublished)) {
					return media.Reject(media.DurabilityRequired, "tool image snapshot lacks its atomic producer publication")
				}
			}
		}
		var previous *RunEvent
		if index > 0 {
			previous = &lineage[index-1]
		}
		verifier.prior = lineage[:index]
		if err := verifier.accept(ctx, event, previous); err != nil {
			return err
		}
	}
	for taskID := range verifier.created {
		if !verifier.bound[taskID] {
			return media.Reject(media.DurabilityRequired, "media task lacks its atomic binding event")
		}
	}
	return nil
}

func (v *nativeMediaVerifier) accept(ctx context.Context, event RunEvent, previous *RunEvent) error {
	switch EventType(event.Type) {
	case EventExecutionPolicySnapshot, EventExecutionPolicySnapshotMigrated, EventContractRecovery, EventDerivedForkCreated:
		return v.acceptPolicy(event)
	case EventMediaInputAdmitted:
		return v.acceptInput(ctx, event)
	case EventTaskCreated:
		return v.acceptCreation(event)
	case EventTaskMediaBound:
		return v.acceptBinding(event, previous)
	case EventToolMediaPublished:
		payload, err := decodeToolMediaPublication(event)
		if err != nil {
			return err
		}
		return verifyToolMediaPublication(event, payload, v.prior)
	case EventMediaConsumerBound:
		payload, err := decodeMediaConsumerEvent(event)
		if err != nil {
			return err
		}
		return verifyMediaConsumer(event, payload, v.prior)
	case EventMediaProviderAdmitted:
		payload, err := decodeMediaProviderEvent(event)
		if err != nil {
			return err
		}
		return verifyMediaProviderAdmission(event, payload, v.prior)
	default:
		return v.acceptTransition(event)
	}
}

func (v *nativeMediaVerifier) acceptPolicy(event RunEvent) error {
	policy, err := executionPolicySnapshotFromEvents(slices.Concat(v.prior, []RunEvent{event}))
	if err != nil {
		return err
	}
	v.policy = nil
	if policy != nil {
		v.policy = policy.Media.clone()
	}
	return nil
}

func (v *nativeMediaVerifier) acceptInput(ctx context.Context, event RunEvent) error {
	snapshot, err := decodeMediaInputEvent(event)
	if err != nil {
		return err
	}
	policy := v.policy
	if policy == nil || policy.Hash != snapshot.PolicyHash {
		return media.Reject(media.EgressDenied, "media input has no preceding frozen policy")
	}
	if err := snapshot.Validate(policy.Policy.Image); err != nil {
		return err
	}
	if err := checkMediaBranchQuota(v.sources, snapshot, policy.Policy.Image); err != nil {
		return err
	}
	for _, ref := range snapshot.Refs {
		if previous, ok := v.verified[ref.ID]; ok {
			if previous != ref {
				return media.Reject(media.IntegrityMismatch, "admitted media metadata conflicts")
			}
			continue
		}
		if err := verifyHistoryImage(ctx, v.store, ref, true); err != nil {
			return err
		}
		v.verified[ref.ID] = ref
	}
	v.sources[snapshot.ID] = snapshot
	return nil
}

func (v *nativeMediaVerifier) acceptCreation(event RunEvent) error {
	var occurrence mediaCreatedOccurrence
	if err := json.Unmarshal(event.Payload, &occurrence); err != nil {
		return err
	}
	if occurrence.Binding == nil && occurrence.Digest == "" {
		if _, exists := v.created[event.TaskID]; exists {
			return media.Reject(media.BindingConflict, "media lifecycle reset omitted its original binding")
		}
		return nil
	}
	if err := validateTaskMediaShape(&TodoItem{ID: event.TaskID, Agent: occurrence.Agent, MediaBinding: occurrence.Binding, MediaInputDigest: occurrence.Digest}); err != nil {
		return err
	}
	_, reset, err := decodeMediaTaskReset(event)
	if err != nil {
		return err
	}
	if reset {
		original, exists := v.created[event.TaskID]
		if !exists || !v.bound[event.TaskID] || !reflect.DeepEqual(original.Binding, occurrence.Binding) || original.Digest != occurrence.Digest {
			return media.Reject(media.AccessDenied, "media reset has no original atomic binding")
		}
		return verifyMediaTaskReset(event, v.prior)
	}
	if err := verifyMediaCreationPolicy(event, occurrence, v.policy, v.sources); err != nil {
		return err
	}
	if occurrence.Binding.InvocationMode != "" {
		if err := verifyNativeMediaInvocationCreation(event, v.prior); err != nil {
			return err
		}
	} else if err := validateMediaInvocationSourceOwner(occurrence.Binding, v.prior); err != nil {
		return err
	}
	if _, exists := v.created[event.TaskID]; exists {
		return media.Reject(media.BindingConflict, "media task has multiple creation events")
	}
	v.created[event.TaskID] = occurrence
	return nil
}

func (v *nativeMediaVerifier) acceptBinding(event RunEvent, previous *RunEvent) error {
	payload, err := decodeTaskMediaEvent(event)
	if err != nil {
		return err
	}
	occurrence, ok := v.created[event.TaskID]
	if !ok || previous == nil || previous.Type != string(EventTaskCreated) || previous.TaskID != event.TaskID || payload.OccurrenceRevision != occurrence.Revision || !reflect.DeepEqual(&payload.Binding, occurrence.Binding) {
		return media.Reject(media.BindingConflict, "task media event is not paired with its canonical creation")
	}
	v.bound[event.TaskID] = true
	return nil
}

func (v *nativeMediaVerifier) acceptTransition(event RunEvent) error {
	if err := verifyRuntimeMediaTransition(event, v.prior); err != nil {
		return err
	}
	occurrence, ok := v.created[event.TaskID]
	if ok && event.Type == string(EventTaskResultCollectionCheckpointed) {
		var collection resultCollectionEvent
		if err := json.Unmarshal(event.Payload, &collection, collectionDurationFormat); err != nil {
			return err
		}
		if collection.Receipt.MediaInputDigest != occurrence.Digest {
			return media.Reject(media.BindingConflict, "result collection changed its media input digest")
		}
		return nil
	}
	if !ok || !mediaTransitionCarriesBinding(EventType(event.Type)) {
		return nil
	}
	var transition mediaCreatedOccurrence
	if err := json.Unmarshal(event.Payload, &transition); err != nil {
		return err
	}
	if !reflect.DeepEqual(transition.Binding, occurrence.Binding) || transition.Digest != occurrence.Digest {
		return media.Reject(media.BindingConflict, "task transition changed its media input binding")
	}
	return validateTaskMediaEventFields(event)
}

func verifyMediaCreationPolicy(event RunEvent, occurrence mediaCreatedOccurrence, policy *MediaPolicySnapshot, sources map[string]media.InputSnapshot) error {
	binding := occurrence.Binding
	if policy == nil || binding.PolicyHash != policy.Hash || binding.RunID != event.RunID || binding.BranchID != event.BranchID {
		return media.Reject(media.EgressDenied, "task media owner differs from its frozen creation policy")
	}
	allowed := false
	for _, target := range policy.Policy.AllowedTargets {
		if target.Backend == occurrence.Target.Backend && target.Model == occurrence.Target.Model {
			allowed = true
			break
		}
	}
	if !allowed {
		return media.Reject(media.EgressDenied, "media task target was not granted")
	}
	return mediaBindingSources(binding, mediaLineageView{snapshots: sources}, policy.Policy.Image)
}

func mediaTransitionCarriesBinding(kind EventType) bool {
	switch kind {
	case EventTaskPlanned, EventTaskStarted, EventTaskVerifying, EventTaskPaused, EventTaskCompleted, EventTaskFailed, EventTaskBlocked, EventTaskSkipped, EventTaskProtocolIncomplete, EventTaskCancelled:
		return true
	}
	return false
}

func verifyRetainedMediaCheckpoints(ctx context.Context, workspace string, lineage []RunEvent, hasMedia bool) error {
	state, exists, err := LoadConversationCompactionState(workspace)
	if err != nil {
		return err
	}
	if !exists {
		if hasMedia {
			return media.Reject(media.DurabilityRequired, "attested media history has no canonical state")
		}
		return nil
	}
	for index, event := range lineage {
		if event.Type != compactionCheckpointEventType {
			continue
		}
		reference, err := compactionCheckpointReferenceFromEvent(event)
		if err != nil {
			return err
		}
		checkpoint, retained := findCompactionCheckpoint(state, reference)
		if !retained {
			continue
		}
		hasReferences, err := agent.ValidatePersistentMessages(checkpoint.History)
		if err != nil {
			return err
		}
		if !hasReferences {
			continue
		}
		generation := state.Generations[checkpoint.GenerationID]
		if !compactionCheckpointAttestationMatches(event, checkpoint, generation) {
			return media.Reject(media.IntegrityMismatch, "media checkpoint differs from its attestation")
		}
		if err := validateMediaHistoryLineage(ctx, workspace, lineage[:index+1], checkpoint.History, true); err != nil {
			return err
		}
	}
	return nil
}
