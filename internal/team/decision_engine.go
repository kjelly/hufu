package team

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// The decision engine (docs/hufu-decision-aware-runtime-spec.md §43 Phase 1).
//
// The engine owns procedure: it seals evidence, dispatches N isolated judges,
// validates their structured output, aggregates deterministically in Go, and
// persists a durable record. Judgment itself stays with the agents behind
// JudgeRunner; the engine never asks a model to do arithmetic and never lets a
// judge see another judge's view.

// JudgeRequest is one isolated dispatch.
type JudgeRequest struct {
	DecisionID string
	Round      int
	JudgeID    string
	Context    JudgeContext
	Packet     DecisionEvidencePacket
}

// JudgeRunner executes one judge and returns its structured opinion. Callers
// must not populate Valid; the engine decides validity (spec §14.3).
type JudgeRunner interface {
	RunJudge(ctx context.Context, req JudgeRequest) (DecisionOpinion, error)
}

type ReferenceEvidenceRequest struct {
	SchemaVersion    int    `json:"schema_version"`
	InvocationID     string `json:"invocation_id"`
	InputHash        string `json:"input_hash"`
	DecisionID       string `json:"decision_id"`
	RunID            string `json:"run_id,omitempty"`
	TaskID           string `json:"task_id,omitempty"`
	Question         string `json:"question"`
	ContractRef      string `json:"contract_ref,omitempty"`
	ContractRevision uint64 `json:"contract_revision,omitempty"`
}

type ReferenceEvidenceRunner interface {
	RunReferenceEvidence(context.Context, ReferenceEvidenceRequest) (ReferenceEvidenceDraft, error)
}

// DecisionServices are the runtime collaborators the engine needs. They map to
// interfaces this repo already has; the draft spec's AgentRuntime type does not
// exist (spec §4.2).
type DecisionServices struct {
	Judges  JudgeRunner
	Journal EventJournal
	Store   ArtifactStore
	Budget  BudgetManager

	// Premortems, Challengers and Revisions back the Phase 2 quality stages.
	// A nil runner disables its stage unless the profile requires it, in which
	// case the decision fails closed rather than quietly skipping the gate.
	Premortems  PremortemRunner
	Challengers ChallengeRunner
	Revisions   RevisionRunner

	// Proposer backs the option proposal stage. It is only consulted when the
	// task declared no options and the profile enables proposal (spec §19.1).
	Proposer          OptionProposer
	ReferenceEvidence ReferenceEvidenceRunner

	// Index is the cross-run decision index. A finalized decision is listed in
	// it so `hufu decision resolve` can find it after the process exits; the
	// index is a projection, never the source of truth (spec §49.2).
	Index *DecisionIndex

	// Now and NewID exist so tests get deterministic records. Both default to
	// wall-clock time and a counter-based ID when unset.
	Now   func() time.Time
	NewID func(prefix string) string
}

// DecisionRequest is one decision to form.
type DecisionRequest struct {
	RunID   string
	TaskID  string
	Profile string
	Policy  DecisionPolicy

	Question string
	Options  []DecisionOption

	Facts       map[string]any
	Artifacts   []ArtifactRef
	BaseRates   []BaseRateEvidence
	Assumptions []DecisionAssumption
	Provenance  []EvidenceProvenance

	RequestContractRef         string
	RequestContractRevision    uint64
	RequestContractArtifact    ArtifactRef
	EvidenceArtifactRef        ArtifactRef
	ReferenceEvidenceResultRef ArtifactRef

	// Role, ProjectContext and Memory are the non-evidence context sources a
	// judge may receive under strict isolation (spec §16).
	Role           string
	ProjectContext string
	Memory         string

	// Contract carries the request-scoped objective and success criteria.
	// Structured decisions require one (spec §11).
	Contract               *RequestContract
	RequireRequestContract bool

	// FinalizationOverride and FinalizationReason apply only under coordinator
	// or judge finalization. Choosing against the aggregate requires a reason,
	// which is persisted (spec §26).
	FinalizationOverride string
	FinalizationReason   string

	// DecisionID lets a caller resume a specific decision. Empty means new.
	DecisionID string
}

// DecisionEngine forms and replays decisions.
type DecisionEngine interface {
	Run(ctx context.Context, req DecisionRequest) (*DecisionRecord, error)
	Resume(ctx context.Context, decisionID string) (*DecisionRecord, error)
}

type decisionEngine struct {
	services DecisionServices
	// pending holds requests by decision ID so Resume can continue a run the
	// process started before crashing within the same session.
	pending map[string]DecisionRequest
	counter int
}

// NewDecisionEngine builds an engine over the given services.
func NewDecisionEngine(services DecisionServices) DecisionEngine {
	return &decisionEngine{services: services, pending: map[string]DecisionRequest{}}
}

func (e *decisionEngine) now() time.Time {
	if e.services.Now != nil {
		return e.services.Now()
	}
	return time.Now().UTC()
}

func (e *decisionEngine) newID(prefix string) string {
	if e.services.NewID != nil {
		return e.services.NewID(prefix)
	}
	e.counter++
	return fmt.Sprintf("%s-%d-%d", prefix, e.now().UnixNano(), e.counter)
}

// Run forms a decision from scratch, or continues one whose ID is already
// known and whose events are already durable.
func (e *decisionEngine) Run(ctx context.Context, req DecisionRequest) (*DecisionRecord, error) {
	req = cloneDecisionRequest(req)
	if err := req.Policy.Validate(); err != nil {
		return nil, fmt.Errorf("decision policy: %w", err)
	}
	if req.DecisionID == "" {
		req.DecisionID = e.newID("decision")
	}
	e.pending[req.DecisionID] = req
	return e.run(ctx, req)
}

