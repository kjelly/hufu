package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

// These receipts attest the admitted provider request, not comprehension. They
// contain hashes and lengths, never another persistent copy of the prompt.
type toolModelInputReceipt struct {
	InvocationID    string `json:"invocation_id"`
	ModelID         string `json:"model_id"`
	RequestSHA256   string `json:"request_sha256"`
	SourceSHA256    string `json:"source_sha256"`
	SourceBytes     int    `json:"source_bytes"`
	DeliveredSHA256 string `json:"delivered_sha256"`
	DeliveredBytes  int    `json:"delivered_bytes"`
	Status          string `json:"status"`
}

type toolModelInputResult struct {
	InvocationID     string                          `json:"invocation_id"`
	ResponseObserved bool                            `json:"response_observed"`
	Outcome          agent.ProviderInvocationOutcome `json:"outcome"`
}

func toolInputDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Auxiliary calls can inherit a recorder; only its exact worker may earn
// delivery credit. Collectors and result repairs read the sealed original.
func toolInputTranscript(ctx context.Context) *taskTranscript {
	t, _ := ctx.Value(taskTranscriptKey{}).(*taskTranscript)
	meta, ok := invocationMetadataFromContext(ctx)
	if t == nil || !ok || meta.AgentRole == "auxiliary" || meta.AgentRole == "coordinator" || meta.TaskID != t.todoID || meta.RunID != t.runID || meta.Attempt != t.attempt || !strings.EqualFold(meta.AgentName, t.agent) {
		return nil
	}
	return t
}

func (c *Coordinator) recordToolModelInputs(ctx context.Context, request agent.ProviderRequest) error {
	t := toolInputTranscript(ctx)
	if t == nil {
		return nil
	}
	_, compiled, err := c.boundResultContract(t.todoID)
	if err != nil {
		return err
	}
	if compiled == nil || compiled.toolEvidence == nil || !compiled.toolEvidence.RequireModelDelivery {
		return nil
	}
	records, err := toolModelInputRecords(compiled.toolEvidence, t.evidenceRecords(), request)
	if err != nil {
		return err
	}
	for _, record := range records {
		if err := t.append(record); err != nil {
			return err
		}
	}
	return nil
}

func toolModelInputRecords(spec *resultToolEvidenceSpec, records []taskTranscriptRecord, request agent.ProviderRequest) ([]taskTranscriptRecord, error) {
	messages, err := providerMediaMessages(request)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(messages, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	requestHash := toolInputDigest(string(encoded))
	type deliveredPart struct {
		text  string
		count int
	}
	parts := make(map[string]deliveredPart)
	for _, message := range messages {
		if message.Role != fantasy.MessageRoleTool {
			continue
		}
		for _, part := range message.Content {
			result, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !ok {
				continue
			}
			text, _ := toolResultOutputText(result.Output)
			p := parts[result.ToolCallID]
			// Compare the actual admitted text. Redacting it again could hide
			// a changed value behind the same marker and falsely earn full credit.
			// Only hashes and lengths of this transient text are persisted.
			p.text, p.count = text, p.count+1
			parts[result.ToolCallID] = p
		}
	}
	var receipts []taskTranscriptRecord
	for _, observation := range matchingToolObservations(spec, records) {
		p := parts[observation.id]
		delivered := spec.observedText(p.text)
		targetMatches := spec.targetPattern == nil || spec.observedTarget(nil, p.text) == observation.match
		if observation.historical != nil {
			var recalled struct {
				Content string `json:"content"`
				Target  string `json:"target"`
			}
			if json.Unmarshal([]byte(p.text), &recalled) == nil {
				delivered, targetMatches = recalled.Content, recalled.Target == observation.match
			}
		}
		status := "omitted"
		if p.count > 0 {
			status = "modified"
			if p.count == 1 && delivered == observation.output && targetMatches {
				status = "full"
			} else if p.count == 1 && len(delivered) < len(observation.output) && (delivered != "" && strings.HasPrefix(observation.output, delivered) || strings.Contains(p.text, "[truncated")) {
				status = "truncated"
			}
		}
		r := &toolModelInputReceipt{InvocationID: request.InvocationID, ModelID: request.ModelID, RequestSHA256: requestHash, SourceSHA256: toolInputDigest(observation.output), SourceBytes: len(observation.output), DeliveredSHA256: toolInputDigest(delivered), DeliveredBytes: len(delivered), Status: status}
		receipts = append(receipts, taskTranscriptRecord{Timestamp: time.Now().Format(time.RFC3339Nano), Event: "model_input", ToolCallID: observation.id, ModelInput: r})
	}
	return receipts, nil
}

func settleToolModelInputs(ctx context.Context, request agent.ProviderRequest, result agent.ProviderInvocationResult) {
	t := toolInputTranscript(ctx)
	if t == nil {
		return
	}
	// Ordinary contracts never opt in. Avoid copying their entire evidence
	// history on every provider settlement just to discover no input receipt.
	t.mu.Lock()
	tracked := t.modelInputs
	t.mu.Unlock()
	if !tracked {
		return
	}
	// No request receipt means there were no tracked sources in this call.
	found := false
	for _, r := range t.evidenceRecords() {
		if r.ModelInput != nil && r.ModelInput.InvocationID == request.InvocationID {
			found = true
			break
		}
	}
	if !found {
		return
	}
	err := t.append(taskTranscriptRecord{Timestamp: time.Now().Format(time.RFC3339Nano), Event: "model_input_result", ProviderResult: &toolModelInputResult{InvocationID: request.InvocationID, ResponseObserved: result.ResponseObserved, Outcome: result.Outcome}})
	if err != nil {
		// Settlement cannot alter a provider result. Invalidate all in-memory
		// credit; the durable transcript has no matching settlement either.
		t.mu.Lock()
		t.evidenceInvalid = true
		t.evidence = nil
		t.mu.Unlock()
	}
}

func observedToolModelInput(records []taskTranscriptRecord, id, body string) *toolModelInputReceipt {
	var selected *toolModelInputReceipt
	sourceHash := toolInputDigest(body)
	for i, r := range records {
		if r.Event != "model_input" || r.ToolCallID != id || r.ModelInput == nil || r.ModelInput.SourceSHA256 != sourceHash || r.ModelInput.SourceBytes != len(body) {
			continue
		}
		candidate := *r.ModelInput
		if candidate.InvocationID == "" || candidate.RequestSHA256 == "" {
			continue
		}
		observed := false
		for _, result := range records[i+1:] {
			if p := result.ProviderResult; result.Event == "model_input_result" && p != nil && p.InvocationID == candidate.InvocationID && p.ResponseObserved && p.Outcome == agent.ProviderInvocationSuccess {
				observed = true
				break
			}
		}
		if !observed {
			candidate.Status = "not_observed"
		}
		if candidate.Status == "full" {
			if candidate.DeliveredSHA256 == candidate.SourceSHA256 && candidate.DeliveredBytes == candidate.SourceBytes {
				return &candidate
			}
			candidate.Status = "modified"
		}
		selected = &candidate
	}
	if selected == nil {
		return &toolModelInputReceipt{Status: "not_observed", SourceSHA256: toolInputDigest(body), SourceBytes: len(body)}
	}
	return selected
}

func modelDeliveryValue(receipt *toolModelInputReceipt) (any, error) {
	data, err := json.Marshal(receipt)
	if err != nil {
		return nil, fmt.Errorf("encode model input receipt: %w", err)
	}
	return decodeResultContractJSON(data)
}
