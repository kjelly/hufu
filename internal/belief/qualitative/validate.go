package qualitative

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Decode rejects unknown fields, duplicate JSON keys, nulls, invalid enums and
// out-of-scope records. There is no permissive normalization of missing values.
func Decode(data []byte) (Input, error) {
	if len(data) > MaxInputBytes {
		return Input{}, invalid(ResourceLimit, "input_bytes")
	}
	var input Input
	if err := json.Unmarshal(data, &input, json.RejectUnknownMembers(true)); err != nil {
		return Input{}, invalid(InvalidInput, "json")
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(data, &fields); err != nil || fields["records"] == nil {
		return Input{}, invalid(InvalidInput, "records")
	}
	// JSON null must not silently become a known Go zero value or an absent arm.
	decoder := jsontext.NewDecoder(strings.NewReader(string(data)))
	for decoder.PeekKind() != 0 {
		token, err := decoder.ReadToken()
		if err != nil {
			return Input{}, invalid(InvalidInput, "json")
		}
		if token.Kind() == 'n' {
			return Input{}, invalid(InvalidInput, "null")
		}
	}
	if err := Validate(input); err != nil {
		return Input{}, err
	}
	return input, nil
}

// Validate applies the same contract to typed callers as to decoded inputs.
func Validate(input Input) error {
	_, err := normalize(input)
	return err
}

func normalize(input Input) ([]Record, error) {
	if input.SchemaVersion != SchemaVersion || input.ReducerVersion != ReducerVersion {
		return nil, invalid(UnsupportedVersion, "version")
	}
	if !validScope(input.Scope) {
		return nil, invalid(InvalidInput, "scope")
	}
	if len(input.Records) > MaxInputRecords {
		return nil, invalid(ResourceLimit, "input_records")
	}
	byID := make(map[string]Record)
	contents := make(map[string]string)
	sources := make(map[string]Source)
	for _, record := range input.Records {
		if err := validateRecord(record, input.Scope); err != nil {
			return nil, err
		}
		if prior, ok := sources[record.Source.ID]; ok && prior != record.Source {
			return nil, invalid(IdentityConflict, "source")
		}
		sources[record.Source.ID] = record.Source
		encoded, err := json.Marshal(record)
		if err != nil {
			return nil, invalid(InvalidInput, "record")
		}
		if prior, ok := contents[record.ID]; ok && prior != string(encoded) {
			return nil, invalid(IdentityConflict, "record")
		}
		contents[record.ID] = string(encoded)
		byID[record.ID] = record
	}
	if len(byID) > MaxRecords {
		return nil, invalid(ResourceLimit, "records")
	}
	data, err := json.Marshal(input)
	if err != nil {
		return nil, invalid(InvalidInput, "input")
	}
	if len(data) > MaxInputBytes {
		return nil, invalid(ResourceLimit, "input_bytes")
	}
	out := make([]Record, 0, len(byID))
	for _, record := range byID {
		if err := validateReferences(record, byID); err != nil {
			return nil, err
		}
		out = append(out, cloneRecord(record))
	}
	slices.SortFunc(out, func(a, b Record) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

func validateRecord(r Record, scope Scope) error {
	if !validID(r.ID) {
		return invalid(InvalidInput, "record.id")
	}
	if r.Scope != scope {
		return invalid(ScopeMismatch, "record.scope")
	}
	if !validID(r.Source.ID) || !validID(r.Source.Revision) || !validText(r.Source.Reference, 512) || !validDigest(r.Source.Digest) {
		return invalid(InvalidInput, "record.source")
	}
	arms := 0
	for _, present := range []bool{r.Evidence != nil, r.Hypothesis != nil, r.Relation != nil, r.Gap != nil, r.Retraction != nil} {
		if present {
			arms++
		}
	}
	if arms != 1 {
		return invalid(InvalidInput, "record.payload")
	}
	switch {
	case r.Kind == EvidenceRecord && r.Evidence != nil:
		return validateEvidence(*r.Evidence)
	case r.Kind == HypothesisRecord && r.Hypothesis != nil:
		if !validText(r.Hypothesis.Claim, MaxTextBytes) {
			return invalid(InvalidInput, "hypothesis.claim")
		}
	case r.Kind == RelationRecord && r.Relation != nil:
		v := r.Relation
		if !validID(v.HypothesisID) || !validID(v.EvidenceID) || !validText(v.Rationale, MaxTextBytes) || v.Stance != Support && v.Stance != Counterevidence {
			return invalid(InvalidInput, "relation")
		}
	case r.Kind == GapRecord && r.Gap != nil:
		if !validID(r.Gap.HypothesisID) || !validText(r.Gap.Question, MaxTextBytes) {
			return invalid(InvalidInput, "gap")
		}
	case r.Kind == RetractionRecord && r.Retraction != nil:
		if !validID(r.Retraction.TargetID) || !validText(r.Retraction.Reason, MaxTextBytes) {
			return invalid(InvalidInput, "retraction")
		}
	default:
		return invalid(InvalidInput, "record.kind")
	}
	return nil
}

func validateEvidence(e Evidence) error {
	if e.Knowledge != Known && e.Knowledge != Unknown {
		return invalid(InvalidInput, "evidence.knowledge")
	}
	if e.Knowledge == Known && !validText(e.Statement, MaxTextBytes) || e.Knowledge == Unknown && e.Statement != "" {
		return invalid(InvalidInput, "evidence.statement")
	}
	switch e.Kind {
	case Fact, Interpretation:
		if e.Verification != nil {
			return invalid(InvalidInput, "evidence.verification")
		}
	case ObjectiveVerification:
		v := e.Verification
		if v == nil || !validID(v.ReceiptID) || !validID(v.VerifierRevision) {
			return invalid(InvalidInput, "evidence.verification")
		}
		if v.Outcome != Pass && v.Outcome != Fail && v.Outcome != OutcomeUnknown {
			return invalid(InvalidInput, "verification.outcome")
		}
		if (e.Knowledge == Unknown) != (v.Outcome == OutcomeUnknown) {
			return invalid(InvalidInput, "verification.knowledge")
		}
	default:
		return invalid(InvalidInput, "evidence.kind")
	}
	return nil
}

func validateReferences(r Record, records map[string]Record) error {
	isKind := func(id string, kind Kind) bool { target, ok := records[id]; return ok && target.Kind == kind }
	switch r.Kind {
	case RelationRecord:
		if !isKind(r.Relation.HypothesisID, HypothesisRecord) || !isKind(r.Relation.EvidenceID, EvidenceRecord) {
			return invalid(InvalidReference, "relation")
		}
	case GapRecord:
		if !isKind(r.Gap.HypothesisID, HypothesisRecord) {
			return invalid(InvalidReference, "gap")
		}
	case RetractionRecord:
		target, ok := records[r.Retraction.TargetID]
		if !ok || target.Kind == RetractionRecord {
			return invalid(InvalidReference, "retraction")
		}
	}
	return nil
}

func validScope(s Scope) bool {
	return validID(s.BranchID) && validID(s.SubjectID) && validID(s.SubjectRevision) && validID(s.EnvironmentRevision)
}
func validID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' || r == ':' {
			continue
		}
		return false
	}
	return true
}
func validText(s string, limit int) bool {
	if len(s) > limit || strings.TrimSpace(s) == "" || !utf8.ValidString(s) {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' })
}
func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'f' {
			continue
		}
		return false
	}
	return true
}

func cloneRecord(r Record) Record {
	if r.Evidence != nil {
		r.Evidence = new(*r.Evidence)
		if r.Evidence.Verification != nil {
			r.Evidence.Verification = new(*r.Evidence.Verification)
		}
	}
	if r.Hypothesis != nil {
		r.Hypothesis = new(*r.Hypothesis)
	}
	if r.Relation != nil {
		r.Relation = new(*r.Relation)
	}
	if r.Gap != nil {
		r.Gap = new(*r.Gap)
	}
	if r.Retraction != nil {
		r.Retraction = new(*r.Retraction)
	}
	return r
}