// cloneDecisionRequest establishes the engine's mutable admission boundary.
// Resolution normalizes artifact references in place, so borrowed nested
// slices must never point back into a TaskDef or another caller-owned request.
func cloneDecisionRequest(req DecisionRequest) DecisionRequest {
	clone := req
	clone.Options = append([]DecisionOption(nil), req.Options...)
	clone.Facts = cloneDecisionFacts(req.Facts)
	clone.Artifacts = append([]ArtifactRef(nil), req.Artifacts...)
	clone.BaseRates = cloneBaseRateEvidence(req.BaseRates)
	clone.Assumptions = cloneDecisionAssumptions(req.Assumptions)
	clone.Provenance = cloneEvidenceProvenance(req.Provenance)
	if req.Contract != nil {
		contract := *req.Contract
		contract.SuccessCriteria = append([]SuccessCriterion(nil), req.Contract.SuccessCriteria...)
		contract.Constraints = append([]Constraint(nil), req.Contract.Constraints...)
		contract.Assumptions = cloneDecisionAssumptions(req.Contract.Assumptions)
		clone.Contract = &contract
	}
	clone.Policy.Criteria = append([]DecisionCriterion(nil), req.Policy.Criteria...)
	return clone
}

// Resume continues a decision from its durable event log.
func (e *decisionEngine) Resume(ctx context.Context, decisionID string) (*DecisionRecord, error) {
	req, ok := e.pending[decisionID]
	if !ok {
		return nil, fmt.Errorf("decision %s is not known to this engine; resume requires its request", decisionID)
	}
	return e.run(ctx, req)
}

func (e *decisionEngine) run(ctx context.Context, req DecisionRequest) (*DecisionRecord, error) {
	state, err := projectDecision(ctx, e.services.Journal, req.DecisionID)
	if err != nil {
		return nil, err
	}
	// A finalized decision is never recomputed (spec §35).
	if state.Record != nil {
		return state.Record, nil
	}
	if req.EvidenceArtifactRef.ID == "" {
		req.EvidenceArtifactRef = state.EvidenceArtifact
	}
	if err := validateReferenceRecoveryState(state); err != nil {
		return nil, err
	}
	var contractErr error
	req, contractErr = e.prepareRequestContract(ctx, req, state)
	if contractErr != nil {
		return nil, contractErr
	}

	if req.Contract != nil && state.Profile == "" {
		if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventRequestContractCommitted, decisionEvent{
			DecisionID: req.DecisionID, TaskID: req.TaskID, ContractRef: req.RequestContractRef,
			ContractRevision: req.RequestContractRevision, ContractArtifact: req.RequestContractArtifact,
		}); err != nil {
			return nil, err
		}
	}
	if state.Profile == "" {
		if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionStarted, decisionEvent{
			DecisionID: req.DecisionID, TaskID: req.TaskID, Profile: req.Profile,
		}); err != nil {
			return nil, err
		}
	}

	policy, degradations, err := e.admitBudget(ctx, req, state)
	if err != nil {
		return nil, err
	}

	// Options are settled before any gate runs: the alternatives gate has to
	// judge the set that judges will actually see, including anything the
	// runtime injected to satisfy the policy (spec §19.1).
	options, err := e.proposeOptions(ctx, req, policy, state)
	if err != nil {
		return nil, err
	}
	req.Options = options
	req, err = e.attachReferenceEvidence(ctx, req, state)
	if err != nil {
		return nil, err
	}
	if err := e.resolveDecisionEvidence(ctx, &req); err != nil {
		return nil, err
	}

	// Gates run before JUDGE. Adding a "do nothing" option after judges have
	// scored a two-option list does not change what they considered.
	if err := e.checkPreJudgeGates(req); err != nil {
		return nil, err
	}

	premortem, err := e.runPremortem(ctx, req, policy, state)
	if err != nil {
		return nil, err
	}

	packet, packetArtifact, err := e.sealEvidence(ctx, req, state)
	if err != nil {
		return nil, err
	}
	// Re-project so a fresh seal's stale-hash bookkeeping is visible to the
	// stages that reuse durable results.
	if state.Packet.Hash != packet.Hash {
		state.Packet = packet
	}

	weights, err := normalizedWeights(packet.Criteria)
	if err != nil {
		return nil, fmt.Errorf("decision %s: %w", req.DecisionID, err)
	}

	opinions, err := e.collectOpinions(ctx, req, policy, packet, weights, state)
	if err != nil {
		return nil, err
	}

	aggregate, err := Aggregate(packet, opinions, policy.EffectiveAggregation(), 1)
	if err != nil {
		return nil, fmt.Errorf("decision %s: %w", req.DecisionID, err)
	}
	aggregate.ID = e.newID("aggregate")
	if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionAggregateComputed, decisionEvent{
		DecisionID: req.DecisionID, EvidenceHash: packet.Hash, Round: 1, Aggregate: &aggregate,
	}); err != nil {
		return nil, err
	}

	challenges, err := e.runChallenges(ctx, req, policy, packet, aggregate, opinions, state)
	if err != nil {
		return nil, err
	}

	revisions, round2, err := e.runRevisions(ctx, req, policy, packet, weights, aggregate, challenges, opinions, state)
	if err != nil {
		return nil, err
	}

	aggregates := []DecisionAggregate{aggregate}
	finalAggregate := aggregate
	if round2 != nil {
		aggregates = append(aggregates, *round2)
		finalAggregate = *round2
	}

	if gate := CheckPremortem(policy.Premortem, premortem); gate != nil {
		return nil, gate
	}

	record := e.buildRecord(req, policy, packet, opinions, aggregates, finalAggregate, append(state.Degradations, degradations...))
	record.RequestContractRef = req.RequestContractRef
	record.RequestContractRevision = req.RequestContractRevision
	if packetArtifact.ID != "" {
		record.EvidenceArtifactRef = &packetArtifact
	}
	if req.ReferenceEvidenceResultRef.ID != "" {
		record.ReferenceEvidenceResultRef = &req.ReferenceEvidenceResultRef
	}
	record.Challenges = challenges
	record.Revisions = revisions
	record.Premortem = premortem
	record.FalsificationConditions = PremortemFalsifications(premortem, challenges)

	finalOption, overridden, err := FinalOptionFor(policy, finalAggregate, req.FinalizationOverride, req.FinalizationReason)
	if err != nil {
		return nil, fmt.Errorf("decision %s: %w", req.DecisionID, err)
	}
	record.FinalOption = finalOption
	record.Probability = finalAggregate.MeanProbability[finalOption]
	if overridden {
		if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionFinalizationOverride, decisionEvent{
			DecisionID: req.DecisionID, EvidenceHash: packet.Hash,
			Reason: fmt.Sprintf("%s chosen over aggregate %s: %s", finalOption, finalAggregate.PreferredOption, req.FinalizationReason),
		}); err != nil {
			return nil, err
		}
	}

	if err := e.applyProvenance(ctx, req, policy, packet, &record); err != nil {
		return nil, err
	}
	if gate := CheckForecast(policy.Forecast, record); gate != nil {
		return nil, gate
	}

	recordRef, err := persistDecisionRecord(ctx, e.services.Store, record)
	if err != nil {
		return nil, err
	}
	if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionFinalized, decisionEvent{
		DecisionID: req.DecisionID, EvidenceHash: packet.Hash, Record: &record,
		Question: req.Question, ForecastRequired: policy.Forecast.Required, RecordRef: recordRef,
	}); err != nil {
		return nil, err
	}
	// Listing the decision is the last step: a decision that failed a gate is
	// not addressable for resolution, because it was never made.
	if e.services.Index != nil {
		entry := IndexEntryFor(record, req.Question, policy.Forecast.Required, recordRef)
		if err := e.services.Index.Append(entry); err != nil {
			return nil, fmt.Errorf("decision %s: %w", req.DecisionID, err)
		}
	}
	return &record, nil
}

