package team

import (
	"encoding/json/v2"
	"strings"

	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/modelprofile"
)

// verifyMediaProviderAdmission uses only the preceding canonical lineage.
// Admission records permission, not a claim that inference actually occurred.
func verifyMediaProviderAdmission(event RunEvent, payload MediaProviderAdmittedPayload, prior []RunEvent) error {
	if err := validateMediaProviderEstimate(payload); err != nil {
		return err
	}
	tasks, err := ReplayTodoList(prior)
	if err != nil {
		return err
	}
	item := todoItemByID(tasks, event.TaskID)
	if item == nil || item.Status != TaskInProgress || item.MediaBinding == nil || !strings.EqualFold(item.Agent, event.Actor) {
		return media.Reject(media.AccessDenied, "image provider admission has no current canonical occurrence")
	}
	binding := item.MediaBinding
	if binding.Hash != payload.BindingHash || binding.InputDigest != payload.MediaInputDigest || binding.PolicyHash != payload.PolicyHash ||
		len(binding.Refs) != payload.ImageCount {
		return media.Reject(media.BindingConflict, "image provider admission differs from canonical task input")
	}
	expectedTarget, err := verifyNativeMediaProviderOwner(event, payload, item, prior)
	if err != nil {
		return err
	}
	if expectedTarget != payload.Target {
		return media.Reject(media.BindingConflict, "image provider admission differs from the current attempt target")
	}
	policy, err := mediaProviderPolicyFromLineage(prior)
	if err != nil {
		return err
	}
	if policy.Media == nil || policy.Media.Hash != payload.PolicyHash {
		return media.Reject(media.EgressDenied, "image provider admission has no frozen media policy")
	}
	if binding.InvocationMode != "" {
		if err := validateNativeMediaInvocation(item, &policy); err != nil {
			return err
		}
		if err := validateNativeMediaSourceScope(binding, prior); err != nil {
			return err
		}
	}
	if err := verifyMediaProviderBackend(policy, payload); err != nil {
		return err
	}
	grant, err := policy.Media.Policy.Allow(payload.Target.Backend, payload.ProviderKey, payload.Target.Model)
	if err != nil {
		return err
	}
	if grant.IdentityHash != payload.ProviderConfigurationHash || grant.ImageTokenUpperBound != payload.ImageTokenUpperBound {
		return media.Reject(media.EgressDenied, "image provider identity or estimate differs from frozen policy")
	}
	if err := media.CheckBundle(binding.Refs, policy.Media.Policy.Image); err != nil {
		return err
	}
	if err := verifyMediaProviderProfile(event, payload, prior); err != nil {
		return err
	}
	return verifyMediaProviderInvocationUniqueness(event, payload, prior)
}

func mediaProviderPolicyFromLineage(prior []RunEvent) (ExecutionPolicySnapshot, error) {
	snapshot, err := executionPolicySnapshotFromEvents(prior)
	if err != nil {
		return ExecutionPolicySnapshot{}, err
	}
	if snapshot == nil {
		return ExecutionPolicySnapshot{}, media.Reject(media.DurabilityRequired, "image provider policy evidence is missing")
	}
	return *snapshot, nil
}

func verifyMediaProviderProfile(event RunEvent, payload MediaProviderAdmittedPayload, prior []RunEvent) error {
	for index := len(prior) - 1; index >= 0; index-- {
		profileEvent := prior[index]
		if profileEvent.Type != string(EventModelProfileResolved) || profileEvent.RunID != event.RunID || profileEvent.BranchID != event.BranchID {
			continue
		}
		var profile modelprofile.TelemetryProjection
		if err := json.Unmarshal(profileEvent.Payload, &profile); err != nil {
			return err
		}
		if profile.InvocationID != payload.ProviderInvocationID {
			continue
		}
		image := profile.Capabilities["image_input"]
		if !mediaProfileMatchesTarget(profile.ModelID, payload.Target) || profile.Provider != payload.ProviderKey || profile.Effective.Value != int(payload.ContextWindow) ||
			image.State != modelprofile.CapabilityYes || !trustedImageCapabilitySource(string(image.Source)) {
			return media.Reject(media.Unsupported, "image provider admission differs from its bound profile evidence")
		}
		return nil
	}
	return media.Reject(media.DurabilityRequired, "image provider admission lacks its preceding profile evidence")
}

// Built-in Ollama profiling intentionally retains the provider's model leaf.
// Named backends require the complete selector so no namespace can fall back
// to the default provider. Provider/configuration identity is checked separately.
func mediaProfileMatchesTarget(modelID string, target execution.ExecutionTarget) bool {
	if execution.IsOllamaBackend(target.Backend) && modelID == target.Model {
		return true
	}
	selector, err := execution.ParseExecutionSelector(modelID)
	return err == nil && selector.Backend == target.Backend && selector.Model == target.Model
}

func verifyMediaProviderInvocationUniqueness(event RunEvent, payload MediaProviderAdmittedPayload, prior []RunEvent) error {
	for _, previous := range prior {
		if previous.Type != string(EventMediaProviderAdmitted) {
			continue
		}
		admitted, err := decodeMediaProviderEvent(previous)
		if err != nil {
			return err
		}
		if admitted.ProviderInvocationID == payload.ProviderInvocationID && (previous.RunID != event.RunID || previous.TaskID != event.TaskID || admitted != payload) {
			return media.Reject(media.BindingConflict, "image provider invocation identity was reused")
		}
		if previous.TaskID == event.TaskID && admitted.ModelFanout.ModelExecutionID == payload.ModelFanout.ModelExecutionID && admitted.OccurrenceRevision == payload.OccurrenceRevision &&
			(admitted.DispatchID != payload.DispatchID || admitted.Attempt != payload.Attempt) {
			return media.Reject(media.AccessDenied, "image provider admission changed an active occurrence lease")
		}
		if previous.RunID == event.RunID && previous.TaskID == event.TaskID && admitted.ModelFanout.present() && payload.ModelFanout.present() &&
			admitted.ModelFanout.ParentRevision == payload.ModelFanout.ParentRevision &&
			(admitted.ModelFanout.ParentAttempt != payload.ModelFanout.ParentAttempt || admitted.ModelFanout.ParentDispatchID != payload.ModelFanout.ParentDispatchID) {
			return media.Reject(media.AccessDenied, "image leaves disagree on their canonical parent lease")
		}
	}
	return nil
}

func verifyMediaProviderBackend(policy ExecutionPolicySnapshot, payload MediaProviderAdmittedPayload) error {
	for _, backend := range policy.Backends {
		if backend.Backend == payload.Target.Backend && backend.Kind == string(execution.BackendKindLLM) &&
			backend.ProviderKey == payload.ProviderKey && backend.IdentityHash == payload.ProviderConfigurationHash {
			return nil
		}
	}
	return media.Reject(media.EgressDenied, "image provider admission has no matching frozen LLM backend")
}
