package paired

import (
	"errors"
	"slices"
	"strings"
	"unicode/utf8"
)

// Evidence references are logical stage IDs inside one frozen experiment.
// The controller binds each to its committed canonical evidence before use.
// Models never supply filesystem paths or decide which journal to read.
type Change struct {
	SurfaceID       string `json:"surface_id"`
	BeforeHash      string `json:"before_hash"`
	ReplacementText string `json:"replacement_text"`
}

type ChangeProposal struct {
	SchemaVersion       int      `json:"schema_version"`
	Hypothesis          string   `json:"hypothesis"`
	ObservedFailureRefs []string `json:"observed_failure_refs"`
	Changes             []Change `json:"changes"`
	ExpectedBehavior    string   `json:"expected_behavior"`
	Applicability       string   `json:"applicability"`
}

type Composition struct {
	SchemaVersion int             `json:"schema_version"`
	Action        string          `json:"action"`
	CandidateSlot string          `json:"candidate_slot,omitempty"`
	Proposal      *ChangeProposal `json:"proposal,omitzero"`
}

type ContrastLesson struct {
	Cue                      string   `json:"cue"`
	ChangeRef                string   `json:"change_ref"`
	BaselineObservationRefs  []string `json:"baseline_observation_refs"`
	CandidateObservationRefs []string `json:"candidate_observation_refs"`
	VerificationRefs         []string `json:"verification_refs"`
	CounterevidenceRefs      []string `json:"counterevidence_refs"`
	Applicability            string   `json:"applicability"`
	Limitations              string   `json:"limitations"`
}

type Reflection struct {
	SchemaVersion int             `json:"schema_version"`
	Action        string          `json:"action"`
	Lesson        *ContrastLesson `json:"lesson,omitzero"`
}

func (p ChangeProposal) Validate() error {
	if p.SchemaVersion != SchemaVersion || len(p.Changes) == 0 || len(p.Changes) > 64 ||
		p.ObservedFailureRefs == nil || len(p.ObservedFailureRefs) > MaxCases*MaxReplicates {
		return errors.New("proposal requires version 1, evidence references and 1–64 changes")
	}
	for _, text := range []string{p.Hypothesis, p.ExpectedBehavior, p.Applicability} {
		if !proposalText(text) {
			return errors.New("proposal description must be bounded nonempty UTF-8")
		}
	}
	seen := map[string]bool{}
	for _, change := range p.Changes {
		if !ValidID(change.SurfaceID) || seen[change.SurfaceID] || !proposalText(change.ReplacementText) ||
			(ArtifactRef{Kind: "surface", Digest: change.BeforeHash}).Validate("surface") != nil {
			return errors.New("invalid or duplicate proposal change")
		}
		seen[change.SurfaceID] = true
	}
	seen = map[string]bool{}
	for _, ref := range p.ObservedFailureRefs {
		if !ValidID(ref) || seen[ref] {
			return errors.New("invalid or duplicate failure reference")
		}
		seen[ref] = true
	}
	return nil
}

func (c Composition) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return errors.New("composition requires version 1")
	}
	switch c.Action {
	case "keep_baseline":
		if c.CandidateSlot != "" || c.Proposal != nil {
			return errors.New("keep_baseline forbids candidate and proposal")
		}
	case "use_candidate":
		if !slices.Contains([]string{"procedure", "domain"}, c.CandidateSlot) || c.Proposal != nil {
			return errors.New("use_candidate requires procedure or domain only")
		}
	case "compose":
		if c.CandidateSlot != "" || c.Proposal == nil {
			return errors.New("compose requires only a proposal")
		}
		return c.Proposal.Validate()
	default:
		return errors.New("unsupported composition action")
	}
	return nil
}

func proposalText(value string) bool {
	return len(value) <= MaxTextBytes && utf8.ValidString(value) && strings.TrimSpace(value) != ""
}

// RoleSchema is compiled by the ordinary result-contract validator. Every
// action vocabulary is an explicit lowercase enum, including union arms.
func RoleSchema(role string) ([]byte, error) {
	text := map[string]any{"type": "string", "minLength": 1, "maxLength": MaxTextBytes}
	ref := map[string]any{"type": "string", "pattern": "^[a-z][a-z0-9_]{0,63}$"}
	refs := map[string]any{"type": "array", "items": ref, "uniqueItems": true, "maxItems": MaxCases * MaxReplicates}
	version := map[string]any{"type": "integer", "const": SchemaVersion}
	change := schemaObject(map[string]any{
		"surface_id": ref, "before_hash": map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"},
		"replacement_text": text,
	}, "surface_id", "before_hash", "replacement_text")
	proposal := schemaObject(map[string]any{
		"schema_version": version, "hypothesis": text, "observed_failure_refs": refs,
		"changes":           map[string]any{"type": "array", "minItems": 1, "maxItems": 64, "items": change},
		"expected_behavior": text, "applicability": text,
	}, "schema_version", "hypothesis", "observed_failure_refs", "changes", "expected_behavior", "applicability")
	var schema map[string]any
	switch role {
	case "proposer":
		schema = proposal
	case "composer":
		schema = map[string]any{"oneOf": []any{
			schemaObject(map[string]any{"schema_version": version, "action": schemaEnum("keep_baseline")}, "schema_version", "action"),
			schemaObject(map[string]any{"schema_version": version, "action": schemaEnum("use_candidate"),
				"candidate_slot": schemaEnum("procedure", "domain")}, "schema_version", "action", "candidate_slot"),
			schemaObject(map[string]any{"schema_version": version, "action": schemaEnum("compose"), "proposal": proposal},
				"schema_version", "action", "proposal"),
		}}
	case "reflector":
		lesson := schemaObject(map[string]any{
			"cue": text, "change_ref": ref, "baseline_observation_refs": refs, "candidate_observation_refs": refs,
			"verification_refs": refs, "counterevidence_refs": refs, "applicability": text, "limitations": text,
		}, "cue", "change_ref", "baseline_observation_refs", "candidate_observation_refs",
			"verification_refs", "counterevidence_refs", "applicability", "limitations")
		schema = map[string]any{"oneOf": []any{
			schemaObject(map[string]any{"schema_version": version, "action": schemaEnum("no_entry")}, "schema_version", "action"),
			schemaObject(map[string]any{"schema_version": version, "action": schemaEnum("lesson"), "lesson": lesson},
				"schema_version", "action", "lesson"),
		}}
	default:
		return nil, errors.New("unknown refinement role")
	}
	schema["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	return CanonicalJSON(schema)
}

func schemaObject(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

func schemaEnum(values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values}
}