func validateReferenceRecoveryState(state decisionState) error {
	if state.ReferenceFailure != nil || (state.ReferenceInvocation != nil && state.ReferenceResult == nil) {
		detail := "reference evidence invocation has no safely reusable completed result"
		if state.ReferenceFailure != nil {
			detail = state.ReferenceFailure.Reason
		}
		return &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: detail}
	}
	return nil
}

func (e *decisionEngine) attachReferenceEvidence(ctx context.Context, req DecisionRequest, state decisionState) (DecisionRequest, error) {
	if len(req.BaseRates) == 0 && req.Policy.OutsideView.Required && req.Policy.OutsideView.ReferenceEvidence {
		rates, artifacts, provenance, resultRef, err := e.runReferenceEvidence(ctx, req, state)
		if err != nil {
			return req, err
		}
		req.BaseRates = rates
		req.Artifacts = append(req.Artifacts, artifacts...)
		req.Provenance = append(req.Provenance, provenance...)
		req.ReferenceEvidenceResultRef = resultRef
		return req, nil
	}
	if state.ReferenceResult != nil {
		req.BaseRates = cloneBaseRateEvidence(state.ReferenceResult.BaseRates)
		req.Artifacts = append(req.Artifacts, state.ReferenceResult.Artifacts...)
		req.Provenance = append(req.Provenance, state.ReferenceResult.Provenance...)
		if state.ReferenceResult.ResultArtifactRef != nil {
			req.ReferenceEvidenceResultRef = *state.ReferenceResult.ResultArtifactRef
		}
	}
	return req, nil
}

func (e *decisionEngine) resolveDecisionEvidence(ctx context.Context, req *DecisionRequest) error {
	if req == nil {
		return fmt.Errorf("%s: decision request is nil", ReasonDecisionOutsideViewMissing)
	}
	if _, err := canonicalEncode(req.Facts); err != nil {
		return &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: fmt.Sprintf("facts are not canonicalizable: %v", err)}
	}
	if !decisionRequestDeclaresEvidence(*req) {
		return nil
	}
	if e.services.Store == nil {
		return &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: "decision evidence requires an artifact store"}
	}
	resolve := func(ref ArtifactRef) (ArtifactRef, error) {
		resolved, err := e.services.Store.Resolve(ctx, ref)
		if err != nil {
			return ArtifactRef{}, err
		}
		return resolved, nil
	}
	for i, ref := range req.Artifacts {
		resolved, err := resolve(ref)
		if err != nil {
			return &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: fmt.Sprintf("artifacts[%d] cannot be resolved: %v", i, err)}
		}
		req.Artifacts[i] = resolved
	}
	for i := range req.BaseRates {
		resolved, err := resolve(req.BaseRates[i].Source)
		if err != nil {
			return &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: fmt.Sprintf("base_rates[%d] source cannot be resolved: %v", i, err)}
		}
		req.BaseRates[i].Source = resolved
	}
	for i := range req.Assumptions {
		for j, ref := range req.Assumptions[i].EvidenceRefs {
			resolved, err := resolve(ref)
			if err != nil {
				return &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: fmt.Sprintf("assumptions[%d].evidence_refs[%d] cannot be resolved: %v", i, j, err)}
			}
			req.Assumptions[i].EvidenceRefs[j] = resolved
		}
	}
	return nil
}

