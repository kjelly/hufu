package team

import (
	"context"
	"encoding/json/v2"
	"errors"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/media"
)

// MediaProviderAdmittedPayload records permission and conservative estimates,
// not execution completion or reported provider usage. Canonical bindings
// already own the ordered references, so no image bytes or paths are copied.
type MediaProviderAdmittedPayload struct {
	ModelFanout               MediaProviderFanoutGrant  `json:"model_fanout,omitzero"`
	Version                   int                       `json:"version"`
	ProviderInvocationID      string                    `json:"provider_invocation_id"`
	OccurrenceRevision        int                       `json:"occurrence_revision"`
	Attempt                   int                       `json:"attempt"`
	DispatchID                string                    `json:"dispatch_id"`
	BindingHash               string                    `json:"binding_hash"`
	MediaInputDigest          string                    `json:"media_input_digest"`
	PolicyHash                string                    `json:"policy_hash"`
	Target                    execution.ExecutionTarget `json:"target"`
	ProviderKey               string                    `json:"provider_key"`
	ProviderConfigurationHash string                    `json:"provider_configuration_hash"`
	ImageCount                int                       `json:"image_count"`
	ImageTokenUpperBound      int64                     `json:"image_token_upper_bound"`
	ImageTokens               int64                     `json:"image_tokens"`
	TextTokens                int64                     `json:"text_tokens"`
	RequestTokens             int64                     `json:"request_tokens"`
	ContextWindow             int64                     `json:"context_window"`
	OutputReservation         int64                     `json:"output_reservation"`
	SafetyMargin              int64                     `json:"safety_margin"`
	EstimateSource            string                    `json:"estimate_source"`
}

const mediaOperatorEstimate = "operator_upper_bound"

func (c *Coordinator) mediaProviderAdmissionPayload(ctx context.Context, request agent.ProviderRequest) (nativeProviderMedia, MediaProviderAdmittedPayload, error) {
	admission, err := c.resolveNativeProviderMedia(ctx, request)
	if err != nil {
		return admission, MediaProviderAdmittedPayload{}, err
	}
	tokens, err := defaultCounter.CountProviderRequest(withNativeProviderCounter(ctx, c), request.ModelID, request)
	if err != nil {
		return admission, MediaProviderAdmittedPayload{}, err
	}
	payload := MediaProviderAdmittedPayload{Version: media.Version, ProviderInvocationID: request.InvocationID,
		OccurrenceRevision: admission.Identity.OccurrenceRevision, Attempt: admission.Identity.Attempt, DispatchID: admission.Identity.DispatchID,
		BindingHash: admission.Binding.Hash, MediaInputDigest: admission.Binding.InputDigest, PolicyHash: admission.Binding.PolicyHash,
		Target: admission.Target, ProviderKey: admission.Grant.ProviderKey, ProviderConfigurationHash: admission.Grant.IdentityHash,
		ImageCount: len(admission.Locations), ImageTokenUpperBound: admission.Grant.ImageTokenUpperBound, ImageTokens: admission.ImageTokens,
		TextTokens: int64(tokens) - admission.ImageTokens, RequestTokens: int64(tokens), ContextWindow: int64(request.AdmissionContext.ContextWindow),
		OutputReservation: int64(providerRequestOutputReservation(request)), SafetyMargin: int64(request.AdmissionContext.SafetyMarginTokens), EstimateSource: mediaOperatorEstimate,
		ModelFanout: admission.ModelFanout}
	return admission, payload, validateMediaProviderEstimate(payload)
}

func decodeMediaProviderEvent(event RunEvent) (MediaProviderAdmittedPayload, error) {
	var payload MediaProviderAdmittedPayload
	if event.SchemaVersion < eventStoreSchemaVersion || len(event.Payload) > 16<<10 {
		return payload, media.Reject(media.BindingConflict, "invalid image provider admission envelope")
	}
	if err := json.Unmarshal(event.Payload, &payload, json.RejectUnknownMembers(true)); err != nil {
		return payload, media.Reject(media.BindingConflict, "invalid image provider admission payload")
	}
	if payload.Version != media.Version || event.RunID == "" || event.BranchID == "" || event.TaskID == "" || event.Actor == "" ||
		payload.ProviderInvocationID == "" || len(payload.ProviderInvocationID) > 256 || payload.OccurrenceRevision <= 0 || payload.Attempt <= 0 || payload.DispatchID == "" ||
		payload.BindingHash == "" || payload.MediaInputDigest == "" || payload.PolicyHash == "" || payload.ProviderConfigurationHash == "" {
		return payload, media.Reject(media.BindingConflict, "image provider admission identity is incomplete")
	}
	if err := payload.Target.Validate(); err != nil {
		return payload, media.Reject(media.EgressDenied, "image provider admission target is invalid")
	}
	if payload.ModelFanout.present() {
		if err := payload.ModelFanout.validate(); err != nil {
			return payload, err
		}
	}
	if err := validateMediaProviderEstimate(payload); err != nil {
		return payload, err
	}
	return payload, nil
}

func validateMediaProviderEstimate(payload MediaProviderAdmittedPayload) error {
	if payload.ImageCount <= 0 || payload.ImageCount > media.MaxCount || payload.ImageTokenUpperBound <= 0 ||
		payload.ImageTokens <= 0 || payload.TextTokens < 0 || payload.RequestTokens < payload.ImageTokens ||
		payload.TextTokens != payload.RequestTokens-payload.ImageTokens || payload.EstimateSource != mediaOperatorEstimate {
		return media.Reject(media.BindingConflict, "invalid image provider token estimate")
	}
	if payload.ImageTokens/int64(payload.ImageCount) != payload.ImageTokenUpperBound || payload.ImageTokens%int64(payload.ImageCount) != 0 {
		return media.Reject(media.BindingConflict, "image provider estimate lost repeated occurrences")
	}
	if payload.ContextWindow <= 0 || payload.OutputReservation < 0 || payload.SafetyMargin < 0 || payload.OutputReservation > payload.ContextWindow ||
		payload.SafetyMargin > payload.ContextWindow-payload.OutputReservation || payload.RequestTokens > payload.ContextWindow-payload.OutputReservation-payload.SafetyMargin {
		return media.Reject(media.LimitExceeded, "image provider admission exceeds its context capacity")
	}
	return nil
}

func (c *Coordinator) commitMediaProviderAdmission(ctx context.Context, request agent.ProviderRequest) error {
	if request.InvocationID == "" {
		return media.Reject(media.DurabilityRequired, "image provider invocation identity is unavailable")
	}
	journal, ok := c.EventJournal().(AtomicEventJournal)
	if !ok {
		return media.Reject(media.DurabilityRequired, "image provider admission requires an atomic event journal")
	}
	for range 16 {
		admission, payload, err := c.mediaProviderAdmissionPayload(ctx, request)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		view, err := c.mediaLineage(ctx)
		if err != nil {
			return err
		}
		event := RunEvent{Type: string(EventMediaProviderAdmitted), RunID: admission.Identity.RunID, BranchID: view.branch, TaskID: admission.Identity.TaskID, Actor: admission.Identity.Agent,
			IdempotencyKey: "media-provider:" + view.branch + ":" + request.InvocationID, Payload: encoded}
		if err := verifyMediaProviderAdmission(event, payload, view.events); err != nil {
			return err
		}
		_, err = journal.AppendBatchAtHead(ctx, []RunEvent{event}, view.head)
		if errors.Is(err, ErrEventHeadChanged) {
			continue
		}
		return err
	}
	return ErrEventHeadChanged
}
