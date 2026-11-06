package team

import (
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"strings"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/cost"
	internalmcp "github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/utils"
)

// ToolMediaPublishedPayload binds a completed, authorized tool call to its
// immutable CAS snapshot. A reference or blob's first writer grants no access.
type ToolMediaPublishedPayload struct {
	Version            int                      `json:"version"`
	Hash               string                   `json:"hash"`
	TaskID             string                   `json:"task_id"`
	Agent              string                   `json:"agent"`
	OccurrenceRevision int                      `json:"occurrence_revision"`
	Attempt            int                      `json:"attempt"`
	DispatchID         string                   `json:"dispatch_id"`
	ToolCallID         string                   `json:"tool_call_id"`
	ToolInvocationID   string                   `json:"tool_invocation_id"`
	ActionInvocationID string                   `json:"action_invocation_id,omitempty"`
	ToolName           string                   `json:"tool_name"`
	IsError            bool                     `json:"is_error"`
	DescriptorSHA256   string                   `json:"descriptor_sha256"`
	ModelFanout        MediaProviderFanoutGrant `json:"model_fanout,omitzero"`
	Snapshot           media.InputSnapshot      `json:"snapshot"`
}

func (p ToolMediaPublishedPayload) digest() (string, error) {
	p.Hash = ""
	return media.Digest(p)
}

func decodeToolMediaPublication(event RunEvent) (ToolMediaPublishedPayload, error) {
	var p ToolMediaPublishedPayload
	if event.SchemaVersion < eventStoreSchemaVersion || len(event.Payload) > 512<<10 {
		return p, media.Reject(media.BindingConflict, "invalid tool media publication envelope")
	}
	if err := json.Unmarshal(event.Payload, &p, json.RejectUnknownMembers(true)); err != nil {
		return p, media.Reject(media.BindingConflict, "invalid tool media publication")
	}
	_, descriptorErr := hex.DecodeString(p.DescriptorSHA256)
	actor := p.Agent
	if p.ActionInvocationID != "" && actor == "" {
		actor = "runtime"
	}
	if p.Version != media.Version || p.IsError || event.TaskID == "" || event.Actor == "" || p.TaskID != event.TaskID || actor != event.Actor || p.OccurrenceRevision < 1 || p.Attempt < 1 ||
		len(p.ActionInvocationID) > 256 || strings.ContainsAny(p.ActionInvocationID, "\x00\r\n") ||
		p.DispatchID == "" || len(p.DispatchID) > 256 || p.ToolCallID == "" || len(p.ToolCallID) > 256 || strings.ContainsAny(p.ToolCallID, "\x00\r\n") ||
		p.ToolInvocationID == "" || len(p.ToolInvocationID) > 256 || strings.ContainsAny(p.ToolInvocationID, "\x00\r\n") ||
		p.ToolName == "" || len(p.ToolName) > 256 || descriptorErr != nil || len(p.DescriptorSHA256) != 64 || strings.ToLower(p.DescriptorSHA256) != p.DescriptorSHA256 ||
		p.Snapshot.RunID != event.RunID || p.Snapshot.BranchID != event.BranchID || p.Snapshot.InvocationID != event.InvocationID || len(p.Snapshot.Occurrences) == 0 {
		return p, media.Reject(media.BindingConflict, "incomplete tool media publication identity")
	}
	if err := p.Snapshot.Validate(media.Limits{}); err != nil {
		return p, err
	}
	for _, occurrence := range p.Snapshot.Occurrences {
		if occurrence.Origin != "mcp_tool" || occurrence.ToolCallID != p.ToolCallID {
			return p, media.Reject(media.AccessDenied, "tool media source origin differs from its publisher")
		}
	}
	hash, err := p.digest()
	if err != nil || hash != p.Hash {
		return p, media.Reject(media.IntegrityMismatch, "tool media publication hash mismatch")
	}
	return p, nil
}