func (e *decisionEngine) runReferenceEvidence(ctx context.Context, req DecisionRequest, state decisionState) ([]BaseRateEvidence, []ArtifactRef, []EvidenceProvenance, ArtifactRef, error) {
	if state.ReferenceResult != nil {
		if state.ReferenceInvocation == nil || state.ReferenceResult.InvocationID != state.ReferenceInvocation.InvocationID || state.ReferenceResult.InputHash != state.ReferenceInvocation.InputHash {
			return nil, nil, nil, ArtifactRef{}, &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: "reference evidence result identity is invalid"}
		}
		expected := ReferenceEvidenceRequest{
			SchemaVersion: ReferenceEvidenceSchemaVersion, InvocationID: state.ReferenceInvocation.InvocationID,
			DecisionID: req.DecisionID, RunID: req.RunID, TaskID: req.TaskID, Question: req.Question,
			ContractRef: req.RequestContractRef, ContractRevision: req.RequestContractRevision,
		}
		expectedHash, err := expected.ComputeInputHash()
		if err != nil || expectedHash != state.ReferenceInvocation.InputHash {
			return nil, nil, nil, ArtifactRef{}, &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: "reference evidence request changed after invocation started"}
		}
		if err := validateReferenceEvidenceResultArtifact(ctx, e.services.Store, *state.ReferenceResult); err != nil {
			return nil, nil, nil, ArtifactRef{}, &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: err.Error()}
		}
		return cloneBaseRateEvidence(state.ReferenceResult.BaseRates), append([]ArtifactRef(nil), state.ReferenceResult.Artifacts...), cloneEvidenceProvenance(state.ReferenceResult.Provenance), *state.ReferenceResult.ResultArtifactRef, nil
	}
	if state.ReferenceInvocation != nil || state.ReferenceFailure != nil {
		return nil, nil, nil, ArtifactRef{}, &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: "reference evidence invocation is incomplete and cannot be replayed safely"}
	}
	if e.services.ReferenceEvidence == nil || e.services.Store == nil {
		return nil, nil, nil, ArtifactRef{}, &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: "reference evidence requires a producer and artifact store"}
	}

	request := ReferenceEvidenceRequest{
		SchemaVersion:    ReferenceEvidenceSchemaVersion,
		InvocationID:     e.newID("reference"),
		DecisionID:       req.DecisionID,
		RunID:            req.RunID,
		TaskID:           req.TaskID,
		Question:         req.Question,
		ContractRef:      req.RequestContractRef,
		ContractRevision: req.RequestContractRevision,
	}
	inputHash, err := request.ComputeInputHash()
	if err != nil {
		return nil, nil, nil, ArtifactRef{}, &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: err.Error()}
	}
	request.InputHash = inputHash
	if err := request.Validate(); err != nil {
		return nil, nil, nil, ArtifactRef{}, &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: err.Error()}
	}
	invocation := ReferenceEvidenceInvocation{
		SchemaVersion: request.SchemaVersion, InvocationID: request.InvocationID,
		InputHash: request.InputHash, DecisionID: request.DecisionID, RunID: request.RunID,
		TaskID: request.TaskID, Question: request.Question, ContractRef: request.ContractRef,
		ContractRevision: request.ContractRevision, StartedAt: e.now(),
	}
	if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionReferenceStarted, decisionEvent{
		DecisionID: req.DecisionID, TaskID: req.TaskID, ReferenceInvocation: &invocation,
	}); err != nil {
		return nil, nil, nil, ArtifactRef{}, err
	}

	draft, producerErr := e.services.ReferenceEvidence.RunReferenceEvidence(ctx, request)
	if producerErr != nil {
		failure := &ReferenceEvidenceFailure{
			SchemaVersion: ReferenceEvidenceSchemaVersion, InvocationID: request.InvocationID,
			InputHash: request.InputHash, Reason: "reference evidence producer failed", FailedAt: e.now(),
		}
		if appendErr := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionReferenceFailed, decisionEvent{
			DecisionID: req.DecisionID, TaskID: req.TaskID, ReferenceFailure: failure,
		}); appendErr != nil {
			return nil, nil, nil, ArtifactRef{}, appendErr
		}
		return nil, nil, nil, ArtifactRef{}, &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: producerErr.Error()}
	}
	if err := draft.Validate(); err != nil {
		failure := &ReferenceEvidenceFailure{
			SchemaVersion: ReferenceEvidenceSchemaVersion, InvocationID: request.InvocationID,
			InputHash: request.InputHash, Reason: "reference evidence draft failed validation", FailedAt: e.now(),
		}
		if appendErr := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionReferenceFailed, decisionEvent{
			DecisionID: req.DecisionID, TaskID: req.TaskID, ReferenceFailure: failure,
		}); appendErr != nil {
			return nil, nil, nil, ArtifactRef{}, appendErr
		}
		return nil, nil, nil, ArtifactRef{}, &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: err.Error()}
	}

	rates := make([]BaseRateEvidence, 0, len(draft.Entries))
	artifacts := make([]ArtifactRef, 0, len(draft.Entries))
	provenance := make([]EvidenceProvenance, 0, len(draft.Entries)*2)
	for i, entry := range draft.Entries {
		artifact := ReferenceEvidenceArtifact{
			SchemaVersion: ReferenceEvidenceSchemaVersion, InvocationID: request.InvocationID,
			DecisionID: request.DecisionID, RunID: request.RunID, TaskID: request.TaskID,
			Entry: entry, PublishedAt: e.now(),
		}
		content, encodeErr := referenceEvidenceEntryBytes(artifact)
		if encodeErr != nil {
			return nil, nil, nil, ArtifactRef{}, e.referencePublicationFailure(ctx, req, request, encodeErr)
		}
		put, putErr := e.services.Store.Put(ctx, PutArtifactRequest{
			Kind: "reference_evidence", Role: "decision_reference",
			Path:        fmt.Sprintf("decisions/reference-evidence/%s/%d.json", request.InvocationID, i),
			Description: fmt.Sprintf("validated reference evidence %s entry %d", request.DecisionID, i),
			MediaType:   ReferenceEvidenceMediaType, Content: content,
			RunID: request.RunID, TaskID: request.TaskID,
		})
		if putErr != nil {
			return nil, nil, nil, ArtifactRef{}, e.referencePublicationFailure(ctx, req, request, putErr)
		}
		ref, resolveErr := e.services.Store.Resolve(ctx, put.ArtifactRef)
		if resolveErr != nil {
			return nil, nil, nil, ArtifactRef{}, e.referencePublicationFailure(ctx, req, request, resolveErr)
		}
		declaredID, idErr := referenceDeclaredSourceID(entry.Source)
		if idErr != nil {
			return nil, nil, nil, ArtifactRef{}, e.referencePublicationFailure(ctx, req, request, idErr)
		}
		rates = append(rates, BaseRateEvidence{
			ReferenceClass: entry.ReferenceClass, Metric: entry.Metric, SampleSize: entry.SampleSize,
			Distribution: entry.Distribution, Source: ref, Limitations: append([]string(nil), entry.Limitations...),
		})
		artifacts = append(artifacts, ref)
		provenance = append(provenance,
			EvidenceProvenance{SourceID: ref.ID, SourceType: EvidenceSourceArtifact, IndependenceGroup: ref.SHA256, ContentHash: ref.SHA256, RetrievedAt: e.now()},
			EvidenceProvenance{SourceID: declaredID, SourceType: EvidenceSourceDeclared, DeclaredParentSourceIDs: append([]string(nil), entry.Source.DeclaredParentSourceIDs...), RetrievedAt: e.now()},
		)
	}
	result := &ReferenceEvidenceResult{
		SchemaVersion: ReferenceEvidenceSchemaVersion, InvocationID: request.InvocationID, InputHash: request.InputHash,
		BaseRates: rates, Artifacts: artifacts, Provenance: provenance, CompletedAt: e.now(),
	}
	resultBytes, err := referenceEvidenceResultBytes(*result)
	if err != nil {
		return nil, nil, nil, ArtifactRef{}, e.referencePublicationFailure(ctx, req, request, err)
	}
	put, err := e.services.Store.Put(ctx, PutArtifactRequest{
		Kind: "reference_evidence_result", Role: "decision_reference_result",
		Path:        fmt.Sprintf("decisions/reference-evidence/%s/result.json", request.InvocationID),
		Description: fmt.Sprintf("validated reference evidence result %s", req.DecisionID),
		MediaType:   ReferenceEvidenceResultMediaType, Content: resultBytes, RunID: request.RunID, TaskID: request.TaskID,
	})
	if err != nil {
		return nil, nil, nil, ArtifactRef{}, e.referencePublicationFailure(ctx, req, request, err)
	}
	resultRef, err := e.services.Store.Resolve(ctx, put.ArtifactRef)
	if err != nil {
		return nil, nil, nil, ArtifactRef{}, e.referencePublicationFailure(ctx, req, request, err)
	}
	result.ResultArtifactRef = &resultRef
	if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionReferenceCompleted, decisionEvent{
		DecisionID: req.DecisionID, TaskID: req.TaskID, ReferenceResult: result,
	}); err != nil {
		return nil, nil, nil, ArtifactRef{}, err
	}
	return rates, artifacts, provenance, resultRef, nil
}

