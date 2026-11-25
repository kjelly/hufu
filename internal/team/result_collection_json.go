package team

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"strings"
)

// collectionResponseJSON removes only one complete transport code block around
// one bounded JSON object. The response artifact retains its original bytes;
// the returned claims still undergo all schema, source and receipt checks.
// Prose, multiple frames, duplicate keys and non-object values stay rejected by
// the strict claims decoder rather than being guessed or repaired here.
func collectionResponseJSON(raw []byte) ([]byte, bool) {
	body, framed := collectionResponseFrame(raw)
	if !framed {
		return raw, false
	}
	value, err := decodeUniqueJSON(body)
	if err != nil {
		return raw, false
	}
	if _, object := value.(map[string]any); !object {
		return raw, false
	}
	return body, true
}

// Source inspection can find individual fields inside a complete frame even
// when strict decoding of the enclosed document will fail.
func collectionResponseFrame(raw []byte) ([]byte, bool) {
	if len(raw) > resultCollectionResponseMaxBytes {
		return nil, false
	}
	trimmed := bytes.TrimSpace(raw)
	for _, prefix := range []string{"```json\n", "```json\r\n", "```\n", "```\r\n"} {
		candidate, opened := bytes.CutPrefix(trimmed, []byte(prefix))
		if opened {
			if bytes.Equal(candidate, []byte("```")) {
				return nil, true
			}
			if content, closed := bytes.CutSuffix(candidate, []byte("\n```")); closed {
				return bytes.TrimSpace(content), true
			}
		}
	}
	return nil, false
}

func collectionSourceInspectionJSON(raw string) string {
	if body, framed := collectionResponseFrame([]byte(raw)); framed {
		return string(body)
	}
	// An interrupted frame can still contain complete claims or credential
	// fields. Only source inspection and diagnostics use this prefix.
	trimmed := strings.TrimSpace(raw)
	for _, prefix := range []string{"```json\n", "```json\r\n", "```\n", "```\r\n"} {
		if body, opened := strings.CutPrefix(trimmed, prefix); opened {
			return strings.TrimSpace(body)
		}
	}
	return raw
}

// The response channel stores rejected arguments verbatim. A malformed outer
// object can still carry complete result claims, including repeated result
// members. Authenticate each before treating the transport failure as repairable.
func (c *Coordinator) validateRejectedCollectionResponseSources(ctx context.Context, cp *ResultCollectionCheckpoint, work *resultCollectionWork, compiled *CompiledResultContract, blob *resultCollectionBlob, assessRecovery bool) error {
	if blob.ResponseIssue == nil {
		return nil
	}
	ref, _, err := c.boundResultContract(cp.TaskID)
	if err != nil {
		return err
	}
	for _, claims := range readableRejectedResultClaims(string(blob.Data)) {
		claims = collectionSourceInspectionJSON(claims)
		fields := readableSourceMembers(claims)
		if len(fields) == 0 {
			continue
		}
		if err := c.validateSubmissionSourceEnvelope(ctx, cp.TaskID, work.Workspace, claims, cp, compiled); err != nil && localResultRejectionReason(err) != RepairFailureInvalidSchema {
			return err
		}
		if assessRecovery {
			var result TaskResult
			result.MemoryUses, _ = submittedSourceClaims[MemoryUseRef](fields, "memory_uses", false)
			result.AssumptionChecks, _ = submittedSourceClaims[AssumptionCheck](fields, "assumption_checks", false)
			if err := c.validateRecoveryContextClaims(ctx, cp, &result, fields); err != nil {
				return err
			}
		}
		if compiled == nil || ref == nil || ref.Normalizer != "" {
			continue
		}
		if err := c.validateReadablePayloadSources(ctx, cp.TaskID, compiled, claims, false); err != nil && localResultRejectionReason(err) != RepairFailureInvalidSchema {
			return err
		}
	}
	return nil
}

func readableRejectedResultClaims(raw string) []string {
	var results []string
	for _, field := range readableSourceMembers(raw) {
		if field.name != "result" {
			continue
		}
		switch field.raw.Kind() {
		case '{':
			results = append(results, string(field.raw))
		case '"':
			var encoded string
			if json.Unmarshal(field.raw, &encoded) == nil {
				results = append(results, encoded)
			}
		}
	}
	return results
}
