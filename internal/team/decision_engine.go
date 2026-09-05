package team

import (
	"context"
	"fmt"
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

// DecisionServices are the runtime collaborators the engine needs. They map to
// interfaces this repo already has; the draft spec's AgentRuntime type does not
// exist (spec §4.2).
type DecisionServices struct {
	Judges  JudgeRunner
	Journal EventJournal
	Store   ArtifactStore
	Budget  BudgetManager

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

	RequestContractRef string

	// Role, ProjectContext and Memory are the non-evidence context sources a
	// judge may receive under strict isolation (spec §16).
	Role           string
	ProjectContext string
	Memory         string

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
	if err := req.Policy.Validate(); err != nil {
		return nil, fmt.Errorf("decision policy: %w", err)
	}
	if req.DecisionID == "" {
		req.DecisionID = e.newID("decision")
	}
	e.pending[req.DecisionID] = req
	return e.run(ctx, req)
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

	if state.Profile == "" {
		if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionStarted, decisionEvent{
			DecisionID: req.DecisionID, Profile: req.Profile,
		}); err != nil {
			return nil, err
		}
	}

	packet, err := e.sealEvidence(ctx, req, state)
	if err != nil {
		return nil, err
	}

	policy, degradations, err := e.admitBudget(ctx, req, state)
	if err != nil {
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

	record := e.buildRecord(req, policy, packet, opinions, aggregate, append(state.Degradations, degradations...))
	if _, err := persistDecisionRecord(ctx, e.services.Store, record); err != nil {
		return nil, err
	}
	if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionFinalized, decisionEvent{
		DecisionID: req.DecisionID, EvidenceHash: packet.Hash, Record: &record,
	}); err != nil {
		return nil, err
	}
	return &record, nil
}

// sealEvidence seals the packet, or reuses the already-sealed one when its
// material content is unchanged. A material change supersedes the old hash and
// makes the earlier opinions stale rather than editing them (spec §15.4).
func (e *decisionEngine) sealEvidence(ctx context.Context, req DecisionRequest, state decisionState) (DecisionEvidencePacket, error) {
	packet := DecisionEvidencePacket{
		ID:                 req.DecisionID + "-evidence",
		Question:           req.Question,
		Options:            req.Options,
		Criteria:           req.Policy.Criteria,
		Facts:              req.Facts,
		Artifacts:          req.Artifacts,
		BaseRates:          req.BaseRates,
		Assumptions:        req.Assumptions,
		RequestContractRef: req.RequestContractRef,
		CreatedAt:          e.now(),
	}
	if err := packet.Validate(); err != nil {
		return DecisionEvidencePacket{}, fmt.Errorf("decision %s evidence: %w", req.DecisionID, err)
	}
	sealed, err := packet.Seal()
	if err != nil {
		return DecisionEvidencePacket{}, err
	}
	if state.Packet.Hash == sealed.Hash {
		// Already sealed on identical material; keep the durable packet so
		// CreatedAt and ID stay exactly what the log recorded.
		return state.Packet, nil
	}
	if state.Packet.Hash != "" {
		if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionEvidenceChanged, decisionEvent{
			DecisionID: req.DecisionID, EvidenceHash: sealed.Hash,
			Reason: fmt.Sprintf("material evidence changed; %s superseded", state.Packet.Hash),
		}); err != nil {
			return DecisionEvidencePacket{}, err
		}
	}
	if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionEvidenceSealed, decisionEvent{
		DecisionID: req.DecisionID, EvidenceHash: sealed.Hash, Packet: &sealed,
	}); err != nil {
		return DecisionEvidencePacket{}, err
	}
	return sealed, nil
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
	aggregate DecisionAggregate,
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
		Aggregates:         []DecisionAggregate{aggregate},
		FinalOption:        aggregate.PreferredOption,
		FinalizationMode:   policy.EffectiveFinalization(),
		Probability:        aggregate.MeanProbability[aggregate.PreferredOption],
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