func validateReferenceEvidenceResultArtifact(ctx context.Context, store ArtifactStore, result ReferenceEvidenceResult) error {
	if err := validateReferenceEvidenceResult(result); err != nil {
		return err
	}
	if store == nil || result.ResultArtifactRef == nil || result.ResultArtifactRef.ID == "" {
		return fmt.Errorf("reference evidence result has no CAS envelope reference")
	}
	ref, err := store.Resolve(ctx, *result.ResultArtifactRef)
	if err != nil {
		return fmt.Errorf("resolve reference evidence result: %w", err)
	}
	if ref.MediaType != ReferenceEvidenceResultMediaType || ref.Kind != "reference_evidence_result" || ref.Role != "decision_reference_result" {
		return fmt.Errorf("reference evidence result CAS metadata is invalid")
	}
	reader, err := store.Open(ctx, ref.ID)
	if err != nil {
		return fmt.Errorf("open reference evidence result: %w", err)
	}
	defer func() { _ = reader.Close() }()
	stored, err := decodeReferenceEvidenceResult(reader)
	if err != nil {
		return err
	}
	stored.ResultArtifactRef = nil
	expected := result
	expected.ResultArtifactRef = nil
	if !reflect.DeepEqual(stored, expected) {
		return fmt.Errorf("reference evidence result CAS envelope does not match completion event")
	}
	for i, artifact := range result.Artifacts {
		if err := validateReferenceEvidenceEntryArtifact(ctx, store, artifact, result.InvocationID, result.BaseRates[i]); err != nil {
			return fmt.Errorf("reference evidence entry %d: %w", i, err)
		}
	}
	return nil
}

