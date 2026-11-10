package team

import (
	"context"
	"encoding/json/v2"
	"reflect"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
)

// RuntimeMediaSource preserves ordered output blocks separately from native
// Outputs and model-authored judgments. Every field is coordinator-owned.
type RuntimeMediaSource struct {
	PublicationHash  string                `json:"publication_hash"`
	ToolCallID       string                `json:"tool_call_id"`
	ToolInvocationID string                `json:"tool_invocation_id"`
	ToolName         string                `json:"tool_name"`
	DescriptorSHA256 string                `json:"descriptor_sha256"`
	Parts            []agent.ToolMediaPart `json:"parts"`
}

// RuntimeMediaProjection is immutable receipt-bound evidence, not an image
// read grant or an attestation that a model interpreted its pixels correctly.
type RuntimeMediaProjection struct {
	Version            int                  `json:"version"`
	Hash               string               `json:"hash"`
	RunID              string               `json:"run_id"`
	TaskID             string               `json:"task_id"`
	Attempt            int                  `json:"attempt"`
	OccurrenceRevision int                  `json:"occurrence_revision"`
	DispatchID         string               `json:"dispatch_id"`
	Agent              string               `json:"agent"`
	ModelExecutionID   string               `json:"model_execution_id,omitempty"`
	ActionInvocationID string               `json:"action_invocation_id,omitempty"`
	Sources            []RuntimeMediaSource `json:"sources"`
}

func runtimeMediaHash(result *TaskResult) string {
	if result == nil || result.RuntimeMedia == nil {
		return ""
	}
	return result.RuntimeMedia.Hash
}

func (c *Coordinator) taskRuntimeMediaHash(taskID string) string {
	if item := c.todoItemByID(taskID); item != nil {
		return runtimeMediaHash(item.TypedResult)
	}
	return ""
}

func toolMediaResultFromPublication(p ToolMediaPublishedPayload) agent.ToolMediaResult {
	result := agent.ToolMediaResult{Version: media.Version, ToolCallID: p.ToolCallID}
	for index, part := range p.Snapshot.Input.Parts {
		if part.Type == "text" {
			result.Parts = append(result.Parts, agent.ToolMediaPart{Type: "text", Text: part.Text})
			continue
		}
		for _, ref := range p.Snapshot.Refs {
			if ref.ID == part.MediaID {
				result.Parts = append(result.Parts, agent.ToolMediaPart{Type: "image", Image: media.HistoryReference{Version: media.Version,
					Source: media.Selection{SnapshotID: p.Snapshot.ID, SnapshotHash: p.Snapshot.Hash, TurnID: p.Snapshot.TurnID, PartIndex: index},
					Ref:    ref, TaskID: p.TaskID, PolicyHash: p.Snapshot.PolicyHash, BindingHash: p.Hash}})
				break
			}
		}
	}
	return result
}

func runtimeMediaSource(p ToolMediaPublishedPayload) RuntimeMediaSource {
	return RuntimeMediaSource{PublicationHash: p.Hash, ToolCallID: p.ToolCallID, ToolInvocationID: p.ToolInvocationID, ToolName: p.ToolName,
		DescriptorSHA256: p.DescriptorSHA256, Parts: toolMediaResultFromPublication(p).Parts}
}

func (p RuntimeMediaProjection) digest() (string, error) { p.Hash = ""; return media.Digest(p) }

func (p *RuntimeMediaProjection) clone() *RuntimeMediaProjection {
	if p == nil {
		return nil
	}
	clone := *p
	clone.Sources = slices.Clone(p.Sources)
	for index := range clone.Sources {
		clone.Sources[index].Parts = slices.Clone(p.Sources[index].Parts)
	}
	return &clone
}

