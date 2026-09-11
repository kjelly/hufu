package team

// codexWorkerResultProposalSchema builds the strict JSON Schema Codex's
// turn/start.outputSchema must enforce (docs/architecture/execution-runtime.md).
// It must: set required status and summary; enumerate allowed status
// values; disallow unknown fields; bound nested structures where JSON Schema
// permits; and never expose a trusted Hufu field (TaskID/RunID/Attempt/
// Agent/Provider/hashes/receipt IDs/verification) — this schema is generated
// directly from WorkerResultProposal's own shape, so there is nothing to
// accidentally add or forget to exclude.
//
// Shape verified live against the real OpenAI structured-output validator
// behind codex-cli 0.153.4, which rejects anything short of two additional
// constraints beyond plain JSON Schema (confirmed by the exact
// invalid_json_schema errors it returns):
//  1. every object, including every nested one, needs
//     "additionalProperties": false explicitly present;
//  2. every key listed in an object's "properties" must also appear in its
//     "required" array — there is no true "optional" property. A field that
//     is logically optional is instead typed as ["<type>", "null"] and the
//     model is expected to emit JSON null for "not present".
//
// A field with no natural null case (WorkerResultProposal's genuinely
// free-form Facts map) cannot be expressed as an open-ended object under
// rule 1 at all — see the facts variable's own comment below.
//
// The decoded response is still re-validated by DecodeWorkerResultProposal's
// strict Go decoder (§9.1): this schema constrains what a well-behaved Codex
// asks the model for, it is not itself Hufu's trust boundary.
func codexWorkerResultProposalSchema() map[string]any {
	nullable := func(t string) []string { return []string{t, "null"} }

	proposedFile := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"path", "description", "role"},
		"properties": map[string]any{
			"path":        map[string]any{"type": "string", "minLength": 1},
			"description": map[string]any{"type": nullable("string")},
			"role":        map[string]any{"type": nullable("string")},
		},
	}
	finding := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"category", "summary", "detail"},
		"properties": map[string]any{
			"category": map[string]any{"type": "string"},
			"summary":  map[string]any{"type": "string"},
			"detail":   map[string]any{"type": nullable("string")},
		},
	}
	risk := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"description", "impact", "mitigation"},
		"properties": map[string]any{
			"description": map[string]any{"type": "string"},
			"impact":      map[string]any{"type": nullable("string")},
			"mitigation":  map[string]any{"type": nullable("string")},
		},
	}
	// Facts (WorkerResultProposal.Facts map[string]any, §9.1) is genuinely
	// free-form by design, but OpenAI's strict structured-output mode has no
	// way to express an open-ended object — every object's keys must be
	// statically enumerated. Rather than fork a second, differently-shaped
	// "facts" concept, this schema only ever allows facts to be null or {}:
	// an external (Codex) provider's proposal simply never populates Facts
	// in v1. WorkerResultProposal.Facts remains available for other
	// providers/paths that decode a proposal without going through this
	// schema.
	facts := map[string]any{
		"type":                 nullable("object"),
		"additionalProperties": false,
		"properties":           map[string]any{},
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required": []string{
			"status", "summary", "details", "proposed_files", "files_read", "findings",
			"risks", "open_questions", "facts", "confidence",
		},
		"properties": map[string]any{
			"status": map[string]any{
				"type": "string",
				"enum": []string{
					TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps,
					TaskResultStatusPartial, TaskResultStatusFailed, TaskResultStatusBlocked,
				},
			},
			"summary":        map[string]any{"type": "string", "minLength": 1},
			"details":        map[string]any{"type": nullable("string")},
			"proposed_files": map[string]any{"type": nullable("array"), "items": proposedFile, "maxItems": workerResultProposalMaxFindings},
			"files_read": map[string]any{
				"type":     nullable("array"),
				"items":    map[string]any{"type": "string", "minLength": 1},
				"maxItems": workerResultProposalMaxFilesRead,
			},
			"findings":       map[string]any{"type": nullable("array"), "items": finding, "maxItems": workerResultProposalMaxFindings},
			"risks":          map[string]any{"type": nullable("array"), "items": risk, "maxItems": workerResultProposalMaxRisks},
			"open_questions": map[string]any{"type": nullable("array"), "items": map[string]any{"type": "string"}, "maxItems": workerResultProposalMaxOpenQuestions},
			"facts":          facts,
			"confidence":     map[string]any{"type": nullable("number"), "minimum": 0, "maximum": 1},
		},
	}
}
