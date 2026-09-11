package team

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// The decision engine (docs/architecture/decision-runtime.md §43 Phase 1).
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
	// DispatchCount is the complete JUDGE fan-out. Capability-routed runners
	// use it to build and validate the full binding plan before invoking any
	// provider.
	DispatchCount int
	Context       JudgeContext
	Packet        DecisionEvidencePacket
	// RoutingRole selects real capability-routed execution for this judge
	// when non-nil (spec.md v2 §17; spec2.md PR-3). Unlike
	// ReferenceEvidenceRequest, JudgeRequest is never marshaled into a
	// producer-facing prompt (RunJudge sends Context.Prompt directly), so
	// this field needs no json tag to stay out of it.
	RoutingRole *agent.JudgeRolePolicy
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
	// RoutingRole selects real capability-routed execution for this
	// invocation when non-nil (plan.md Stage 8 follow-up; spec.md v2 §15).
	// json:"-" for the same reason TaskDef.DecisionOptions is: it must never
	// reach the producer's own prompt or count toward ComputeInputHash, and a
	// resumed decision must derive it solely from the durable policy
	// snapshot, never from live config.
	RoutingRole *agent.ReferenceRolePolicy `json:"-"`
}

type ReferenceEvidenceRunner interface {
	RunReferenceEvidence(context.Context, ReferenceEvidenceRequest) (ReferenceEvidenceDraft, error)
}

