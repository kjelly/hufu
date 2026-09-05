package team

import (
	"context"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// stubProposer stands in for the proposal stage.
type stubProposer struct {
	calls   int
	respond func() ([]DecisionOption, error)
}

func (p *stubProposer) ProposeOptions(context.Context, OptionProposalRequest) ([]DecisionOption, error) {
	p.calls++
	if p.respond != nil {
		return p.respond()
	}
	return []DecisionOption{
		{ID: "Migrate Now", Kind: OptionExecute, Title: "Migrate now"},
		{ID: "reduce", Kind: OptionReduceScope, Title: "Migrate one topic first"},
	}, nil
}

func proposalPolicy() DecisionPolicy {
	policy := enginePolicy(2)
	policy.OptionProposal = OptionProposalPolicy{Enabled: true, MaxOptions: 5}
	policy.Discipline.Alternatives = AlternativesPolicy{RequireNoActionOption: true}
	return policy
}

func TestSlugifyOptionID(t *testing.T) {
	tests := map[string]string{
		"Migrate Now":           "migrate-now",
		"  REDUCE_scope  ":      "reduce-scope",
		"a//b":                  "ab",
		"---":                   "",
		"":                      "",
		"Ship It 2.0":           "ship-it-20",
		strings.Repeat("x", 80): strings.Repeat("x", 48),
	}
	for in, want := range tests {
		if got := slugifyOptionID(in); got != want {
			t.Errorf("slugifyOptionID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeProposedOptions(t *testing.T) {
	proposed := []DecisionOption{
		{ID: "Migrate Now", Kind: OptionExecute, Title: "Migrate now"},
		{ID: "migrate now", Kind: OptionExecute},         // duplicate slug
		{ID: "  ", Kind: OptionExecute},                  // unusable id
		{ID: "odd", Kind: DecisionOptionKind("wishful")}, // unknown kind
		{ID: "long", Kind: OptionDefer, Title: strings.Repeat("t", 500)},
	}
	options := NormalizeProposedOptions(proposed, 5)

	if len(options) != 3 {
		t.Fatalf("options = %#v, want the duplicate and the unusable id dropped", options)
	}
	if options[0].ID != "migrate-now" || options[0].Origin != OptionOriginProposed {
		t.Fatalf("options[0] = %#v", options[0])
	}
	if options[1].Kind != OptionCustom {
		t.Fatalf("an unknown kind became %q, want custom", options[1].Kind)
	}
	if len([]rune(options[2].Title)) != maxProposalTitleRunes {
		t.Fatalf("title was not capped: %d runes", len([]rune(options[2].Title)))
	}

	capped := NormalizeProposedOptions(proposed, 1)
	if len(capped) != 1 {
		t.Fatalf("capped = %#v, want max-options honored", capped)
	}
}

// This is the property that keeps the no-go gate meaningful once options can
// be proposed: the runtime puts "do nothing" on the table itself rather than
// trusting the proposer to remember (spec §19.1).
func TestEnsureRequiredAlternativesInjectsWhatThePolicyRequires(t *testing.T) {
	proposed := []DecisionOption{{ID: "migrate", Kind: OptionExecute, Origin: OptionOriginProposed}}

	options, injected := EnsureRequiredAlternatives(proposed, AlternativesPolicy{
		RequireNoActionOption: true, RequireInfoOption: true,
	})
	if len(options) != 3 || len(injected) != 2 {
		t.Fatalf("options = %#v, injected = %v", options, injected)
	}

	var sawNoGo, sawInfo bool
	for _, option := range options {
		if option.Origin == OptionOriginRuntime && option.Kind.IsNoGo() {
			sawNoGo = true
		}
		if option.Origin == OptionOriginRuntime && option.Kind == OptionRequestInfo {
			sawInfo = true
		}
	}
	if !sawNoGo || !sawInfo {
		t.Fatalf("required alternatives were not injected: %#v", options)
	}
	// Injected options must be distinguishable from proposed ones.
	for _, option := range options {
		if option.EffectiveOrigin() == "" {
			t.Fatalf("option %q has no origin", option.ID)
		}
	}
	// Ordering is by ID so the evidence hash does not depend on injection order.
	for i := 1; i < len(options); i++ {
		if options[i-1].ID > options[i].ID {
			t.Fatalf("options are not in ascending ID order: %#v", options)
		}
	}
}

func TestEnsureRequiredAlternativesLeavesSatisfiedSetsAlone(t *testing.T) {
	proposed := []DecisionOption{
		{ID: "migrate", Kind: OptionExecute},
		{ID: "wait", Kind: OptionDefer},
	}
	options, injected := EnsureRequiredAlternatives(proposed, AlternativesPolicy{RequireNoActionOption: true})
	if len(injected) != 0 || len(options) != 2 {
		t.Fatalf("injected %v into an already-satisfied set", injected)
	}

	// No policy requirement means no injection at all.
	options, injected = EnsureRequiredAlternatives(proposed[:1], AlternativesPolicy{})
	if len(injected) != 0 || len(options) != 1 {
		t.Fatalf("injected %v with no policy requiring it", injected)
	}
}

// A task that declared its own options is authoritative; the proposal stage
// must not second-guess a written contract.
func TestDeclaredOptionsSkipProposal(t *testing.T) {
	journal := &memoryJournal{}
	proposer := &stubProposer{}
	engine := newTestEngineWithStages(journal, spreadRunner(), nil, DecisionServices{Proposer: proposer})

	req := engineRequest(proposalPolicy())
	record, err := engine.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if proposer.calls != 0 {
		t.Fatalf("proposer ran %d times for a task with declared options", proposer.calls)
	}
	if journal.count(agent.EventDecisionOptionsProposed) != 0 {
		t.Fatal("a proposal event was emitted for declared options")
	}
	for _, option := range record.Options {
		if option.Origin != OptionOriginDeclared {
			t.Fatalf("declared option %q has origin %q", option.ID, option.Origin)
		}
	}
}

// With no declared options, the proposal stage produces them and the runtime
// completes the set so the alternatives gate passes on substance.
func TestProposedOptionsReachTheDecision(t *testing.T) {
	journal := &memoryJournal{}
	proposer := &stubProposer{}
	engine := newTestEngineWithStages(journal, newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		return DecisionOpinion{
			OptionScores: []OptionScore{
				{OptionID: "migrate-now", Criteria: map[string]float64{"cost": 8, "risk": 8}},
				{OptionID: "reduce", Criteria: map[string]float64{"cost": 5, "risk": 5}},
				{OptionID: "runtime-defer", Criteria: map[string]float64{"cost": 2, "risk": 2}},
			},
			PreferredOption: "migrate-now", SuccessProbability: 0.7,
		}, nil
	}), nil, DecisionServices{Proposer: proposer})

	req := engineRequest(proposalPolicy())
	req.Options = nil

	record, err := engine.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if proposer.calls != 1 {
		t.Fatalf("proposer ran %d times, want once", proposer.calls)
	}
	if len(record.Options) != 3 {
		t.Fatalf("options = %#v, want two proposed plus the injected no-go", record.Options)
	}
	if record.NoGoOptionID == "" || !record.AlternativesChecked {
		t.Fatalf("the no-go alternative was not recorded: %#v", record.NoGoOptionID)
	}
	if journal.count(agent.EventDecisionOptionsProposed) != 1 {
		t.Fatalf("proposal events = %d, want 1", journal.count(agent.EventDecisionOptionsProposed))
	}

	var runtimeInjected int
	for _, option := range record.Options {
		if option.Origin == OptionOriginRuntime {
			runtimeInjected++
		}
	}
	if runtimeInjected != 1 {
		t.Fatalf("record does not distinguish the injected option: %#v", record.Options)
	}
}

// Options are material evidence, so a resume must reuse the durable proposal
// rather than proposing again and invalidating the round (spec §15.4).
func TestProposalIsReusedOnResume(t *testing.T) {
	journal := &memoryJournal{}
	proposer := &stubProposer{}
	failing := newRecordingRunner(func(judgeID string, _ int) (DecisionOpinion, error) {
		if judgeID == "judge-2" {
			return DecisionOpinion{}, context.Canceled
		}
		return DecisionOpinion{
			OptionScores: []OptionScore{
				{OptionID: "migrate-now", Criteria: map[string]float64{"cost": 8, "risk": 8}},
				{OptionID: "reduce", Criteria: map[string]float64{"cost": 5, "risk": 5}},
				{OptionID: "runtime-defer", Criteria: map[string]float64{"cost": 2, "risk": 2}},
			},
			PreferredOption: "migrate-now", SuccessProbability: 0.7,
		}, nil
	})

	req := engineRequest(proposalPolicy())
	req.Options = nil
	req.DecisionID = "dec-proposal"

	engine := newTestEngineWithStages(journal, failing, nil, DecisionServices{Proposer: proposer})
	if _, err := engine.Run(context.Background(), req); err == nil {
		t.Fatal("Run succeeded despite judge-2 failing")
	}
	firstCalls := proposer.calls

	recovered := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		return DecisionOpinion{
			OptionScores: []OptionScore{
				{OptionID: "migrate-now", Criteria: map[string]float64{"cost": 8, "risk": 8}},
				{OptionID: "reduce", Criteria: map[string]float64{"cost": 5, "risk": 5}},
				{OptionID: "runtime-defer", Criteria: map[string]float64{"cost": 2, "risk": 2}},
			},
			PreferredOption: "migrate-now", SuccessProbability: 0.7,
		}, nil
	})
	resumed := newTestEngineWithStages(journal, recovered, nil, DecisionServices{Proposer: proposer})
	if _, err := resumed.Run(context.Background(), req); err != nil {
		t.Fatalf("resume = %v", err)
	}
	if proposer.calls != firstCalls {
		t.Fatalf("proposer ran again on resume: %d then %d", firstCalls, proposer.calls)
	}
}