func validateReferenceEvidenceEntryArtifact(ctx context.Context, store ArtifactStore, ref ArtifactRef, invocationID string, rate BaseRateEvidence) error {
	if store == nil {
		return fmt.Errorf("artifact store is unavailable")
	}
	resolved, err := store.Resolve(ctx, ref)
	if err != nil {
		return fmt.Errorf("resolve artifact: %w", err)
	}
	if resolved.MediaType != ReferenceEvidenceMediaType || resolved.Kind != "reference_evidence" || resolved.Role != "decision_reference" {
		return fmt.Errorf("artifact metadata is invalid")
	}
	reader, err := store.Open(ctx, resolved.ID)
	if err != nil {
		return fmt.Errorf("open artifact: %w", err)
	}
	defer func() { _ = reader.Close() }()
	artifact, err := decodeReferenceEvidenceEntry(reader)
	if err != nil {
		return err
	}
	if artifact.InvocationID != invocationID {
		return fmt.Errorf("artifact invocation identity does not match result")
	}
	if artifact.Entry.ReferenceClass != rate.ReferenceClass || artifact.Entry.Metric != rate.Metric ||
		artifact.Entry.SampleSize != rate.SampleSize || !reflect.DeepEqual(artifact.Entry.Distribution, rate.Distribution) ||
		!reflect.DeepEqual(artifact.Entry.Limitations, rate.Limitations) {
		return fmt.Errorf("artifact entry does not match result base rate")
	}
	return nil
}

func validateReferenceEvidenceResult(result ReferenceEvidenceResult) error {
	if result.SchemaVersion != ReferenceEvidenceSchemaVersion {
		return fmt.Errorf("unsupported reference evidence result schema version %d", result.SchemaVersion)
	}
	if result.InvocationID == "" || result.InputHash == "" {
		return fmt.Errorf("reference evidence result identity is incomplete")
	}
	if result.ResultArtifactRef == nil || result.ResultArtifactRef.ID == "" {
		return fmt.Errorf("reference evidence result has no CAS envelope reference")
	}
	if len(result.BaseRates) < ReferenceEvidenceMinEntries || len(result.BaseRates) > ReferenceEvidenceMaxEntries || len(result.BaseRates) != len(result.Artifacts) {
		return fmt.Errorf("reference evidence result has inconsistent entry and artifact counts")
	}
	for i, rate := range result.BaseRates {
		if err := validateBaseRate(rate); err != nil {
			return fmt.Errorf("reference evidence result base_rates[%d]: %w", i, err)
		}
		if rate.Source.ID == "" || result.Artifacts[i].ID != rate.Source.ID {
			return fmt.Errorf("reference evidence result entry %d is not bound to its artifact", i)
		}
	}
	return nil
}

func (e *decisionEngine) referencePublicationFailure(ctx context.Context, req DecisionRequest, request ReferenceEvidenceRequest, cause error) error {
	failure := &ReferenceEvidenceFailure{
		SchemaVersion: ReferenceEvidenceSchemaVersion, InvocationID: request.InvocationID,
		InputHash: request.InputHash, Reason: "reference evidence publication failed", FailedAt: e.now(),
	}
	if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionReferenceFailed, decisionEvent{
		DecisionID: req.DecisionID, TaskID: req.TaskID, ReferenceFailure: failure,
	}); err != nil {
		return err
	}
	return &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: cause.Error()}
}

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
	if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionInvalidated, decisionEvent{DecisionID: req.DecisionID, Reason: reason}); err != nil {
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

// sealEvidence seals the packet, or reuses the already-sealed one when its
// material content is unchanged. A material change supersedes the old hash and
// makes the earlier opinions stale rather than editing them (spec §15.4).
func (e *decisionEngine) sealEvidence(ctx context.Context, req DecisionRequest, state decisionState) (DecisionEvidencePacket, ArtifactRef, error) {
	packet := DecisionEvidencePacket{
		ID:                 req.DecisionID + "-evidence",
		Question:           req.Question,
		Options:            req.Options,
		Criteria:           req.Policy.Criteria,
		Facts:              req.Facts,
		Artifacts:          req.Artifacts,
		BaseRates:          req.BaseRates,
		Assumptions:        req.Assumptions,
		Provenance:         req.Provenance,
		RequestContractRef: req.RequestContractRef,
		CreatedAt:          e.now(),
	}
	if err := packet.Validate(); err != nil {
		return DecisionEvidencePacket{}, ArtifactRef{}, fmt.Errorf("decision %s evidence: %w", req.DecisionID, err)
	}
	sealed, err := packet.Seal()
	if err != nil {
		return DecisionEvidencePacket{}, ArtifactRef{}, err
	}
	if state.Packet.Hash == sealed.Hash {
		// Already sealed on identical material; keep the durable packet so
		// CreatedAt and ID stay exactly what the log recorded.
		if req.EvidenceArtifactRef.ID != "" {
			return state.Packet, req.EvidenceArtifactRef, nil
		}
		if e.services.Store == nil {
			return state.Packet, ArtifactRef{}, nil
		}
		artifact, err := persistDecisionEvidence(ctx, e.services.Store, req, state.Packet)
		if err != nil {
			return DecisionEvidencePacket{}, ArtifactRef{}, err
		}
		if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionEvidenceSealed, decisionEvent{
			DecisionID: req.DecisionID, EvidenceHash: state.Packet.Hash, Packet: &state.Packet, EvidenceArtifact: artifact,
		}); err != nil {
			return DecisionEvidencePacket{}, ArtifactRef{}, err
		}
		return state.Packet, artifact, nil
	}
	if e.services.Store == nil {
		if decisionRequestDeclaresEvidence(req) {
			return DecisionEvidencePacket{}, ArtifactRef{}, &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: "decision evidence artifact store is unavailable"}
		}
		if state.Packet.Hash != "" {
			if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionEvidenceChanged, decisionEvent{
				DecisionID: req.DecisionID, EvidenceHash: sealed.Hash,
				Reason: evidenceChangeReason(state.Packet, sealed),
			}); err != nil {
				return DecisionEvidencePacket{}, ArtifactRef{}, err
			}
		}
		if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionEvidenceSealed, decisionEvent{
			DecisionID: req.DecisionID, EvidenceHash: sealed.Hash, Packet: &sealed,
		}); err != nil {
			return DecisionEvidencePacket{}, ArtifactRef{}, err
		}
		return sealed, ArtifactRef{}, nil
	}
	put, err := persistDecisionEvidence(ctx, e.services.Store, req, sealed)
	if err != nil {
		return DecisionEvidencePacket{}, ArtifactRef{}, err
	}
	if state.Packet.Hash != "" {
		if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionEvidenceChanged, decisionEvent{
			DecisionID: req.DecisionID, EvidenceHash: sealed.Hash,
			Reason: evidenceChangeReason(state.Packet, sealed),
		}); err != nil {
			return DecisionEvidencePacket{}, ArtifactRef{}, err
		}
	}
	if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionEvidenceSealed, decisionEvent{
		DecisionID: req.DecisionID, EvidenceHash: sealed.Hash, Packet: &sealed, EvidenceArtifact: put,
	}); err != nil {
		return DecisionEvidencePacket{}, ArtifactRef{}, err
	}
	return sealed, put, nil
}