// DecisionServices are the runtime collaborators the engine needs. They map to
// interfaces this repo already has; the draft spec's AgentRuntime type does not
// exist (spec §4.2).
type DecisionServices struct {
	Judges               JudgeRunner
	CoordinatorFinalizer CoordinatorFinalizer
	JudgeFinalizer       NamedJudgeFinalizer
	Journal              EventJournal
	Store                ArtifactStore
	Budget               BudgetManager

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
	Attempt int
	Profile string
	Policy  DecisionPolicy

	Question string
	Options  []DecisionOption

	// RoutingHints widens a routed role's preferred-capability list for
	// this specific decision, based on Question (spec.md v2 §16, §30-31).
	// Sourced once from the team's decision.routing-hints at request
	// construction (decision_dispatch.go); applying it independently but
	// identically at both the JUDGE and REVISE construction sites is what
	// keeps REVISE resolving to the same binding JUDGE round 1 did.
	RoutingHints []agent.RoutingHint

	Facts       map[string]any
	Artifacts   []ArtifactRef
	BaseRates   []BaseRateEvidence
	Assumptions []DecisionAssumption
	Provenance  []EvidenceProvenance
	// trustedProvenance is set only by resolveDecisionEvidence after the
	// artifact store has resolved immutable metadata. Request-declared
	// provenance is intentionally never promoted into this field.
	trustedProvenance []EvidenceProvenance

	RequestContractRef      string
	RequestContractRevision uint64
	RequestContractArtifact ArtifactRef
	// AdmissionInputDigest binds a durable task-occurrence admission to this
	// immutable request snapshot. It is empty only for legacy/non-coordinator
	// engine callers that have no admission record.
	AdmissionInputDigest       string
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
	if req.Attempt < 0 {
		return nil, fmt.Errorf("decision attempt must not be negative")
	}
	if req.Attempt == 0 {
		req.Attempt = 1
	}
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
	clone.trustedProvenance = cloneEvidenceProvenance(req.trustedProvenance)
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
		state, err := projectDecision(ctx, e.services.Journal, decisionID)
		if err != nil {
			return nil, err
		}
		// A legacy finalized record is already authoritative and remains
		// readable even though pre-envelope sessions have no request snapshot.
		if state.Record != nil {
			if err := validateFinalizedDecisionState(ctx, e.services.Store, state); err != nil {
				return nil, err
			}
			return state.Record, nil
		}
		if state.EnvelopeRef.ID == "" {
			return nil, &LegacyUnfinishedDecisionError{DecisionID: decisionID}
		}
		envelope, err := loadDecisionRunEnvelope(ctx, e.services.Store, state.EnvelopeRef)
		if err != nil {
			return nil, err
		}
		req = envelope.RequestSnapshot()
		e.pending[decisionID] = req
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
		if err := validateFinalizedDecisionState(ctx, e.services.Store, state); err != nil {
			return nil, err
		}
		return state.Record, nil
	}
	anchored := state.EnvelopeRef.ID != ""
	if anchored {
		envelope, err := loadDecisionRunEnvelope(ctx, e.services.Store, state.EnvelopeRef)
		if err != nil {
			return nil, err
		}
		if err := validateDecisionRunRequestIdentity(req, envelope); err != nil {
			return nil, err
		}
		req = envelope.RequestSnapshot()
		// The envelope owns the immutable policy and identity. Do not let a
		// caller's current team configuration alter an unfinished run.
		e.pending[req.DecisionID] = req
	}
	req, err = e.prepareDecisionStart(ctx, req, &state)
	if err != nil {
		return nil, err
	}

	var policy DecisionPolicy
	var degradations []DecisionDegradation
	if anchored {
		policy = req.Policy
		degradations = append([]DecisionDegradation(nil), state.Degradations...)
	} else {
		policy, degradations, err = e.admitBudget(ctx, req, state)
		if err != nil {
			return nil, err
		}
	}
	// Finalization authority is part of policy admission. Reject an admitted
	// policy that cannot be finalized before any option, reference, or model
	// capable stage can observe or persist work for this decision.
	if err := validateFinalizationPolicy(policy, e.services); err != nil {
		return nil, fmt.Errorf("decision %s finalization preflight: %w", req.DecisionID, err)
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

	packet, packetArtifact, err := e.sealDecisionEvidence(ctx, req, state)
	if err != nil {
		return nil, err
	}
	// Re-project so a fresh seal's stale-hash bookkeeping is visible to the
	// stages that reuse durable results.
	if state.Packet.Hash != packet.Hash {
		state.Packet = packet
	}
	// The envelope is the durable admission boundary for judge execution. The
	// artifact must reach the content-addressed store before its anchor event,
	// and the anchor must reach the journal before the first judge dispatch.
	// A nil store is retained as the legacy compatibility path used by older
	// in-memory callers; such unfinished runs can only resume with their request
	// supplied again.
	if err := e.anchorDecisionRun(ctx, &req, &state, policy, packet, packetArtifact); err != nil {
		return nil, err
	}

	weights, err := normalizedWeights(packet.Criteria)
	if err != nil {
		return nil, fmt.Errorf("decision %s: %w", req.DecisionID, err)
	}

	opinions, err := e.collectOpinions(ctx, req, policy, packet, weights, state)
	if err != nil {
		return nil, err
	}

	aggregate, aggregateExists := state.Aggregates[1]
	if !aggregateExists || aggregate.EvidenceHash != packet.Hash {
		aggregate, err = Aggregate(packet, opinions, policy.EffectiveAggregation(), 1)
		if err != nil {
			return nil, fmt.Errorf("decision %s: %w", req.DecisionID, err)
		}
		aggregate.ID = e.newID("aggregate")
		event := decisionEventFor(req, "aggregate", packet.Hash, "1")
		event.EvidenceHash, event.Round, event.Aggregate = packet.Hash, 1, &aggregate
		if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionAggregateComputed, event); err != nil {
			return nil, err
		}
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
	record.JudgeDiversity = judgeDiversitySummary(opinions)
	record.ChallengeDiversity = challengeDiversitySummary(challenges)
	record.Premortem = premortem
	record.FalsificationConditions = PremortemFalsifications(premortem, challenges)

	finalization, finalizationRef, err := e.runFinalization(ctx, req, policy, packet, aggregates, challenges, revisions, state)
	if err != nil {
		return nil, fmt.Errorf("decision %s: %w", req.DecisionID, err)
	}
	record.FinalOption = finalization.OptionID
	record.Probability = finalAggregate.MeanProbability[finalization.OptionID]
	record.FinalizationIdentity = finalization.Identity
	record.FinalizationReason = finalization.Reason
	record.FinalizationResultRef = &finalizationRef
	record.FinalizationStale = finalization.Stale
	record.FinalizationWarnings = append([]string(nil), finalization.Warnings...)
	record.FinalizationOutcome = finalization.Outcome

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
	event := decisionEventFor(req, "finalized", packet.Hash)
	event.EvidenceHash, event.Record = packet.Hash, &record
	event.Question, event.ForecastRequired, event.RecordRef = req.Question, policy.Forecast.Required, recordRef
	if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionFinalized, event); err != nil {
		return nil, err
	}
	finalizedState, err := projectDecision(ctx, e.services.Journal, req.DecisionID)
	if err != nil {
		return nil, fmt.Errorf("decision %s: reproject finalized state: %w", req.DecisionID, err)
	}
	if err := validateFinalizedDecisionState(ctx, e.services.Store, finalizedState); err != nil {
		return nil, fmt.Errorf("decision %s: finalized state integrity: %w", req.DecisionID, err)
	}
	// Listing the decision is the last step: a decision that failed a gate is
	// not addressable for resolution, because it was never made.
	if e.services.Index != nil {
		e.services.Index.SetEventJournal(e.services.Journal)
		e.services.Index.SetArtifactStore(e.services.Store)
		entry := IndexEntryFor(record, req.Question, policy.Forecast.Required, recordRef)
		if err := e.services.Index.Append(entry); err != nil {
			return nil, fmt.Errorf("decision %s: %w", req.DecisionID, err)
		}
	}
	return &record, nil
}

