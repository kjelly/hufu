package team

// codexWorkerResultProposalSchema builds the strict JSON Schema Codex's
// turn/start.outputSchema must enforce (docs/hufu-external-coding-agent-runtime-spec.md
// §14.5). It must: set required status and summary; enumerate allowed status
// values; disallow unknown fields; bound nested structures where JSON Schema
// permits; and never expose a trusted Hufu field (TaskID/RunID/Attempt/
// Agent/Provider/hashes/receipt IDs/verification) — this schema is generated
// directly from WorkerResultProposal's own shape, so there is nothing to
// accidentally add or forget to exclude.
//
// The decoded response is still re-validated by DecodeWorkerResultProposal's
// strict Go decoder (§9.1): this schema constrains what a well-behaved Codex
// asks the model for, it is not itself Hufu's trust boundary.
func codexWorkerResultProposalSchema() map[string]any {
	proposedFile := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"path"},
		"properties": map[string]any{
			"path":        map[string]any{"type": "string", "minLength": 1},
			"description": map[string]any{"type": "string"},
			"role":        map[string]any{"type": "string"},
		},
	}
	finding := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"category", "summary"},
		"properties": map[string]any{
			"category": map[string]any{"type": "string"},
			"summary":  map[string]any{"type": "string"},
			"detail":   map[string]any{"type": "string"},
		},
	}
	risk := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"description"},
		"properties": map[string]any{
			"description": map[string]any{"type": "string"},
			"impact":      map[string]any{"type": "string"},
			"mitigation":  map[string]any{"type": "string"},
		},
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"status", "summary"},
		"properties": map[string]any{
			"status": map[string]any{
				"type": "string",
				"enum": []string{
					TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps,
					TaskResultStatusPartial, TaskResultStatusFailed, TaskResultStatusBlocked,
				},
			},
			"summary":        map[string]any{"type": "string", "minLength": 1},
			"details":        map[string]any{"type": "string"},
			"proposed_files": map[string]any{"type": "array", "items": proposedFile, "maxItems": workerResultProposalMaxFindings},
			"findings":       map[string]any{"type": "array", "items": finding, "maxItems": workerResultProposalMaxFindings},
			"risks":          map[string]any{"type": "array", "items": risk, "maxItems": workerResultProposalMaxRisks},
			"open_questions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": workerResultProposalMaxOpenQuestions},
			"facts":          map[string]any{"type": "object"},
			"confidence":     map[string]any{"type": "number", "minimum": 0, "maximum": 1},
		},
	}
}