func (c *Coordinator) toolMediaOccurrence(ctx context.Context) (submitResultRuntimeIdentity, *TodoItem, error) {
	identity, ok := ctx.Value(submitResultRuntimeIdentityKey{}).(submitResultRuntimeIdentity)
	item := c.todoItemByID(identity.TaskID)
	active, opened := c.activeTaskResultOccurrence(identity.TaskID)
	if !ok || !opened || !validSubmitResultIdentity(identity) || identity.RunID != c.executionRunID || item == nil || item.Status != TaskInProgress ||
		!sameTaskResultOccurrence(active, identity) || identity.OccurrenceRevision != item.OccurrenceRevision || identity.DispatchID != item.DispatchID || currentResultCollection(item) != nil || protocolRepairExecution(ctx) {
		return identity, nil, media.Reject(media.AccessDenied, "tool image publisher has no current worker lease")
	}
	purpose, err := c.providerCostIdentity(ctx, "tool-media-publication")
	if err != nil || !nativeProviderPurposeMatches(purpose, identity) || purpose.Purpose != cost.PurposeWorker && purpose.Purpose != cost.PurposeDirectAgent {
		return identity, nil, media.Reject(media.AccessDenied, "tool image publisher is outside worker execution")
	}
	if err := c.validateMediaAttempt(ctx, taskDefFromTodoItem(item), item.ID); err != nil {
		return identity, nil, err
	}
	return identity, item, nil
}

// publishToolMediaResult is called only by the trusted post-authorization MCP
// hook. Text-only results retain their existing response/error representation.
func (c *Coordinator) publishToolMediaResult(ctx context.Context, tool internalmcp.MCPTool, call fantasy.ToolCall, result internalmcp.RuntimeToolResult) (fantasy.ToolResponse, error) {
	var texts []string
	var imageCount int
	hasImage := false
	for _, block := range result.Content {
		switch block.Type {
		case "text":
			texts = append(texts, utils.RedactSecrets(block.Text))
		case "image":
			hasImage = true
			imageCount++
		default:
			return fantasy.ToolResponse{}, media.Reject(media.Unsupported, "unsupported MCP tool content")
		}
	}
	text := strings.Join(texts, "\n")
	if !hasImage {
		if result.IsError {
			return fantasy.NewTextErrorResponse(text), nil
		}
		return fantasy.NewTextResponse(text), nil
	}
	if result.IsError {
		return fantasy.ToolResponse{}, media.Reject(media.Unsupported, "error tool result cannot authorize image continuation")
	}
	if result.InvocationID == "" || len(result.InvocationID) > 256 {
		return fantasy.ToolResponse{}, media.Reject(media.AccessDenied, "tool image publisher has no client-owned invocation identity")
	}
	identity, item, err := c.mediaPublicationOccurrence(ctx)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	policy, err := c.requiredMediaPolicy()
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	limits, err := policy.Policy.Image.Resolve()
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if len(result.Content) > 128 || len(text) > 256<<10 || imageCount > limits.MaxCount {
		return fantasy.ToolResponse{}, media.Reject(media.LimitExceeded, "MCP tool media exceeds its content budget")
	}
	descriptor, err := internalmcp.MCPToolDescriptorSHA256(tool)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	view, err := c.mediaLineage(ctx)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	_, fanout, err := c.nativeMediaProviderTarget(ctx, item, identity, view.events)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	p := ToolMediaPublishedPayload{Version: media.Version, TaskID: identity.TaskID, Agent: identity.Agent, OccurrenceRevision: identity.OccurrenceRevision, Attempt: identity.Attempt, DispatchID: identity.DispatchID,
		ToolCallID: call.ID, ToolInvocationID: result.InvocationID, ToolName: tool.Name, DescriptorSHA256: descriptor, ModelFanout: fanout}
	if action, ok := ctx.Value(mcpActionMediaGrantKey{}).(mcpActionMediaGrant); ok {
		p.ActionInvocationID = action.environment.ActionInvocationID
	}
	turn, err := media.Digest([]any{identity.RunID, identity.TaskID, identity.OccurrenceRevision, identity.Attempt, identity.DispatchID, fanout, call.ID, descriptor, result.InvocationID})
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	owner := c.canonicalJournalOwner()
	p.Snapshot = media.InputSnapshot{RunID: identity.RunID, BranchID: view.branch, InvocationID: owner.currentInvocationID(), TurnID: "mcp-" + strings.TrimPrefix(turn, "sha256:"), PolicyHash: policy.Hash}
	var ordered []media.Ref
	seen := make(map[string]bool)
	for _, block := range result.Content {
		if block.Type == "text" {
			p.Snapshot.Input.Parts = append(p.Snapshot.Input.Parts, media.Part{Type: "text", Text: utils.RedactSecrets(block.Text)})
			continue
		}
		stage, err := media.StageBase64Image(ctx, block.Data, block.MIMEType, policy.Policy.Image)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		var ref media.Ref
		err = stage.WithBytes(ctx, func(data []byte, expected media.Ref) error {
			var publishErr error
			ref, publishErr = c.publishMediaTurnImage(ctx, c.artifactStoreRootPath(), PutArtifactRequest{Kind: "image", Content: data, ID: expected.ID, MediaType: expected.MediaType, MediaLimits: policy.Policy.Image})
			return publishErr
		})
		stage.Close()
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		ordered = append(ordered, ref)
		if err := media.CheckBundle(ordered, policy.Policy.Image); err != nil {
			return fantasy.ToolResponse{}, err
		}
		if !seen[ref.ID] {
			seen[ref.ID] = true
			p.Snapshot.Refs = append(p.Snapshot.Refs, ref)
		}
		p.Snapshot.Occurrences = append(p.Snapshot.Occurrences, media.Occurrence{RunID: p.Snapshot.RunID, BranchID: p.Snapshot.BranchID, InvocationID: p.Snapshot.InvocationID,
			TurnID: p.Snapshot.TurnID, PartIndex: len(p.Snapshot.Input.Parts), MediaID: ref.ID, Origin: "mcp_tool", ToolCallID: call.ID})
		p.Snapshot.Input.Parts = append(p.Snapshot.Input.Parts, media.Part{Type: "image", MediaID: ref.ID})
	}
	p.Snapshot, err = media.SealSnapshot(p.Snapshot, policy.Policy.Image)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	p.Hash, err = p.digest()
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if err := c.commitToolMediaPublication(ctx, identity, p); err != nil {
		return fantasy.ToolResponse{}, err
	}
	carrier := toolMediaResultFromPublication(p)
	metadata, err := agent.EncodeToolMediaMetadata(carrier)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	response := fantasy.NewTextResponse(text)
	response.Metadata = metadata
	return response, nil
}