func (e *decisionEngine) prepareDecisionStart(ctx context.Context, req DecisionRequest, state *decisionState) (DecisionRequest, error) {
	if req.EvidenceArtifactRef.ID == "" {
		req.EvidenceArtifactRef = state.EvidenceArtifact
	}
	if err := validateReferenceRecoveryState(*state); err != nil {
		return req, err
	}
	var err error
	req, err = e.prepareRequestContract(ctx, req, *state)
	if err != nil {
		return req, err
	}
	if req.Contract != nil && state.Profile == "" {
		event := decisionEventFor(req, "request_contract", req.RequestContractRef, fmt.Sprint(req.RequestContractRevision))
		event.ContractRef, event.ContractRevision, event.ContractArtifact = req.RequestContractRef, req.RequestContractRevision, req.RequestContractArtifact
		if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventRequestContractCommitted, event); err != nil {
			return req, err
		}
	}
	if state.Profile == "" {
		event := decisionEventFor(req, "started")
		event.Profile = req.Profile
		if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionStarted, event); err != nil {
			return req, err
		}
	}
	return req, nil
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
	var lastOperationalErr error
	var lastValidationErr error
	for attempt := 0; attempt < 2; attempt++ {
		hint := ""
		if attempt > 0 {
			switch {
			case lastValidationErr != nil:
				hint = lastValidationErr.Error()
			case lastOperationalErr != nil:
				hint = lastOperationalErr.Error()
			}
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
			DecisionID: req.DecisionID, Round: 1, JudgeID: judgeID, DispatchCount: policy.IndependentJudgments, Context: judgeCtx, Packet: packet,
			RoutingRole: hintedJudgeRole(req.Policy.JudgeRole, req.RoutingHints, req.Question),
		})
		if err != nil {
			lastOperationalErr = err
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
			lastValidationErr = err
			continue
		}
		opinion.Valid = true
		if suppliedOverall {
			event := decisionEventFor(req, "judge_overall_ignored", packet.Hash, judgeID, "1")
			event.EvidenceHash, event.JudgeID, event.Round, event.Reason = packet.Hash, judgeID, 1, "criteria are configured; the runtime owns the overall score"
			if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionJudgeOverallIgnored, event); err != nil {
				return DecisionOpinion{}, err
			}
		}
		event := decisionEventFor(req, "opinion", packet.Hash, judgeID, "1")
		event.EvidenceHash, event.JudgeID, event.Round, event.Opinion = packet.Hash, judgeID, 1, &opinion
		if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionOpinionSubmitted, event); err != nil {
			return DecisionOpinion{}, err
		}
		return opinion, nil
	}

	if lastValidationErr == nil {
		// A provider or execution failure leaves the judge incomplete. It is not
		// a rejected opinion: no structured opinion exists to audit, and resume
		// must be able to dispatch this judge again.
		if lastOperationalErr != nil {
			return DecisionOpinion{}, fmt.Errorf("decision %s judge %s execution: %w", req.DecisionID, judgeID, lastOperationalErr)
		}
		return DecisionOpinion{}, fmt.Errorf("decision %s judge %s did not produce an opinion", req.DecisionID, judgeID)
	}

	reason := ReasonDecisionOpinionInvalid
	detail := lastValidationErr.Error()
	rejected := DecisionOpinion{
		ID: e.newID("opinion"), JudgeID: judgeID, Round: 1, EvidenceHash: packet.Hash,
		Valid: false, RejectedReason: detail,
	}
	event := decisionEventFor(req, "opinion_rejected", packet.Hash, judgeID, "1")
	event.EvidenceHash, event.JudgeID, event.Round, event.Reason, event.Opinion = packet.Hash, judgeID, 1, reason, &rejected
	if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionOpinionRejected, event); err != nil {
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