func (p *RuntimeMediaProjection) Validate() error {
	if p == nil {
		return nil
	}
	if p.Version != media.Version || p.RunID == "" || p.TaskID == "" || p.Attempt < 1 || p.OccurrenceRevision < 1 || p.DispatchID == "" || len(p.Sources) == 0 || len(p.Sources) > media.MaxCount {
		return media.Reject(media.BindingConflict, "invalid runtime media result identity")
	}
	var refs []media.Ref
	var textBytes int
	seen := make(map[string]bool)
	for _, source := range p.Sources {
		if seen[source.PublicationHash] || source.PublicationHash == "" || source.ToolInvocationID == "" || source.ToolName == "" || len(source.DescriptorSHA256) != 64 {
			return media.Reject(media.BindingConflict, "invalid runtime media result source")
		}
		seen[source.PublicationHash] = true
		carrier := agent.ToolMediaResult{Version: media.Version, ToolCallID: source.ToolCallID, Parts: source.Parts}
		if err := carrier.Validate(); err != nil {
			return err
		}
		for _, part := range source.Parts {
			if part.Type == "text" {
				textBytes += len(part.Text)
				continue
			}
			if part.Image.BindingHash != source.PublicationHash || part.Image.TaskID != p.TaskID {
				return media.Reject(media.AccessDenied, "runtime media result borrowed a source owner")
			}
			refs = append(refs, part.Image.Ref)
		}
	}
	if textBytes > 256<<10 {
		return media.Reject(media.LimitExceeded, "runtime media result text exceeds its budget")
	}
	if err := media.CheckBundle(refs, media.Limits{}); err != nil {
		return err
	}
	hash, err := p.digest()
	if err != nil || hash != p.Hash {
		return media.Reject(media.IntegrityMismatch, "runtime media result hash mismatch")
	}
	encoded, err := json.Marshal(p)
	if err != nil || len(encoded) > 512<<10 {
		return media.Reject(media.LimitExceeded, "runtime media result exceeds its metadata budget")
	}
	return nil
}

func (c *Coordinator) runtimeMediaForOccurrence(ctx context.Context, identity submitResultRuntimeIdentity, actionID string) (*RuntimeMediaProjection, error) {
	if c.executionPolicy == nil || c.executionPolicy.snapshot == nil || c.executionPolicy.snapshot.Media == nil || !c.hasDurableEventJournal() {
		return nil, nil
	}
	view, err := c.mediaLineage(ctx)
	if err != nil {
		return nil, err
	}
	return runtimeMediaFromLineage(view.events, identity, c.modelExecutionID, actionID)
}

func runtimeMediaFromLineage(events []RunEvent, identity submitResultRuntimeIdentity, modelExecutionID, actionID string) (*RuntimeMediaProjection, error) {
	p := &RuntimeMediaProjection{Version: media.Version, RunID: identity.RunID, TaskID: identity.TaskID, Attempt: identity.Attempt,
		OccurrenceRevision: identity.OccurrenceRevision, DispatchID: identity.DispatchID, Agent: identity.Agent, ModelExecutionID: modelExecutionID, ActionInvocationID: actionID}
	for index, event := range events {
		if event.Type != string(EventToolMediaPublished) || event.RunID != identity.RunID || event.TaskID != identity.TaskID {
			continue
		}
		publication, err := decodeToolMediaPublication(event)
		if err != nil {
			return nil, err
		}
		if publication.Attempt != identity.Attempt || publication.OccurrenceRevision != identity.OccurrenceRevision || publication.DispatchID != identity.DispatchID ||
			!strings.EqualFold(publication.Agent, identity.Agent) || publication.ActionInvocationID != actionID || publication.ModelFanout.ModelExecutionID != modelExecutionID {
			continue
		}
		if err := verifyToolMediaPublication(event, publication, events[:index]); err != nil {
			return nil, err
		}
		p.Sources = append(p.Sources, runtimeMediaSource(publication))
	}
	if len(p.Sources) == 0 {
		return nil, nil
	}
	var err error
	p.Hash, err = p.digest()
	if err != nil {
		return nil, err
	}
	return p, p.Validate()
}

