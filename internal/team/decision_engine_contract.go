package team

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

// Binding a decision to its request contract.
//
// The contract states what the decision is for; it is loaded from the
// content-addressed store and validated rather than reconstructed from live
// configuration. Moved out of decision_engine.go unchanged.

func (e *decisionEngine) prepareRequestContract(ctx context.Context, req DecisionRequest, state decisionState) (DecisionRequest, error) {
	if state.Invalidated {
		return req, fmt.Errorf("%s: decision %s has been invalidated", ReasonDecisionStale, req.DecisionID)
	}
	if state.Profile != "" && state.ContractRef == "" && req.Contract != nil {
		return req, fmt.Errorf("decision request contract binding is unavailable for unfinished decision %s", req.DecisionID)
	}
	if req.RequireRequestContract && req.Contract == nil && state.ContractRef == "" {
		return req, CheckRequestContract(nil)
	}
	if req.Contract == nil && state.ContractRef != "" {
		req.RequestContractRef = state.ContractRef
		req.RequestContractRevision = state.ContractRevision
		envelope, err := e.loadContractArtifact(ctx, req.RequestContractRef)
		if err != nil {
			return req, err
		}
		contract := envelope.RequestContract()
		req.Contract = &contract
	}
	if req.Contract == nil {
		return req, nil
	}
	if err := CheckRequestContract(req.Contract); err != nil {
		return req, err
	}
	if strings.TrimSpace(req.RequestContractRef) == "" || req.RequestContractRevision == 0 {
		return req, fmt.Errorf("decision request contract binding is incomplete")
	}
	if err := e.validateContractArtifact(ctx, req); err != nil {
		return req, err
	}
	if state.ContractRef == "" || (state.ContractRef == req.RequestContractRef && state.ContractRevision == req.RequestContractRevision) {
		return req, nil
	}
	reason := "request contract revision superseded"
	event := decisionEventFor(req, "invalidated", req.RequestContractRef)
	event.Reason = reason
	if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionInvalidated, event); err != nil {
		return req, err
	}
	return req, fmt.Errorf("%s: %s", ReasonDecisionStale, reason)
}

func (e *decisionEngine) validateContractArtifact(ctx context.Context, req DecisionRequest) error {
	if e.services.Store == nil {
		return fmt.Errorf("decision request contract artifact store is unavailable")
	}
	envelope, err := e.loadContractArtifact(ctx, req.RequestContractRef)
	if err != nil {
		return err
	}
	if envelope.ID != req.Contract.ID || envelope.Revision != req.RequestContractRevision {
		return fmt.Errorf("request contract artifact binding mismatch")
	}
	return nil
}

func (e *decisionEngine) loadContractArtifact(ctx context.Context, ref string) (RequestContractEnvelope, error) {
	if e.services.Store == nil {
		return RequestContractEnvelope{}, fmt.Errorf("decision request contract artifact store is unavailable")
	}
	resolved, err := e.services.Store.Resolve(ctx, ArtifactRef{ID: ref})
	if err != nil {
		return RequestContractEnvelope{}, fmt.Errorf("resolve request contract artifact: %w", err)
	}
	reader, err := e.services.Store.Open(ctx, resolved.ID)
	if err != nil {
		return RequestContractEnvelope{}, fmt.Errorf("open request contract artifact: %w", err)
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(reader)
	if err != nil {
		return RequestContractEnvelope{}, fmt.Errorf("read request contract artifact: %w", err)
	}
	var envelope RequestContractEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return RequestContractEnvelope{}, fmt.Errorf("decode request contract artifact: %w", err)
	}
	if err := envelope.Validate(); err != nil {
		return RequestContractEnvelope{}, fmt.Errorf("validate request contract artifact: %w", err)
	}
	return envelope, nil
}