func decisionRequestDeclaresEvidence(req DecisionRequest) bool {
	if len(req.Facts) > 0 || len(req.Artifacts) > 0 || len(req.BaseRates) > 0 || len(req.Provenance) > 0 {
		return true
	}
	for _, assumption := range req.Assumptions {
		if len(assumption.EvidenceRefs) > 0 {
			return true
		}
	}
	return false
}

func persistDecisionEvidence(ctx context.Context, store ArtifactStore, req DecisionRequest, packet DecisionEvidencePacket) (ArtifactRef, error) {
	data, err := json.Marshal(packet)
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("encode decision evidence artifact: %w", err)
	}
	put, err := store.Put(ctx, PutArtifactRequest{
		Kind: "decision_evidence", Role: "decision", Path: "decisions/evidence/" + packet.ID + ".json",
		Description: "sealed decision evidence " + packet.ID, MediaType: "application/json", Content: data,
		RunID: req.RunID, TaskID: req.TaskID,
	})
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("persisting decision evidence: %w", err)
	}
	return put.ArtifactRef, nil
}

// collectOpinions dispatches only the judges whose opinion is not already
// durable on the current evidence, so a crash never re-runs completed work.
func (e *decisionEngine) collectOpinions(
	ctx context.Context,
	req DecisionRequest,
	policy DecisionPolicy,
	packet DecisionEvidencePacket,
	weights map[string]float64,
	state decisionState,
) ([]DecisionOpinion, error) {
	existing := state.JudgesWithValidOpinion(1)
	opinions := state.OpinionsForRound(1)

	for _, judgeID := range judgeIDs(policy.IndependentJudgments) {
		if existing[judgeID] {
			continue
		}
		opinion, err := e.runJudgeWithRepair(ctx, req, policy, packet, weights, judgeID)
		if err != nil {
			return nil, err
		}
		opinions = append(opinions, opinion)
	}

	validCount := 0
	for _, opinion := range opinions {
		if opinion.Valid {
			validCount++
		}
	}
	if validCount < policy.EffectiveMinJudgments() {
		return nil, fmt.Errorf("%s: %d valid opinions, profile requires at least %d",
			ReasonDecisionInsufficientValidOpinions, validCount, policy.EffectiveMinJudgments())
	}
	sort.Slice(opinions, func(i, j int) bool { return opinions[i].JudgeID < opinions[j].JudgeID })
	return opinions, nil
}

