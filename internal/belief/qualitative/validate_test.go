package qualitative

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
)

func TestTypedValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Input)
	}{
		{"scope", func(i *Input) { i.Scope.SubjectID = "" }},
		{"id", func(i *Input) { i.Records[0].ID = "not an id" }},
		{"source", func(i *Input) { i.Records[0].Source.ID = "" }},
		{"source_digest", func(i *Input) { i.Records[0].Source.Digest = strings.Repeat("A", 64) }},
		{"source_revision", func(i *Input) { i.Records[0].Source.Revision = "" }},
		{"source_locator", func(i *Input) { i.Records[0].Source.Reference = "  " }},
		{"kind", func(i *Input) { i.Records[0].Kind = "Hypothesis" }},
		{"missing_payload", func(i *Input) { i.Records[0].Hypothesis = nil }},
		{"extra_payload", func(i *Input) { i.Records[0].Evidence = i.Records[1].Evidence }},
		{"wrong_payload", func(i *Input) { i.Records[0].Kind = EvidenceRecord }},
		{"claim", func(i *Input) { i.Records[0].Hypothesis.Claim = "\t\n" }},
		{"control", func(i *Input) { i.Records[0].Hypothesis.Claim = "text\x1b" }},
		{"invalid_utf8", func(i *Input) { i.Records[0].Hypothesis.Claim = string([]byte{255}) }},
		{"missing_knowledge", func(i *Input) { i.Records[1].Evidence.Knowledge = "" }},
		{"known_empty", func(i *Input) { i.Records[1].Evidence.Statement = "" }},
		{"unknown_nonempty", func(i *Input) { i.Records[1].Evidence.Knowledge = Unknown }},
		{"evidence_kind", func(i *Input) { i.Records[1].Evidence.Kind = "guess" }},
		{"fact_verification", func(i *Input) { i.Records[1].Evidence.Verification = i.Records[3].Evidence.Verification }},
		{"missing_verification", func(i *Input) { i.Records[3].Evidence.Verification = nil }},
		{"receipt", func(i *Input) { i.Records[3].Evidence.Verification.ReceiptID = "" }},
		{"verifier", func(i *Input) { i.Records[3].Evidence.Verification.VerifierRevision = "" }},
		{"outcome", func(i *Input) { i.Records[3].Evidence.Verification.Outcome = "timeout" }},
		{"unknown_outcome_known_value", func(i *Input) { i.Records[3].Evidence.Verification.Outcome = OutcomeUnknown }},
		{"known_outcome_unknown_value", func(i *Input) { i.Records[3].Evidence.Knowledge = Unknown; i.Records[3].Evidence.Statement = "" }},
		{"stance", func(i *Input) { i.Records[4].Relation.Stance = "confirm" }},
		{"rationale", func(i *Input) { i.Records[4].Relation.Rationale = "" }},
		{"relation_id", func(i *Input) { i.Records[4].Relation.EvidenceID = "" }},
		{"gap_question", func(i *Input) { i.Records[6].Gap.Question = "" }},
		{"retraction_reason", func(i *Input) { r := retract("r", "f"); r.Retraction.Reason = ""; i.Records = append(i.Records, r) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := fixture()
			tc.mutate(&input)
			requireError(t, Validate(input), InvalidInput)
		})
	}
	for _, version := range []string{"", "qualitative_v2"} {
		input := fixture()
		input.ReducerVersion = version
		requireError(t, Validate(input), UnsupportedVersion)
	}
	input := fixture()
	input.SchemaVersion = 0
	requireError(t, Validate(input), UnsupportedVersion)
}

func TestSizeBounds(t *testing.T) {
	input := fixture()
	input.Records[0].ID = strings.Repeat("a", 128)
	input.Records = input.Records[:1]
	if err := Validate(input); err != nil {
		t.Fatal(err)
	}
	input.Records[0].ID += "a"
	requireError(t, Validate(input), InvalidInput)
	input = fixture()
	input.Records[0].Hypothesis.Claim = strings.Repeat("a", MaxTextBytes)
	if err := Validate(input); err != nil {
		t.Fatal(err)
	}
	input.Records[0].Hypothesis.Claim += "a"
	requireError(t, Validate(input), InvalidInput)
	input = fixture()
	h := input.Records[0]
	input.Records = nil
	for n := range MaxRecords {
		r := h
		r.ID = fmt.Sprintf("h-%d", n)
		input.Records = append(input.Records, r)
	}
	if err := Validate(input); err != nil {
		t.Fatal(err)
	}
	r := h
	r.ID = "one-more"
	input.Records = append(input.Records, r)
	requireError(t, Validate(input), ResourceLimit)
	input.Records = make([]Record, MaxInputRecords)
	for n := range input.Records {
		input.Records[n] = h
	}
	if err := Validate(input); err != nil {
		t.Fatal(err)
	}
	input.Records = append(input.Records, h)
	requireError(t, Validate(input), ResourceLimit)
	_, err := Decode([]byte(strings.Repeat(" ", MaxInputBytes+1)))
	requireError(t, err, ResourceLimit)
	input = fixture()
	h = input.Records[0]
	h.Hypothesis.Claim = strings.Repeat("a", MaxTextBytes)
	input.Records = make([]Record, 300)
	for n := range input.Records {
		input.Records[n] = h
	}
	requireError(t, Validate(input), ResourceLimit)
}

func TestDecodeStrictJSON(t *testing.T) {
	data, err := json.Marshal(fixture())
	if err != nil {
		t.Fatal(err)
	}
	original := string(data)
	empty := fixture()
	empty.Records = []Record{}
	emptyData, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalidJSON := range []string{
		strings.Replace(string(emptyData), `"records":[]`, `"records":null`, 1),
		strings.Replace(string(emptyData), `,"records":[]`, ``, 1),
	} {
		_, err := Decode([]byte(invalidJSON))
		requireError(t, err, InvalidInput)
	}

	for name, data := range map[string]string{
		"unknown_field":    strings.Replace(original, `"schema_version":1`, `"schema_version":1,"unexpected":true`, 1),
		"duplicate_key":    strings.Replace(original, `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1),
		"null_and_unknown": strings.Replace(original, `"records":[`, `"records":null,"ignored":[`, 1),
		"optional_null":    strings.Replace(original, `"kind":"fact"`, `"kind":"fact","verification":null`, 1),
		"nested_unknown":   strings.Replace(original, `"kind":"fact"`, `"kind":"fact","confidence":1`, 1),
		"trailing":         original + `{}`,
		"case":             strings.Replace(original, `"schema_version"`, `"Schema_version"`, 1),
		"not_json":         "not json",
	} {
		t.Run(name, func(t *testing.T) { _, err := Decode([]byte(data)); requireError(t, err, InvalidInput) })
	}
}

func FuzzDecodeReplay(f *testing.F) {
	data, err := json.Marshal(fixture())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(data)
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		input, err := Decode(data)
		if err != nil {
			return
		}
		first := project(t, input)
		second := project(t, input)
		if first.Digest != second.Digest {
			t.Fatal("nondeterministic replay")
		}
	})
}
