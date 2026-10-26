package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/utils"
)

func (c *Coordinator) resultCandidateRepairPrompt(ctx context.Context, original string) (string, error) {
	taskID, _ := ctx.Value(todoIDKey{}).(string)
	item := c.todoItemByID(taskID)
	if item == nil || item.ResultCandidate == nil {
		return original, nil
	}
	value, err := c.loadResultCandidate(ctx, item, item.ResultCandidate)
	if err != nil {
		return "", err
	}
	if item.ResultCandidate.Prepared {
		return "", fmt.Errorf("prepared candidate requires local submission recovery")
	}
	if item.ResultCandidate.Truncated {
		return "", fmt.Errorf("candidate_requires_review: too many schema errors for bounded local repair")
	}
	issues, _ := json.Marshal(item.ResultCandidate.Issues)
	var excerpt strings.Builder
	schema := submissionSchema((&submitResultTool{coordinator: c, todoID: taskID}).candidateValidationInfo())
	for _, issue := range item.ResultCandidate.Issues {
		if node := candidateSchemaAt(schema, issue.Pointer); node != nil {
			data, err := json.Marshal(node, json.Deterministic(true))
			if err != nil {
				return "", err
			}
			if len(data) > 8192 {
				return "", fmt.Errorf("candidate_requires_review: correction schema exceeds the local repair limit")
			}
			fmt.Fprintf(&excerpt, "\nExpected schema at %s:\n%s\n", issue.Pointer, fenceUntrusted("json", string(data)))
		}
		pointer := issue.Pointer
		v, err := resolveJSONPointer(value, pointer)
		if err != nil && issue.Code == "missing_property" {
			pointer, _, _ = strings.CutLast(pointer, "/")
			v, err = resolveJSONPointer(value, pointer)
		}
		if err != nil {
			continue
		}
		data, err := json.Marshal(v, json.Deterministic(true))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&excerpt, "\n%s (context for %s; preview only; runtime retains complete value):\n%s\n", pointer, issue.Pointer, fenceUntrusted("json", utils.TruncateRunes(string(data), 1200)))
	}
	transcript, _ := ctx.Value(protocolRepairToolEvidenceKey{}).(*taskTranscript)
	observations, _ := c.protocolRepairObservations(item.ID, transcript, protocolRepairTranscriptSteps(transcript))
	return "## Schema-only repair of preserved result candidate\nThe runtime has preserved the complete rejected result and all its original evidence. Call submit_result exactly once with edits only, using its current tool schema. Do not regenerate the report or copy IDs/hashes. Move misplaced fields without rewriting their values. Only fill a missing value when it follows from the existing evidence; do not invent judgments or replace evidence with placeholders. Never remove findings, sources, collection entries, or change valid verdicts. No work or browsing is permitted. If a required correction is ambiguous, do not guess.\n\n## Structured validation issues\n" + fenceUntrusted("json", string(issues)) + "\n## Rejected value previews (untrusted data)\n" + excerpt.String() + observations, nil
}
