package team

import (
	"context"
	"encoding/json/v2"
	"reflect"
	"slices"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/modelprofile"
)

type coordinatorReferenceMetadataKey struct{}

// This grants only the coordinator's existing canonical history metadata.
// A child worker, auxiliary role or later invocation cannot inherit this view.
type coordinatorReferenceMetadata struct {
	coordinator  *Coordinator
	lease        *invocationLease
	runID        string
	invocationID string
	branch       string
	agent        string
	bound        agent.ProviderAdmissionContext
	references   []media.HistoryReference
}

func (c *Coordinator) withCoordinatorReferenceMetadata(ctx context.Context, bound agent.ProviderAdmissionContext) (context.Context, error) {
	c.conversationHistoryMu.Lock()
	history := cloneMessages(c.conversationHistory)
	c.conversationHistoryMu.Unlock()
	if !agent.MessagesHaveMedia(history) {
		return ctx, nil
	}
	if _, err := canonicalMediaHistoryProjection(c.sessionWorkspace(), history); err != nil {
		return nil, err
	}
	lease := c.activeInvocationLease()
	orch := c.GetOrchestratorDef()
	if lease == nil || orch == nil || !bound.IsBound() || bound.ModelID == "" {
		return nil, media.Reject(media.AccessDenied, "metadata view requires a live coordinator invocation and bound provider")
	}
	scope := &coordinatorReferenceMetadata{coordinator: c, lease: lease, runID: c.executionRunID,
		invocationID: c.currentInvocationID(), branch: c.activeBranchID(), agent: orch.Name, bound: bound}
	for _, location := range historyReferences(history) {
		scope.references = append(scope.references, location.Reference)
	}
	return withNativeProviderCounter(context.WithValue(ctx, coordinatorReferenceMetadataKey{}, scope), c), nil
}

var _ agent.ProviderReferenceMetadataAdmission = providerRequestAdmission{}

func (a providerRequestAdmission) AdmitProviderReferenceMetadata(ctx context.Context, request agent.ProviderRequest) (bool, error) {
	return a.c.resolveProviderReferenceMetadata(ctx, request)
}

func (c *Coordinator) resolveProviderReferenceMetadata(ctx context.Context, request agent.ProviderRequest) (bool, error) {
	scope, _ := ctx.Value(coordinatorReferenceMetadataKey{}).(*coordinatorReferenceMetadata)
	if scope == nil || !agent.ProviderRequestHasMedia(request) {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if c == nil {
		return true, media.Reject(media.AccessDenied, "metadata consumer has no runtime owner")
	}
	// Worker and auxiliary contexts retain their own native admission rules.
	identity, err := c.providerCostIdentity(ctx, "metadata-preflight")
	if err != nil {
		return true, media.Reject(media.AccessDenied, "metadata consumer run identity is unavailable")
	}
	todoID, _ := ctx.Value(todoIDKey{}).(string)
	if todoID != CoordTodoID || identity.TaskID != CoordTodoID || identity.Purpose != cost.PurposeCoordinator {
		return false, nil
	}
	if err := scope.validate(ctx, c, request, identity.Agent); err != nil {
		return true, err
	}
	messages, err := providerMediaMessages(request)
	if err != nil {
		return true, err
	}
	if _, err := agent.ValidatePersistentMessages(messages); err != nil {
		return true, err
	}
	var references []media.HistoryReference
	for _, location := range historyReferences(messages) {
		if messages[location.Message].Role != fantasy.MessageRoleUser {
			return true, media.Reject(media.AccessDenied, "metadata reference is outside its user turn")
		}
		references = append(references, location.Reference)
	}
	if !slices.Equal(scope.references, references) {
		return true, media.Reject(media.AccessDenied, "metadata references differ from the authorized ordered history")
	}
	if err := validateMediaHistoryMetadata(ctx, c.sessionWorkspace(), scope.branch, messages); err != nil {
		return true, err
	}
	return true, scope.validate(ctx, c, request, identity.Agent)
}

func (s *coordinatorReferenceMetadata) validate(ctx context.Context, c *Coordinator, request agent.ProviderRequest, agentName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c == nil || c.providerManager == nil || s.coordinator != c || s.lease != c.activeInvocationLease() || s.runID == "" || s.runID != c.executionRunID ||
		s.invocationID == "" || s.invocationID != c.currentInvocationID() || s.branch != c.activeBranchID() || s.agent != agentName ||
		request.ModelID != s.bound.ModelID || request.AdmissionContext != s.bound {
		return media.Reject(media.AccessDenied, "metadata consumer identity changed or expired")
	}
	actual, err := c.providerManager.ResolveProviderExecutionIdentity(request.ModelID)
	if err != nil || actual.ProviderKey != s.bound.ProviderIdentity || actual.ConfigurationHash != s.bound.ProviderConfigurationHash {
		return media.Reject(media.EgressDenied, "metadata provider configuration changed after resolution")
	}
	return nil
}

func (c *Coordinator) authorizeReferenceMetadataTransport(ctx context.Context, request agent.ProviderRequest) error {
	if request.InvocationID == "" {
		return media.Reject(media.DurabilityRequired, "metadata transport has no committed invocation")
	}
	var expected modelprofile.TelemetryProjection
	if err := json.Unmarshal([]byte(request.AdmissionContext.ProfileTelemetryJSON), &expected); err != nil {
		return err
	}
	expected.InvocationID = request.InvocationID
	view, err := c.mediaLineage(ctx)
	if err != nil {
		return err
	}
	for _, event := range view.events {
		if event.Type != string(EventModelProfileResolved) || event.RunID != c.executionRunID || event.BranchID != view.branch {
			continue
		}
		var actual modelprofile.TelemetryProjection
		if err := json.Unmarshal(event.Payload, &actual); err != nil {
			return err
		}
		if actual.InvocationID == request.InvocationID {
			if !reflect.DeepEqual(actual, expected) {
				return media.Reject(media.AccessDenied, "metadata transport differs from its committed profile")
			}
			metadata, err := c.resolveProviderReferenceMetadata(ctx, request)
			if err != nil {
				return err
			}
			if !metadata {
				return media.Reject(media.AccessDenied, "metadata consumer is no longer admitted")
			}
			return nil
		}
	}
	return media.Reject(media.DurabilityRequired, "metadata transport profile was not committed")
}
