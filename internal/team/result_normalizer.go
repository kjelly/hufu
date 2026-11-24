package team

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/utils"
)

// normalizeStructuredPayload invokes the team's action provider for the given
// normalizer capability to preprocess a model's raw submission before schema validation.
func (c *Coordinator) normalizeStructuredPayload(ctx context.Context, capability string, raw json.RawMessage) (json.RawMessage, error) {
	if c == nil || c.session == nil {
		return raw, nil
	}
	capability = normalizeCapability(capability)
	if capability == "" {
		return raw, nil
	}
	var provider ActionProvider
	if c.session.ProviderRegistry != nil {
		provider, _ = c.session.ProviderRegistry.Get(capability)
	}
	if provider == nil {
		return nil, fmt.Errorf("action provider for normalizer capability %q is not registered", capability)
	}
	action := Action{
		Capability: capability,
		Type:       "normalize",
		Payload:    string(raw),
	}
	res, err := provider.Execute(ctx, action)
	if err != nil {
		return nil, fmt.Errorf("normalizer execution failed: %w", err)
	}
	return parseNormalizerOutput(res, raw)
}

func parseNormalizerOutput(res any, fallback json.RawMessage) (json.RawMessage, error) {
	switch v := res.(type) {
	case nil:
		return fallback, nil
	case json.RawMessage:
		return v, nil
	case []byte:
		if len(bytes.TrimSpace(v)) == 0 {
			return fallback, nil
		}
		return json.RawMessage(v), nil
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return fallback, nil
		}
		return json.RawMessage(trimmed), nil
	case ActionResult:
		return extractActionResultPayload(v, fallback)
	case *ActionResult:
		if v == nil {
			return fallback, nil
		}
		return extractActionResultPayload(*v, fallback)
	case map[string]any:
		if outputs, ok := v["outputs"].(map[string]any); ok {
			return extractMapOutputsPayload(outputs, fallback)
		}
		data, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("marshal normalized map: %w", err)
		}
		return json.RawMessage(data), nil
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("marshal normalized result: %w", err)
		}
		return json.RawMessage(data), nil
	}
}

func extractActionResultPayload(result ActionResult, fallback json.RawMessage) (json.RawMessage, error) {
	if result.Outputs == nil {
		return fallback, nil
	}
	return extractMapOutputsPayload(result.Outputs, fallback)
}

func extractMapOutputsPayload(outputs map[string]any, fallback json.RawMessage) (json.RawMessage, error) {
	for _, key := range []string{"normalized_payload", "structured_payload", "payload", "result"} {
		if val, ok := outputs[key]; ok && val != nil {
			switch s := val.(type) {
			case string:
				return json.RawMessage(strings.TrimSpace(s)), nil
			case []byte:
				return json.RawMessage(bytes.TrimSpace(s)), nil
			case json.RawMessage:
				return s, nil
			default:
				data, err := json.Marshal(s)
				if err != nil {
					return nil, fmt.Errorf("marshal normalizer output %q: %w", key, err)
				}
				return json.RawMessage(data), nil
			}
		}
	}
	return fallback, nil
}

// applyResultContractReceipt records the attempt's result contract outcome
// on its receipt.
func (c *Coordinator) applyResultContractReceipt(todoID string, result *TaskResult, receipt *ExecutionReceipt) {
	if receipt == nil {
		return
	}
	// Accumulate: a result-only repair re-applies the outcome to the same
	// attempt's receipt after the worker turn already recorded its own.
	receipt.ResultValidationFailures += c.takeResultValidationFailures(todoID)
	var ref *ResultContractRef
	if item := c.todoItemByID(todoID); item != nil {
		ref = item.ResultContract
	}
	if ref == nil {
		return
	}
	receipt.ResultContractID = ref.ID
	receipt.ResultContractSHA256 = ref.SchemaSHA256
	switch {
	case result != nil && result.StructuredPayload != nil:
		receipt.ResultPayloadSHA256 = result.StructuredPayload.SHA256
		if result.StructuredPayload.RawSHA256 != "" {
			receipt.ResultRawPayloadSHA256 = result.StructuredPayload.RawSHA256
		}
		receipt.ResultValidation = ResultValidationValid
	case receipt.ResultValidationFailures > 0:
		receipt.ResultValidation = ResultValidationInvalid
	default:
		receipt.ResultValidation = ResultValidationAbsent
	}
}

// formatForContext renders a validated payload for the coordinator and
// downstream task context, bounded to keep context budgets predictable.
func (p *ResultPayload) formatForContext() string {
	if p == nil {
		return ""
	}
	header := fmt.Sprintf("Structured Payload (contract %s, sha256 %s):\n", p.Contract.ID, p.SHA256)
	if p.RawSHA256 != "" {
		header = fmt.Sprintf("Structured Payload (contract %s, sha256 %s, raw_sha256 %s):\n", p.Contract.ID, p.SHA256, p.RawSHA256)
	}
	if len(p.Value) <= resultPayloadContextMaxBytes {
		return header + string(p.Value) + "\n"
	}
	return header + utils.TruncateString(string(p.Value), resultPayloadContextMaxBytes) +
		fmt.Sprintf("\n[structured payload truncated at %d bytes; the complete value is stored in the durable typed result]\n", resultPayloadContextMaxBytes)
}

// accumulateResultContractMetrics projects rejected structured payloads from
// the run's receipts.
func accumulateResultContractMetrics(metrics *RunMetrics, receipt ExecutionReceipt, runID string) {
	if metrics == nil || (runID != "" && receipt.RunID != runID) {
		return
	}
	metrics.StructuredResultValidationFailures += receipt.ResultValidationFailures
}
