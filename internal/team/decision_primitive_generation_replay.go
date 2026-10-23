package team

import (
	"bytes"
	"context"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"fmt"
)

// restorePrimaryGeneration reads the non-authoritative generation already bound
// to a primitive start. The normal executor then validates/replays its settlement
// or rejects an interrupted call; neither path repeats the generation first.
func (r *coordinatorDecisionRunners) restorePrimaryGeneration(ctx context.Context, name string, request, generated any) (bool, error) {
	c := r.coordinator
	if c == nil || !c.hasDurableEventJournal() {
		return false, nil
	}
	metadata, err := c.runtimeChoiceMetadata(r.choiceContext(ctx))
	if err != nil {
		return false, &runtimeDecisionFailure{err}
	}
	if err := c.EventJournal().VerifyHashChain(ctx); err != nil {
		return false, &runtimeDecisionFailure{err}
	}
	events, err := c.readActiveLineageEvents(ctx)
	if err != nil {
		return false, &runtimeDecisionFailure{err}
	}
	scope := decisionPrimitiveScope(events, c.activeBranchID(), metadata)
	want, err := json.Marshal(request, json.Deterministic(true), jsonv1.FormatDurationAsNano(true))
	if err != nil {
		return false, &runtimeDecisionFailure{err}
	}
	var found jsonv1.RawMessage
	for _, event := range events {
		if event.Type != string(EventDecisionPrimitiveStarted) {
			continue
		}
		var p decisionPrimitivePayload
		if err := decodeStrictDecisionPayload(event.Payload, &p); err != nil {
			return false, &runtimeDecisionFailure{err}
		}
		if p.Version != 3 || p.Name != name || p.Scope != scope {
			continue
		}
		if err := validateDecisionPrimitiveEvent(event); err != nil {
			return false, &runtimeDecisionFailure{err}
		}
		raw, err := c.readDecisionArtifact(ctx, *p.Snapshot, resultDecisionContextMax)
		if err != nil {
			return false, &runtimeDecisionFailure{err}
		}
		var snapshot struct {
			Input struct {
				Request                   jsonv1.RawMessage
				NonAuthoritativeGenerated jsonv1.RawMessage
			} `json:"input"`
		}
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			return false, &runtimeDecisionFailure{err}
		}
		if !bytes.Equal(want, snapshot.Input.Request) {
			continue
		}
		if found != nil {
			return false, &runtimeDecisionFailure{fmt.Errorf("decision_generation_binding_conflict")}
		}
		found = snapshot.Input.NonAuthoritativeGenerated
	}
	if found == nil {
		return false, nil
	}
	if err := json.Unmarshal(found, generated, jsonv1.FormatDurationAsNano(true)); err != nil {
		return false, &runtimeDecisionFailure{err}
	}
	return true, nil
}
