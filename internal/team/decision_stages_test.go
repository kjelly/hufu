package team

import (
	"context"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func phase2Policy() DecisionPolicy {
	policy := enginePolicy(3)
	policy.MaxRounds = 2
	policy.Challenge = ChallengePolicy{Enabled: true, Count: 1}
	policy.Revision = RevisionPolicy{Enabled: true}
	policy.Premortem = PremortemPolicy{Enabled: true}
	return policy
}

func spreadRunner() *recordingRunner {
	return newRecordingRunner(func(judgeID string, _ int) (DecisionOpinion, error) {
		switch judgeID {
		case "judge-1":
			return scoredOpinion(9, 2, "migrate", 0.9), nil
		case "judge-2":
			return scoredOpinion(6, 5, "migrate", 0.6), nil
		default:
			return scoredOpinion(2, 8, "wait", 0.3), nil
		}
	})
}

// Gates block before JUDGE: no judge is dispatched at all (spec §19, §17).
func TestPreJudgeGatesBlockBeforeDispatch(t *testing.T) {
	tests := []struct {
		name       string
		mutatePol  func(*DecisionPolicy)
		mutateReq  func(*DecisionRequest)
		wantReason string
	}{
		{
			name:       "missing no-go option",
			mutatePol:  func(p *DecisionPolicy) { p.Discipline.Alternatives.RequireNoActionOption = true },
			mutateReq:  func(r *DecisionRequest) { r.Options = r.Options[:1] },
			wantReason: ReasonDecisionNoNoGoOption,
		},
		{
			name:       "missing outside view",
			mutatePol:  func(p *DecisionPolicy) { p.OutsideView.Required = true },
			wantReason: ReasonDecisionOutsideViewMissing,
		},
		{
			name:      "missing objective",
			mutatePol: func(*DecisionPolicy) {},
			mutateReq: func(r *DecisionRequest) {
				r.Contract = &RequestContract{ID: "contract-1"}
			},
			wantReason: ReasonDecisionMissingObjective,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			journal := &memoryJournal{}
			runner := spreadRunner()
			policy := enginePolicy(3)
			tt.mutatePol(&policy)
			req := engineRequest(policy)
			if tt.mutateReq != nil {
				tt.mutateReq(&req)
			}
			engine := newTestEngine(journal, runner, nil)

			_, err := engine.Run(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tt.wantReason) {
				t.Fatalf("Run = %v, want %s", err, tt.wantReason)
			}
			if len(runner.dispatched()) != 0 {
				t.Fatalf("dispatched %v judges despite a blocking gate", runner.dispatched())
			}
		})
	}
}

// Supplying the required evidence lets the same decision proceed.
func TestOutsideViewSatisfiedProceeds(t *testing.T) {
	journal := &memoryJournal{}
	policy := enginePolicy(3)
	policy.OutsideView.Required = true
	req := engineRequest(policy)
	req.BaseRates = []BaseRateEvidence{usableBaseRate()}

	engine := newTestEngine(journal, spreadRunner(), nil)
	record, err := engine.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if record.FinalOption == "" {
		t.Fatal("decision produced no final option")
	}
}

// Challenge runs only after aggregation and sees anonymized opinions, never
// judge identity (spec §23).
func TestChallengeRunsAfterAggregateAndIsAnonymized(t *testing.T) {
	journal := &memoryJournal{}
	challenger := &stubChallenger{}
	engine := newTestEngineWithStages(journal, spreadRunner(), nil, DecisionServices{
		Challengers: challenger,
		Revisions:   &stubReviser{},
		Premortems:  &stubPremortem{},
	})

	record, err := engine.Run(context.Background(), engineRequest(phase2Policy()))
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if len(record.Challenges) != 1 {
		t.Fatalf("challenges = %d, want 1", len(record.Challenges))
	}

	types := journal.typesOf()
	aggregateAt, challengeAt := -1, -1
	for i, name := range types {
		if name == agent.EventDecisionAggregateComputed && aggregateAt == -1 {
			aggregateAt = i
		}
		if name == agent.EventDecisionChallengeSubmitted && challengeAt == -1 {
			challengeAt = i
		}
	}
	if aggregateAt == -1 || challengeAt == -1 || challengeAt < aggregateAt {
		t.Fatalf("challenge did not follow aggregation; event order = %v", types)
	}

	prompt := challenger.lastPrompt()
	for _, judgeID := range []string{"judge-1", "judge-2", "judge-3"} {
		if strings.Contains(prompt, judgeID) {
			t.Errorf("challenge prompt revealed judge identity %q", judgeID)
		}
	}
	if !strings.Contains(prompt, "judge-a") {
		t.Fatalf("challenge prompt did not use positional aliases:\n%s", prompt)
	}
	if !strings.Contains(prompt, "cannot change the sealed evidence") {
		t.Fatal("challenge prompt did not state that sealed evidence is immutable")
	}
}

