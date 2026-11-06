package team

import (
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"errors"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/media"
)

// Comparable and omitted from older admissions. This evidence describes the
// verified adapter, not an invented LLM capability profile or measured usage.
type CodexImageAdmission struct {
	Version        string `json:"version,omitempty"`
	BinaryHash     string `json:"binary_hash,omitempty"`
	ProjectionHash string `json:"projection_hash,omitempty"`
	ContextSource  string `json:"context_source,omitempty"`
}

type externalCostReservationKey struct{}

type codexImageAttempt struct {
	projection *codexImageProjection
	payload    MediaProviderAdmittedPayload
}

func (p *CodexSubagentProvider) prepareImageAttempt(ctx context.Context, request AttemptRequest, environment []string) (*codexImageAttempt, error) {
	if !taskHasMedia(request.Task) {
		return nil, nil
	}
	if protocolRepairExecution(ctx) {
		return nil, nil
	}
	c := p.coordinator
	if err := c.validateCodexMediaAttempt(ctx, request); err != nil {
		return nil, err
	}
	item := c.todoItemByID(request.TaskID)
	messages, err := c.nativeWorkerMessages(ctx, request.TaskID, request.Prompt, request.History)
	if err != nil {
		return nil, err
	}
	policy, err := c.requiredMediaPolicy()
	if err != nil {
		return nil, err
	}
	frozen, ok := c.executionPolicy.backendByName[p.name]
	if !ok || frozen.Kind != string(execution.BackendKindAgent) || frozen.IdentityHash != executionPolicyCodexIdentityHash(p.name, p.config) || request.ExecutionTarget.Backend != p.name || request.ExecutionTarget != item.ExecutionTarget {
		return nil, media.Reject(media.EgressDenied, "Codex image destination differs from frozen backend")
	}
	grant, err := policy.Policy.Allow(p.name, "", request.ExecutionTarget.Model)
	if err != nil {
		return nil, err
	}
	if grant.ImageTokenUpperBound <= 0 {
		return nil, media.Reject(media.Unsupported, "Codex image admission requires a positive operator image token bound")
	}
	if grant.IdentityHash != frozen.IdentityHash {
		return nil, media.Reject(media.EgressDenied, "Codex media grant changed")
	}
	if err := validateNativeProviderReferences(messages, historyReferences(messages), item.MediaBinding, policy.Policy.Image); err != nil {
		return nil, err
	}
	binary, err := codexImageBinaryIdentity(ctx, p.config.Command, environment)
	if err != nil {
		return nil, err
	}
	projectionHash, err := codexImageProjectionHash(item.MediaBinding, request.ExecutionTarget.String())
	if err != nil {
		return nil, err
	}
	spec := c.codexImageContextSpec(ctx, request)
	if spec.ContextWindowSource != "operator" || spec.ContextWindow <= 0 {
		return nil, media.Reject(media.Unsupported, "Codex image admission requires explicit --context-window")
	}
	schema, err := json.Marshal(struct {
		DeveloperInstructions string           `json:"developer_instructions"`
		Input                 []codexUserInput `json:"input"`
		OutputSchema          any              `json:"output_schema"`
	}{codexWorkerDeveloperInstructions, codexImageInputs(codexImageProjectionPath(c.artifactStoreRootPath(), projectionHash), request.Prompt, item.MediaBinding.Refs), codexWorkerResultProposalSchemaForProjection(projectionForContract(request.resultContractSchema))})
	if err != nil {
		return nil, err
	}
	textTokens, err := defaultCounter.CountText(ctx, request.ExecutionTarget.String(), string(schema))
	if err != nil {
		return nil, err
	}
	identity, _, err := c.nativeProviderOccurrence(ctx)
	if err != nil {
		return nil, err
	}
	invocationID := ""
	if reservation, ok := ctx.Value(externalCostReservationKey{}).(AgentExecutionCostReservation); ok && reservation.RunID == request.RunID {
		invocationID = reservation.ProviderInvocationID
	}
	if c.costManager != nil && invocationID == "" {
		return nil, media.Reject(media.AccessDenied, "Codex image attempt lacks its opaque cost reservation")
	}
	if invocationID == "" {
		invocationID, err = agent.NewProviderInvocationID()
		if err != nil {
			return nil, err
		}
	}
	imageTokens := int64(len(item.MediaBinding.Refs)) * grant.ImageTokenUpperBound
	payload := MediaProviderAdmittedPayload{Version: media.Version, ProviderInvocationID: invocationID, OccurrenceRevision: identity.OccurrenceRevision, Attempt: identity.Attempt, DispatchID: identity.DispatchID,
		BindingHash: item.MediaBinding.Hash, MediaInputDigest: item.MediaInputDigest, PolicyHash: policy.Hash, Target: request.ExecutionTarget, ProviderConfigurationHash: frozen.IdentityHash,
		ImageCount: len(item.MediaBinding.Refs), ImageTokenUpperBound: grant.ImageTokenUpperBound, ImageTokens: imageTokens, TextTokens: int64(textTokens), RequestTokens: int64(textTokens) + imageTokens,
		ContextWindow: int64(spec.ContextWindow), OutputReservation: int64(spec.MaxOutputTokens), SafetyMargin: int64(spec.SafetyMarginTokens), EstimateSource: mediaOperatorEstimate,
		Codex: CodexImageAdmission{Version: codexImageProtocolVersion, BinaryHash: binary, ProjectionHash: projectionHash, ContextSource: spec.ContextWindowSource}}
	if err := validateMediaProviderEstimate(payload); err != nil {
		return nil, err
	}
	projection, err := newCodexImageProjection(ctx, c, request, projectionHash)
	if err != nil {
		return nil, err
	}
	return &codexImageAttempt{projection: projection, payload: payload}, nil
}