func TestProposalDisabledIsAConfigurationError(t *testing.T) {
	policy := enginePolicy(2)
	req := engineRequest(policy)
	req.Options = nil

	engine := newTestEngineWithStages(&memoryJournal{}, spreadRunner(), nil, DecisionServices{Proposer: &stubProposer{}})
	_, err := engine.Run(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), ReasonDecisionMissingAlternative) {
		t.Fatalf("Run = %v, want %s", err, ReasonDecisionMissingAlternative)
	}
}

func TestProposalEnabledWithoutAProposerFailsClosed(t *testing.T) {
	req := engineRequest(proposalPolicy())
	req.Options = nil

	engine := newTestEngineWithStages(&memoryJournal{}, spreadRunner(), nil, DecisionServices{})
	_, err := engine.Run(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), ReasonDecisionMissingAlternative) {
		t.Fatalf("Run = %v, want a fail-closed on the missing proposer", err)
	}
}

func TestProposalProducingNothingIsBlocked(t *testing.T) {
	req := engineRequest(proposalPolicy())
	req.Options = nil
	req.Policy.Discipline.Alternatives = AlternativesPolicy{}

	engine := newTestEngineWithStages(&memoryJournal{}, spreadRunner(), nil, DecisionServices{
		Proposer: &stubProposer{respond: func() ([]DecisionOption, error) { return nil, nil }},
	})
	_, err := engine.Run(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), ReasonDecisionMissingAlternative) {
		t.Fatalf("Run = %v, want a block on an empty proposal", err)
	}
}

func TestOptionProposalPolicyValidation(t *testing.T) {
	policy := enginePolicy(2)
	policy.OptionProposal = OptionProposalPolicy{Enabled: true, MaxOptions: 2}
	policy.Discipline.Alternatives = AlternativesPolicy{MinOptions: 3}
	if err := policy.Validate(); err == nil || !strings.Contains(err.Error(), "below discipline.alternatives.min-options") {
		t.Fatalf("Validate = %v, want a cap-below-floor rejection", err)
	}

	policy.OptionProposal.MaxOptions = 4
	if err := policy.Validate(); err != nil {
		t.Fatalf("Validate = %v, want nil", err)
	}
	if got := (OptionProposalPolicy{}).EffectiveMaxOptions(); got != 5 {
		t.Fatalf("EffectiveMaxOptions = %d, want the built-in default", got)
	}
}