// A trigger that does not fire records the observed dispersion rather than
// leaving the skip invisible (spec §22).
func TestChallengeSkipIsRecorded(t *testing.T) {
	journal := &memoryJournal{}
	// All judges agree, so dispersion is zero.
	runner := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		return scoredOpinion(8, 3, "migrate", 0.8), nil
	})
	policy := phase2Policy()
	policy.Challenge.Trigger = &ChallengeTrigger{DispersionAbove: 1.5}

	challenger := &stubChallenger{}
	engine := newTestEngineWithStages(journal, runner, nil, DecisionServices{
		Challengers: challenger, Revisions: &stubReviser{}, Premortems: &stubPremortem{},
	})
	record, err := engine.Run(context.Background(), engineRequest(policy))
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if len(record.Challenges) != 0 {
		t.Fatalf("challenge ran below the dispersion threshold: %#v", record.Challenges)
	}
	if journal.count(agent.EventDecisionChallengeSkipped) != 1 {
		t.Fatalf("skip events = %d, want 1", journal.count(agent.EventDecisionChallengeSkipped))
	}
	if len(challenger.prompts) != 0 {
		t.Fatal("challenger was dispatched despite the trigger not firing")
	}
}

// Revision is one independent second look: the judge sees the aggregate and the
// challenge, never another judge's individual view, and the round is bounded.
func TestRevisionIsBoundedAndIndependent(t *testing.T) {
	journal := &memoryJournal{}
	reviser := &stubReviser{respond: func(judgeID string) (DecisionRevision, error) {
		if judgeID == "judge-3" {
			return DecisionRevision{
				Changed: true, Reason: "the countercase moved me",
				RevisedScores: []OptionScore{
					{OptionID: "migrate", Criteria: map[string]float64{"cost": 7, "risk": 7}},
					{OptionID: "wait", Criteria: map[string]float64{"cost": 3, "risk": 3}},
				},
				RevisedProbability: 0.6,
			}, nil
		}
		return DecisionRevision{Changed: false}, nil
	}}
	engine := newTestEngineWithStages(journal, spreadRunner(), nil, DecisionServices{
		Challengers: &stubChallenger{}, Revisions: reviser, Premortems: &stubPremortem{},
	})

	record, err := engine.Run(context.Background(), engineRequest(phase2Policy()))
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if len(record.Revisions) != 3 {
		t.Fatalf("revisions = %d, want one per valid judge", len(record.Revisions))
	}
	if len(record.Aggregates) != 2 {
		t.Fatalf("aggregates = %d, want round 1 and round 2", len(record.Aggregates))
	}
	if record.Aggregates[1].Round != 2 {
		t.Fatalf("second aggregate round = %d, want 2", record.Aggregates[1].Round)
	}
	// Round 2 must be strictly bounded: no third round exists.
	for _, aggregate := range record.Aggregates {
		if aggregate.Round > 2 {
			t.Fatalf("round %d exists; MaxRounds is 2", aggregate.Round)
		}
	}

	reviser.mu.Lock()
	defer reviser.mu.Unlock()
	for judgeID, prompt := range reviser.prompts {
		for _, other := range []string{"judge-1", "judge-2", "judge-3"} {
			if other == judgeID {
				continue
			}
			if strings.Contains(prompt, "Your round 1 scores") && strings.Contains(prompt, other+" prefers") {
				t.Errorf("%s revision prompt exposed %s's individual view", judgeID, other)
			}
		}
		if !strings.Contains(prompt, "not a negotiation") {
			t.Errorf("%s revision prompt did not frame the round as independent", judgeID)
		}
	}
}