func (p *CodexSubagentProvider) admitImageTurn(ctx context.Context, request AttemptRequest, input *codexImageAttempt) error {
	if input == nil {
		return nil
	}
	c := p.coordinator
	journal, ok := c.EventJournal().(AtomicEventJournal)
	if !ok {
		return media.Reject(media.DurabilityRequired, "Codex images require atomic journal")
	}
	for range 16 {
		if err := c.validateCodexMediaAttempt(ctx, request); err != nil {
			return err
		}
		current, err := codexImageBinaryHashForCommand(p.config.Command)
		if err != nil {
			return err
		}
		if executionPolicyCodexIdentityHash(p.name, p.config) != input.payload.ProviderConfigurationHash {
			return media.Reject(media.EgressDenied, "Codex launch policy changed before image turn")
		}
		if current != input.payload.Codex.BinaryHash {
			return media.Reject(media.BindingConflict, "Codex executable changed before image turn")
		}
		policy, err := c.requiredMediaPolicy()
		if err != nil {
			return err
		}
		if err := input.projection.Verify(ctx, policy.Policy.Image); err != nil {
			return err
		}
		view, err := c.mediaLineage(ctx)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(input.payload)
		if err != nil {
			return err
		}
		event := RunEvent{SchemaVersion: eventStoreSchemaVersion, Type: string(EventMediaProviderAdmitted), RunID: request.RunID, BranchID: view.branch, TaskID: request.TaskID, Actor: request.Task.Agent,
			IdempotencyKey: "media-provider:" + view.branch + ":" + input.payload.ProviderInvocationID, Payload: encoded}
		if err := verifyMediaProviderAdmission(event, input.payload, view.events); err != nil {
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

func verifyCodexImageProfile(payload MediaProviderAdmittedPayload, item *TodoItem, prior []RunEvent) error {
	proof := payload.Codex
	digest, err := hex.DecodeString(proof.BinaryHash)
	if err != nil || len(digest) != 32 || hex.EncodeToString(digest) != proof.BinaryHash || proof.Version != codexImageProtocolVersion || proof.ContextSource != "operator" || payload.ProviderKey != "" || payload.ConsumerHash != "" || payload.ModelFanout.present() {
		return media.Reject(media.Unsupported, "Codex image adapter proof is unsupported")
	}
	expected, err := codexImageProjectionHash(item.MediaBinding, payload.Target.String())
	if err != nil || expected != proof.ProjectionHash {
		return media.Reject(media.BindingConflict, "Codex image projection identity differs from binding")
	}
	for _, event := range prior {
		if event.Type != string(EventMediaProviderAdmitted) || event.TaskID != item.ID {
			continue
		}
		previous, err := decodeMediaProviderEvent(event)
		if err != nil {
			return err
		}
		if previous.BindingHash == payload.BindingHash && previous.Codex != (CodexImageAdmission{}) && previous.Codex != proof {
			return media.Reject(media.BindingConflict, "Codex image adapter changed across resume")
		}
	}
	return nil
}

// Reject raw or unbound history before preflight/process/world side effects.
// Text-only callers keep their existing protocol and do not need image grants.
func (c *Coordinator) validateCodexMediaAttempt(ctx context.Context, request AttemptRequest) error {
	if _, err := agent.ValidatePersistentMessages(request.History); err != nil {
		return err
	}
	item := c.todoItemByID(request.TaskID)
	hasMedia := taskHasMedia(request.Task) || item != nil && (item.MediaBinding != nil || item.MediaInputDigest != "")
	if !hasMedia && (len(historyReferences(request.History)) != 0 || agent.MessagesHaveToolMedia(request.History)) {
		return media.Reject(media.AccessDenied, "Codex history has no canonical image binding")
	}
	return c.validateLocalMediaAttempt(ctx, request)
}

// CLI capacity registered for the exact target takes precedence. Otherwise
// use the same agent-first/team-fallback operator declaration as local work,
// without inventing an LLM profile for an external agent backend.
func (c *Coordinator) codexImageContextSpec(ctx context.Context, request AttemptRequest) ModelContextSpec {
	spec := c.modelContextSpecForInvocation(ctx, request.ExecutionTarget.String(), request.Agent)
	if spec.ContextWindowSource == "operator" {
		return spec
	}
	window := c.session.Config.Generation.ContextWindow
	if request.Agent != nil && request.Agent.Generation.ContextWindow > 0 {
		window = request.Agent.Generation.ContextWindow
	}
	if window > 0 {
		spec.ContextWindow, spec.ContextWindowSource, spec.IsEstimated = window, "operator", false
	}
	return spec
}

// A direct provider caller may retain an in-memory lease after another journal
// writer revoked it. Check the canonical current occurrence before reading CAS
// and again before launching the external process.
func (c *Coordinator) validateCodexImageLease(ctx context.Context, request AttemptRequest) error {
	if err := c.validateMediaAttempt(ctx, request.Task, request.TaskID); err != nil {
		return err
	}
	view, err := c.mediaLineage(ctx)
	if err != nil {
		return err
	}
	items, err := ReplayTodoList(view.events)
	if err != nil {
		return err
	}
	live := todoItemByID(items, request.TaskID)
	local := c.todoItemByID(request.TaskID)
	if live == nil || local == nil || live.Status != TaskInProgress || live.OccurrenceRevision != local.OccurrenceRevision || live.DispatchID != local.DispatchID || live.MediaBinding == nil || live.MediaBinding.Hash != request.Task.MediaBinding.Hash {
		return media.Reject(media.AccessDenied, "Codex image lease was revoked in the canonical journal")
	}
	return nil
}

func (input *codexImageAttempt) Close() {
	if input != nil {
		input.projection.Close()
	}
}
