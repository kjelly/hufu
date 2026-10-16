package qualitative

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"slices"
)

// Project reconstructs a qualitative view from the complete input set. Input
// order and exact duplicate delivery do not affect the result. Retractions win
// regardless of position; replay cannot resurrect a tombstoned record.
// Invalid input returns no partial state. Neither input nor prior State is mutated.
func Project(input Input) (State, error) {
	records, err := normalize(input)
	if err != nil {
		return State{}, err
	}
	state := State{SchemaVersion: SchemaVersion, ReducerVersion: ReducerVersion, Scope: input.Scope,
		Records: records, RetractedIDs: []string{}, Hypotheses: []HypothesisView{}}
	withdrawn := make(map[string]bool)
	byID := make(map[string]Record, len(records))
	for _, r := range records {
		byID[r.ID] = r
		if r.Retraction != nil {
			withdrawn[r.Retraction.TargetID] = true
		}
	}
	for id := range withdrawn {
		state.RetractedIDs = append(state.RetractedIDs, id)
	}
	slices.Sort(state.RetractedIDs)
	for _, r := range records {
		if r.Kind == HypothesisRecord {
			state.Hypotheses = append(state.Hypotheses, projectHypothesis(r.ID, records, byID, withdrawn))
		}
	}
	canonical := Input{SchemaVersion: SchemaVersion, ReducerVersion: ReducerVersion, Scope: input.Scope, Records: records}
	state.InputDigest, err = digest("hufu-qualitative-input-v1", canonical)
	if err != nil {
		return State{}, err
	}
	state.Digest, err = digest("hufu-qualitative-state-v1", state)
	if err != nil {
		return State{}, err
	}
	return state, nil
}

func projectHypothesis(id string, records []Record, byID map[string]Record, withdrawn map[string]bool) HypothesisView {
	view := HypothesisView{ID: id, Status: Unresolved, SupportIDs: []string{}, CounterevidenceIDs: []string{}, PendingRelationIDs: []string{}, GapIDs: []string{}}
	if withdrawn[id] {
		view.Status = Withdrawn
		return view
	}
	for _, r := range records {
		if withdrawn[r.ID] {
			continue
		}
		if r.Gap != nil && r.Gap.HypothesisID == id {
			view.GapIDs = append(view.GapIDs, r.ID)
		}
		link := r.Relation
		if link == nil || link.HypothesisID != id {
			continue
		}
		if withdrawn[link.EvidenceID] || byID[link.EvidenceID].Evidence.Knowledge == Unknown {
			view.PendingRelationIDs = append(view.PendingRelationIDs, r.ID)
		} else if link.Stance == Support {
			view.SupportIDs = append(view.SupportIDs, r.ID)
		} else {
			view.CounterevidenceIDs = append(view.CounterevidenceIDs, r.ID)
		}
	}
	switch {
	case len(view.SupportIDs) > 0 && len(view.CounterevidenceIDs) > 0:
		view.Status = Contested
	case len(view.SupportIDs) > 0:
		view.Status = Supported
	case len(view.CounterevidenceIDs) > 0:
		view.Status = Countered
	}
	return view
}

func digest(domain string, value any) (string, error) {
	data, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return "", invalid(InvalidInput, "digest")
	}
	sum := sha256.Sum256(append([]byte(domain+"\x00"), data...))
	return hex.EncodeToString(sum[:]), nil
}