// An unchanged revision carries the original scores forward rather than making
// the judge retype them.
func TestUnchangedRevisionPreservesScores(t *testing.T) {
	packet := aggregatePacket(t)
	weights, _ := normalizedWeights(packet.Criteria)
	original := opinion("judge-1", packet.Hash, [2]float64{8, 6}, [2]float64{4, 4}, "a", 0.7)
	if err := ValidateOpinion(&original, packet, weights); err != nil {
		t.Fatal(err)
	}
	original.Valid = true

	revision := DecisionRevision{Changed: false}
	if err := ValidateRevision(&revision, packet, weights, original); err != nil {
		t.Fatal(err)
	}
	if len(revision.RevisedScores) != len(original.OptionScores) {
		t.Fatalf("unchanged revision dropped scores: %#v", revision.RevisedScores)
	}
	if revision.RevisedProbability != original.SuccessProbability {
		t.Fatalf("unchanged revision changed probability to %v", revision.RevisedProbability)
	}

	revised := RevisedOpinions([]DecisionOpinion{original}, []DecisionRevision{revision})
	if len(revised) != 1 || revised[0].Round != 2 || revised[0].OptionScores[0].Overall != original.OptionScores[0].Overall {
		t.Fatalf("round 2 projection = %#v", revised)
	}
}

func TestValidateRevisionRejectsBadOutput(t *testing.T) {
	packet := aggregatePacket(t)
	weights, _ := normalizedWeights(packet.Criteria)
	original := opinion("judge-1", packet.Hash, [2]float64{8, 6}, [2]float64{4, 4}, "a", 0.7)
	_ = ValidateOpinion(&original, packet, weights)
	original.Valid = true

	bad := DecisionRevision{Changed: true, RevisedProbability: 3}
	if err := ValidateRevision(&bad, packet, weights, original); err == nil {
		t.Fatal("out-of-range revised probability accepted")
	}

	incomplete := DecisionRevision{
		Changed:            true,
		RevisedProbability: 0.5,
		RevisedScores:      []OptionScore{{OptionID: "a", Criteria: map[string]float64{"cost": 5, "risk": 5}}},
	}
	if err := ValidateRevision(&incomplete, packet, weights, original); err == nil ||
		!strings.Contains(err.Error(), "was not scored") {
		t.Fatalf("incomplete revision = %v, want a not-scored rejection", err)
	}
}

// A required premortem that produces nothing blocks before the decision is
// finalized (spec §24, test matrix H).
func TestRequiredPremortemBlocks(t *testing.T) {
	policy := phase2Policy()
	policy.Premortem.RequiredBeforeCommit = true

	t.Run("no runner configured", func(t *testing.T) {
		journal := &memoryJournal{}
		engine := newTestEngineWithStages(journal, spreadRunner(), nil, DecisionServices{
			Challengers: &stubChallenger{}, Revisions: &stubReviser{},
		})
		_, err := engine.Run(context.Background(), engineRequest(policy))
		if err == nil || !strings.Contains(err.Error(), ReasonDecisionPremortemRequired) {
			t.Fatalf("Run = %v, want %s", err, ReasonDecisionPremortemRequired)
		}
	})

	t.Run("runner produces no failure modes", func(t *testing.T) {
		journal := &memoryJournal{}
		engine := newTestEngineWithStages(journal, spreadRunner(), nil, DecisionServices{
			Challengers: &stubChallenger{}, Revisions: &stubReviser{},
			Premortems: &stubPremortem{respond: func() (PremortemResult, error) {
				return PremortemResult{}, nil
			}},
		})
		_, err := engine.Run(context.Background(), engineRequest(policy))
		if err == nil || !strings.Contains(err.Error(), ReasonDecisionPremortemRequired) {
			t.Fatalf("Run = %v, want %s", err, ReasonDecisionPremortemRequired)
		}
	})
}

// The premortem's early warning signals become the decision's falsification
// conditions, which is what a required forecast needs (spec §24, §40).
func TestPremortemFeedsFalsificationConditions(t *testing.T) {
	journal := &memoryJournal{}
	policy := phase2Policy()
	policy.Forecast.Required = true
	engine := newTestEngineWithStages(journal, spreadRunner(), nil, DecisionServices{
		Challengers: &stubChallenger{}, Revisions: &stubReviser{}, Premortems: &stubPremortem{},
	})

	record, err := engine.Run(context.Background(), engineRequest(policy))
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if len(record.FalsificationConditions) == 0 {
		t.Fatal("no falsification conditions recorded")
	}
	if record.Premortem == nil || len(record.Premortem.FailureModes) == 0 {
		t.Fatal("premortem result was not attached to the record")
	}
	if journal.count(agent.EventDecisionPremortemSubmitted) != 1 {
		t.Fatalf("premortem events = %d, want 1", journal.count(agent.EventDecisionPremortemSubmitted))
	}
}