func (c *Coordinator) commitToolMediaPublication(ctx context.Context, identity submitResultRuntimeIdentity, p ToolMediaPublishedPayload) error {
	journal, ok := c.EventJournal().(AtomicEventJournal)
	if !ok {
		return media.Reject(media.DurabilityRequired, "tool media requires an atomic journal")
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		return err
	}
	snapshot, err := json.Marshal(p.Snapshot)
	if err != nil {
		return err
	}
	slot, err := acquireSem(ctx, c.mediaInputSemaphore())
	if err != nil {
		return err
	}
	defer slot.release()
	for range 16 {
		current, _, err := c.mediaPublicationOccurrence(ctx)
		if err != nil {
			return err
		}
		if current != identity {
			return media.Reject(media.AccessDenied, "tool media owner changed during publication")
		}
		view, err := c.mediaLineage(ctx)
		if err != nil {
			return err
		}
		if view.branch != p.Snapshot.BranchID || view.policyHash != p.Snapshot.PolicyHash {
			return media.Reject(media.AccessDenied, "tool media policy or branch changed")
		}
		if err := checkMediaBranchQuota(view.snapshots, p.Snapshot, c.executionPolicy.snapshot.Media.Policy.Image); err != nil {
			return err
		}
		input := RunEvent{SchemaVersion: eventStoreSchemaVersion, Type: string(EventMediaInputAdmitted), RunID: identity.RunID, BranchID: view.branch, InvocationID: p.Snapshot.InvocationID, Actor: "runtime",
			IdempotencyKey: "media-input:" + view.branch + ":" + p.Snapshot.ID, Payload: snapshot}
		producer := RunEvent{SchemaVersion: eventStoreSchemaVersion, Type: string(EventToolMediaPublished), RunID: identity.RunID, BranchID: view.branch, InvocationID: p.Snapshot.InvocationID, TaskID: identity.TaskID, Actor: identity.Agent,
			IdempotencyKey: "tool-media:" + view.branch + ":" + p.Snapshot.TurnID, Payload: encoded}
		if p.ActionInvocationID != "" && producer.Actor == "" {
			producer.Actor = "runtime"
		}
		if err := verifyToolMediaPublication(producer, p, append(view.events, input)); err != nil {
			return err
		}
		_, err = journal.AppendBatchAtHead(ctx, []RunEvent{input, producer}, view.head)
		if errors.Is(err, ErrEventHeadChanged) {
			continue
		}
		if err != nil {
			return err
		}
		owner := c.canonicalJournalOwner()
		return owner.mutateSessionData(func(sd *SessionData) error {
			sd.MediaInputSnapshots = appendMediaInputSnapshot(cloneMediaInputSnapshots(view.inputs), p.Snapshot)
			return nil
		})
	}
	return ErrEventHeadChanged
}