// runJudgeWithRepair dispatches one judge, and on invalid structured output
// re-asks exactly once before rejecting the opinion. Repair is bounded so the
// runtime can never grind out a quorum by retrying (spec §14.3).
func (e *decisionEngine) runJudgeWithRepair(
	ctx context.Context,
	req DecisionRequest,
	policy DecisionPolicy,
	packet DecisionEvidencePacket,
	weights map[string]float64,
	judgeID string,
) (DecisionOpinion, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		hint := ""
		if attempt > 0 && lastErr != nil {
			hint = lastErr.Error()
		}
		judgeCtx, err := BuildJudgeContext(JudgeContextRequest{
			JudgeID:        judgeID,
			Round:          1,
			Packet:         packet,
			Isolation:      policy.EffectiveIsolation(),
			Role:           req.Role,
			ProjectContext: req.ProjectContext,
			Memory:         req.Memory,
			RepairHint:     hint,
		})
		if err != nil {
			return DecisionOpinion{}, err
		}
		if e.services.Judges == nil {
			return DecisionOpinion{}, fmt.Errorf("decision %s: no judge runner configured", req.DecisionID)
		}
		opinion, err := e.services.Judges.RunJudge(ctx, JudgeRequest{
			DecisionID: req.DecisionID, Round: 1, JudgeID: judgeID, Context: judgeCtx, Packet: packet,
		})
		if err != nil {
			lastErr = err
			continue
		}
		opinion.ID = e.newID("opinion")
		opinion.JudgeID = judgeID
		opinion.Round = 1
		if opinion.EvidenceHash == "" {
			opinion.EvidenceHash = packet.Hash
		}

		suppliedOverall := JudgeSuppliedOverall(opinion, weights)
		if err := ValidateOpinion(&opinion, packet, weights); err != nil {
			lastErr = err
			continue
		}
		opinion.Valid = true
		if suppliedOverall {
			if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionJudgeOverallIgnored, decisionEvent{
				DecisionID: req.DecisionID, EvidenceHash: packet.Hash, JudgeID: judgeID, Round: 1,
				Reason: "criteria are configured; the runtime owns the overall score",
			}); err != nil {
				return DecisionOpinion{}, err
			}
		}
		if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionOpinionSubmitted, decisionEvent{
			DecisionID: req.DecisionID, EvidenceHash: packet.Hash, JudgeID: judgeID, Round: 1, Opinion: &opinion,
		}); err != nil {
			return DecisionOpinion{}, err
		}
		return opinion, nil
	}

	reason := ReasonDecisionOpinionInvalid
	detail := reason
	if lastErr != nil {
		detail = lastErr.Error()
	}
	rejected := DecisionOpinion{
		ID: e.newID("opinion"), JudgeID: judgeID, Round: 1, EvidenceHash: packet.Hash,
		Valid: false, RejectedReason: detail,
	}
	if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionOpinionRejected, decisionEvent{
		DecisionID: req.DecisionID, EvidenceHash: packet.Hash, JudgeID: judgeID, Round: 1,
		Reason: reason, Opinion: &rejected,
	}); err != nil {
		return DecisionOpinion{}, err
	}
	return rejected, nil
}

// judgeIDs returns stable judge identities for a round.
func judgeIDs(count int) []string {
	ids := make([]string, 0, count)
	for i := 1; i <= count; i++ {
		ids = append(ids, fmt.Sprintf("judge-%d", i))
	}
	return ids
}

// buildRecord assembles the durable record. It keeps every opinion, including
// rejected ones, and the whole distribution — never only the winner (spec §27).
func (e *decisionEngine) buildRecord(
	req DecisionRequest,
	policy DecisionPolicy,
	packet DecisionEvidencePacket,
	opinions []DecisionOpinion,
	aggregates []DecisionAggregate,
	finalAggregate DecisionAggregate,
	degradations []DecisionDegradation,
) DecisionRecord {
	record := DecisionRecord{
		ID:                 req.DecisionID,
		RunID:              req.RunID,
		TaskID:             req.TaskID,
		Profile:            req.Profile,
		EvidenceHash:       packet.Hash,
		RequestContractRef: req.RequestContractRef,
		Options:            packet.Options,
		Assumptions:        packet.Assumptions,
		Opinions:           opinions,
		Aggregates:         aggregates,
		FinalOption:        finalAggregate.PreferredOption,
		FinalizationMode:   policy.EffectiveFinalization(),
		Probability:        finalAggregate.MeanProbability[finalAggregate.PreferredOption],
		NoGoOptionID:       packet.NoGoOption(),
		Degradations:       degradations,
		StopPolicySnapshot: policy.Discipline.Stop,
		CreatedAt:          e.now(),
	}
	record.AlternativesChecked = record.NoGoOptionID != ""
	record.KeyAssumptions = collectKeyAssumptions(opinions)
	record.Normalize()
	return record
}

// evidenceChangeReason explains why a sealed hash no longer matches. Without
// this an encoder upgrade is indistinguishable from evidence that genuinely
// moved, and a release touching the canonical form would look like it
// invalidated every decision on record.
func evidenceChangeReason(previous, sealed DecisionEvidencePacket) string {
	if previous.CanonicalFormOutdated() {
		return fmt.Sprintf("%s: hash %s was produced by canonical form v%d, this build uses v%d; the evidence itself may be unchanged",
			ReasonDecisionCanonicalFormChanged, previous.Hash,
			previous.EffectiveCanonicalVersion(), CanonicalFormVersion)
	}
	return fmt.Sprintf("material evidence changed; %s superseded", previous.Hash)
}

// checkPreJudgeGates runs every gate that must block before judgment starts.
// The request contract gate only applies when a contract was supplied: a team
// that has not adopted contracts keeps its old behavior (spec §11, §17, §19).
func (e *decisionEngine) checkPreJudgeGates(req DecisionRequest) error {
	if req.Contract != nil {
		if gate := CheckRequestContract(req.Contract); gate != nil {
			return gate
		}
	}
	if gate := CheckAlternatives(req.Policy.Discipline.Alternatives, req.Options); gate != nil {
		return gate
	}
	if gate := CheckOutsideView(req.Policy.OutsideView, req.BaseRates); gate != nil {
		return gate
	}
	return nil
}

// collectKeyAssumptions merges the judges' declared key assumptions, keeping
// each once and in a stable order.
func collectKeyAssumptions(opinions []DecisionOpinion) []string {
	seen := map[string]bool{}
	var out []string
	for _, opinion := range opinions {
		if !opinion.Valid {
			continue
		}
		for _, assumption := range opinion.KeyAssumptions {
			trimmed := strings.TrimSpace(assumption)
			if trimmed == "" || seen[trimmed] {
				continue
			}
			seen[trimmed] = true
			out = append(out, trimmed)
		}
	}
	sort.Strings(out)
	return out
}