func verifyRuntimeMediaResult(result *TaskResult, receipt *ExecutionReceipt, prior []RunEvent) error {
	if result == nil || result.RuntimeMedia == nil {
		if receipt != nil && receipt.RuntimeMediaHash != "" {
			return media.Reject(media.BindingConflict, "receipt media binding has no canonical result")
		}
		return nil
	}
	p := result.RuntimeMedia
	if err := p.Validate(); err != nil {
		return err
	}
	if receipt == nil || receipt.RuntimeMediaHash != p.Hash || receipt.RunID != p.RunID || receipt.TaskID != p.TaskID || receipt.Attempt != p.Attempt || result.TaskID != p.TaskID ||
		result.Attempt != p.Attempt || !strings.EqualFold(result.Agent, p.Agent) || p.ModelExecutionID != "" && p.ModelExecutionID != receipt.ModelExecutionID ||
		p.ActionInvocationID != receipt.ActionInvocationID {
		return media.Reject(media.AccessDenied, "runtime media result differs from its canonical receipt")
	}
	identity := submitResultRuntimeIdentity{RunID: p.RunID, TaskID: p.TaskID, Attempt: p.Attempt, Agent: p.Agent, OccurrenceRevision: p.OccurrenceRevision, DispatchID: p.DispatchID}
	expected, err := runtimeMediaFromLineage(prior, identity, p.ModelExecutionID, p.ActionInvocationID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(p, expected) {
		return media.Reject(media.IntegrityMismatch, "runtime media result differs from complete canonical publications")
	}
	return nil
}

func verifyRuntimeMediaTransition(event RunEvent, prior []RunEvent) error {
	if !strings.HasPrefix(event.Type, "task_") {
		return nil
	}
	var payload struct {
		Result   *TaskResult        `json:"typed_result"`
		Receipt  *ExecutionReceipt  `json:"execution_receipt"`
		Receipts []ExecutionReceipt `json:"execution_receipts"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return err
	}
	if event.Type == string(EventTaskCompleted) {
		receipts := append([]ExecutionReceipt(nil), payload.Receipts...)
		if payload.Receipt != nil {
			receipts = append(receipts, *payload.Receipt)
		}
		for _, receipt := range receipts {
			if receipt.RuntimeMediaHash == "" {
				continue
			}
			if !receipt.Succeeded() {
				if err := verifyRuntimeMediaResult(receipt.SubmittedResult, &receipt, prior); err != nil {
					return err
				}
				continue
			}
			if receipt.ModelExecutionID != "" && receipt.AcceptedResult != nil {
				if receipt.TaskID != event.TaskID {
					return media.Reject(media.AccessDenied, "accepted media leaf has another lifecycle owner")
				}
				if err := verifyRuntimeMediaResult(receipt.AcceptedResult, &receipt, prior); err != nil {
					return err
				}
				continue
			}
			if payload.Result == nil || runtimeMediaHash(payload.Result) != receipt.RuntimeMediaHash {
				return media.Reject(media.BindingConflict, "completed media receipt lost its canonical result")
			}
		}
	}
	if payload.Result == nil || payload.Result.RuntimeMedia == nil {
		return nil
	}
	if payload.Result.TaskID != event.TaskID {
		return media.Reject(media.AccessDenied, "runtime media result has another lifecycle owner")
	}
	// Intermediate projections can precede receipt finalization. TaskDone is
	// the acceptance boundary; it must carry the complete immutable binding.
	if event.Type != string(EventTaskCompleted) {
		return payload.Result.RuntimeMedia.Validate()
	}
	receipt := payload.Receipt
	for index := range payload.Receipts {
		if payload.Receipts[index].RuntimeMediaHash == payload.Result.RuntimeMedia.Hash {
			receipt = &payload.Receipts[index]
		}
	}
	return verifyRuntimeMediaResult(payload.Result, receipt, prior)
}

func (p *RuntimeMediaProjection) artifacts() []ArtifactRef {
	if p == nil {
		return nil
	}
	var refs []ArtifactRef
	seen := make(map[string]bool)
	for _, source := range p.Sources {
		for _, part := range source.Parts {
			if part.Type != "image" || seen[part.Image.Ref.ID] {
				continue
			}
			seen[part.Image.Ref.ID] = true
			// CAS metadata describes content shared by occurrences. Ownership
			// stays in this receipt-bound projection, never in blob claims.
			refs = append(refs, imageArtifactRef(part.Image.Ref))
		}
	}
	return refs
}

func (c *Coordinator) finalizeTaskMedia(ctx context.Context, identity submitResultRuntimeIdentity) error {
	projection, err := c.runtimeMediaForOccurrence(ctx, identity, "")
	if err != nil || projection == nil {
		return err
	}
	_, err = c.finalizeTaskResultRuntime(identity, func(result *TaskResult) (string, error) {
		prior := make(map[string]bool)
		for _, artifact := range result.RuntimeMedia.artifacts() {
			prior[artifact.ID] = true
		}
		var artifacts []ArtifactRef
		for _, artifact := range result.Artifacts {
			if !prior[artifact.ID] {
				artifacts = append(artifacts, artifact)
			}
		}
		result.RuntimeMedia = projection.clone()
		result.Artifacts = append(artifacts, projection.artifacts()...)
		return "", nil
	})
	return err
}