// A required forecast with no usable probability blocks the decision.
func TestRequiredForecastBlocksWithoutProbability(t *testing.T) {
	journal := &memoryJournal{}
	policy := phase2Policy()
	policy.Forecast.Required = true
	policy.Premortem.Enabled = false
	// Judges give no probability at all.
	runner := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		o := scoredOpinion(8, 3, "migrate", 0)
		return o, nil
	})
	engine := newTestEngineWithStages(journal, runner, nil, DecisionServices{
		Challengers: &stubChallenger{}, Revisions: &stubReviser{},
	})
	_, err := engine.Run(context.Background(), engineRequest(policy))
	if err == nil || !strings.Contains(err.Error(), "forecast is required") {
		t.Fatalf("Run = %v, want a forecast rejection", err)
	}
}

// Independence counts land on the record and shared origins warn (spec §28.2).
func TestRecordCarriesIndependenceCounts(t *testing.T) {
	journal := &memoryJournal{}
	policy := enginePolicy(2)
	req := engineRequest(policy)
	req.Artifacts = []ArtifactRef{
		{ID: "art-1", SHA256: "h1", MediaType: "application/json"},
		{ID: "art-2", SHA256: "h1", MediaType: "application/json"},
	}
	req.Policy.Discipline.Evidence.WarnSharedOrigin = true

	engine := newTestEngine(journal, spreadRunner(), nil)
	record, err := engine.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if record.SourceCount != 2 || record.IndependenceGroupCount != 1 {
		t.Fatalf("record independence = %d groups from %d sources, want 1 from 2",
			record.IndependenceGroupCount, record.SourceCount)
	}
	if len(record.SharedOriginWarnings) != 1 {
		t.Fatalf("shared-origin warnings = %v, want one", record.SharedOriginWarnings)
	}
	if journal.count(agent.EventDecisionEvidenceSharedOrigin) != 1 {
		t.Fatal("shared origin was not reported as an event")
	}
}

// V1 keeps the independence requirement advisory unless a team opts in.
func TestIndependenceRequirementIsOptIn(t *testing.T) {
	req := engineRequest(enginePolicy(2))
	req.Artifacts = []ArtifactRef{{ID: "art-1", SHA256: "h1"}, {ID: "art-2", SHA256: "h1"}}

	if _, err := newTestEngine(&memoryJournal{}, spreadRunner(), nil).Run(context.Background(), req); err != nil {
		t.Fatalf("advisory default blocked the decision: %v", err)
	}

	req.Policy.Discipline.Evidence.RequiredIndependentGroups = 2
	_, err := newTestEngine(&memoryJournal{}, spreadRunner(), nil).Run(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "independent groups") {
		t.Fatalf("Run = %v, want the opted-in requirement to block", err)
	}
}

// Stages are resumable: a durable challenge or revision is not re-run.
func TestPhase2StagesResumeWithoutRerunning(t *testing.T) {
	journal := &memoryJournal{}
	challenger := &stubChallenger{}
	reviser := &stubReviser{}
	req := engineRequest(phase2Policy())
	req.DecisionID = "decision-stages"

	engine := newTestEngineWithStages(journal, spreadRunner(), nil, DecisionServices{
		Challengers: challenger, Revisions: reviser, Premortems: &stubPremortem{},
	})
	if _, err := engine.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	challengesFirst := len(challenger.prompts)

	// A fresh engine over the same journal re-derives state from events. The
	// decision is finalized, so nothing is dispatched again.
	resumed := newTestEngineWithStages(journal, spreadRunner(), nil, DecisionServices{
		Challengers: challenger, Revisions: reviser, Premortems: &stubPremortem{},
	})
	if _, err := resumed.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(challenger.prompts) != challengesFirst {
		t.Fatalf("challenger ran again on resume: %d then %d", challengesFirst, len(challenger.prompts))
	}
}
