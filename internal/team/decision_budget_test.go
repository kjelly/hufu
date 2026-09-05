package team

import (
	"context"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func budgetWith(maxTokens, used int64) BudgetManager {
	var ledger budgetLedger
	ledger.setLimits(0, maxTokens)
	ledger.addTokens(used)
	return &ledger
}

func TestRequiredDecisionTokens(t *testing.T) {
	// An explicit allowance is authoritative.
	if got := requiredDecisionTokens(DecisionPolicy{IndependentJudgments: 5, MaxTokens: 12345}); got != 12345 {
		t.Fatalf("requiredDecisionTokens = %d, want the configured allowance", got)
	}
	// Otherwise the floor counts every dispatch the profile implies.
	policy := DecisionPolicy{IndependentJudgments: 3, MaxRounds: 2, Challenge: ChallengePolicy{Enabled: true, Count: 1}}
	if got := requiredDecisionTokens(policy); got != (3*2+1)*defaultJudgeTokenEstimate {
		t.Fatalf("requiredDecisionTokens = %d, want judges*rounds+challengers", got)
	}
}

func TestRemainingDecisionTokens(t *testing.T) {
	if got := remainingDecisionTokens(nil); got != -1 {
		t.Fatalf("remainingDecisionTokens(nil) = %d, want -1 for an unbounded run", got)
	}
	if got := remainingDecisionTokens(budgetWith(0, 500)); got != -1 {
		t.Fatalf("remainingDecisionTokens(no ceiling) = %d, want -1", got)
	}
	if got := remainingDecisionTokens(budgetWith(1000, 400)); got != 600 {
		t.Fatalf("remainingDecisionTokens = %d, want 600", got)
	}
	if got := remainingDecisionTokens(budgetWith(1000, 5000)); got != 0 {
		t.Fatalf("remainingDecisionTokens when overspent = %d, want 0", got)
	}
}

// The default is fail-closed: a profile that cannot run at its configured rigor
// does not quietly run at less (spec §34.1, Phase 1 budget acceptance).
func TestDecisionBudgetFailsClosedByDefault(t *testing.T) {
	journal := &memoryJournal{}
	runner := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		return scoredOpinion(8, 4, "migrate", 0.8), nil
	})
	policy := enginePolicy(5)
	engine := newTestEngine(journal, runner, budgetWith(1000, 900))

	_, err := engine.Run(context.Background(), engineRequest(policy))
	if err == nil || !strings.Contains(err.Error(), ReasonDecisionBudgetInsufficient) {
		t.Fatalf("Run = %v, want %s", err, ReasonDecisionBudgetInsufficient)
	}
	if len(runner.dispatched()) != 0 {
		t.Fatalf("dispatched %v judges despite failing closed", runner.dispatched())
	}
	if journal.count(agent.EventDecisionBudgetDegraded) != 0 {
		t.Fatal("a forbidden-degradation profile emitted a degradation event")
	}
}

// Silently reducing five judges to one is exactly what the runtime must not do.
func TestDecisionBudgetDoesNotSilentlyReduceJudges(t *testing.T) {
	journal := &memoryJournal{}
	dispatched := 0
	runner := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		dispatched++
		return scoredOpinion(8, 4, "migrate", 0.8), nil
	})
	engine := newTestEngine(journal, runner, budgetWith(1_000_000, 0))

	record, err := engine.Run(context.Background(), engineRequest(enginePolicy(5)))
	if err != nil {
		t.Fatal(err)
	}
	if dispatched != 5 || record.Aggregates[0].JudgeCount != 5 {
		t.Fatalf("dispatched %d judges, aggregate counted %d; want 5 both", dispatched, record.Aggregates[0].JudgeCount)
	}
	if len(record.Degradations) != 0 {
		t.Fatalf("degradations recorded when the budget was sufficient: %#v", record.Degradations)
	}
}

// Opting into degradation reduces rigor in a fixed order, and never silently:
// every step is an event and lands in the record (spec §34.1).
func TestDecisionBudgetExplicitDegradationLadder(t *testing.T) {
	journal := &memoryJournal{}
	runner := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		return scoredOpinion(8, 4, "migrate", 0.8), nil
	})

	policy := enginePolicy(5)
	policy.MinIndependentJudgments = 3
	policy.MaxRounds = 2
	policy.Revision.Enabled = true
	policy.Challenge = ChallengePolicy{Enabled: true, Count: 2}
	policy.BudgetDegradation = agent.BudgetDegradationExplicit

	// Enough for the floor (3 judges * 1 round + 1 challenger), not for the
	// configured 5 judges * 2 rounds + 2 challengers, so the whole ladder runs.
	engine := newTestEngineWithStages(journal, runner, budgetWith(4*defaultJudgeTokenEstimate, 0), DecisionServices{
		Challengers: &stubChallenger{},
		Revisions:   &stubReviser{},
	})

	record, err := engine.Run(context.Background(), engineRequest(policy))
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	steps := make([]string, 0, len(record.Degradations))
	for _, degradation := range record.Degradations {
		steps = append(steps, degradation.Step)
	}
	want := []string{degradeChallengeCount, degradeRevision, degradeJudgeCount}
	if len(steps) != len(want) {
		t.Fatalf("degradation steps = %v, want %v", steps, want)
	}
	for i := range want {
		if steps[i] != want[i] {
			t.Fatalf("degradation order = %v, want the fixed ladder %v", steps, want)
		}
	}
	if journal.count(agent.EventDecisionBudgetDegraded) != len(want) {
		t.Fatalf("degradation events = %d, want %d; degradation must never be silent",
			journal.count(agent.EventDecisionBudgetDegraded), len(want))
	}
	// The floor is respected: judges never drop below min-independent-judgments.
	if got := len(runner.dispatched()); got != 3 {
		t.Fatalf("dispatched %d judges, want the declared floor of 3", got)
	}
	for _, degradation := range record.Degradations {
		if degradation.Reason == "" || degradation.Timestamp.IsZero() {
			t.Fatalf("degradation %#v is missing its reason or timestamp", degradation)
		}
	}
}

// Even with degradation enabled, a budget that cannot support the floor fails
// closed rather than dropping below min-independent-judgments (spec §34.2).
func TestDecisionBudgetFailsClosedBelowTheFloor(t *testing.T) {
	journal := &memoryJournal{}
	runner := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		return scoredOpinion(8, 4, "migrate", 0.8), nil
	})
	policy := enginePolicy(5)
	policy.MinIndependentJudgments = 3
	policy.BudgetDegradation = agent.BudgetDegradationExplicit
	engine := newTestEngine(journal, runner, budgetWith(defaultJudgeTokenEstimate, 0))

	_, err := engine.Run(context.Background(), engineRequest(policy))
	if err == nil || !strings.Contains(err.Error(), ReasonDecisionBudgetInsufficient) {
		t.Fatalf("Run = %v, want %s", err, ReasonDecisionBudgetInsufficient)
	}
	if len(runner.dispatched()) != 0 {
		t.Fatalf("dispatched %v judges despite failing closed", runner.dispatched())
	}
}

// Quality gates are never on the ladder (spec §34.2).
func TestDegradationNeverTouchesQualityGates(t *testing.T) {
	policy := DecisionPolicy{
		IndependentJudgments:    5,
		MinIndependentJudgments: 3,
		MaxRounds:               2,
		Challenge:               ChallengePolicy{Enabled: true, Count: 2},
		Revision:                RevisionPolicy{Enabled: true},
		OutsideView:             OutsideViewPolicy{Required: true},
		Premortem:               PremortemPolicy{Enabled: true, RequiredBeforeCommit: true},
		ContextIsolation:        agent.DecisionIsolationSealed,
		Discipline: DisciplinePolicy{
			Alternatives: AlternativesPolicy{RequireNoActionOption: true, RequireInfoOption: true, MinOptions: 3},
			Stop:         StopPolicy{RequireKillCriteria: true},
			Commit:       CommitGatePolicy{RequireVerification: true, RequireEvidence: true},
		},
	}
	for _, step := range []func(*DecisionPolicy) (DecisionDegradation, bool){
		degradeChallenge, degradeRevisionRound, degradeJudges,
	} {
		step(&policy)
	}
	if !policy.OutsideView.Required || !policy.Premortem.RequiredBeforeCommit {
		t.Fatal("degradation weakened outside-view or premortem")
	}
	if policy.ContextIsolation != agent.DecisionIsolationSealed {
		t.Fatal("degradation weakened context isolation")
	}
	if !policy.Discipline.Alternatives.RequireNoActionOption ||
		!policy.Discipline.Alternatives.RequireInfoOption ||
		policy.Discipline.Alternatives.MinOptions != 3 {
		t.Fatal("degradation weakened the alternatives gate")
	}
	if !policy.Discipline.Stop.RequireKillCriteria ||
		!policy.Discipline.Commit.RequireVerification ||
		!policy.Discipline.Commit.RequireEvidence {
		t.Fatal("degradation weakened a stop or commit prerequisite")
	}
	if policy.IndependentJudgments != 3 {
		t.Fatalf("judges = %d, want the floor of 3", policy.IndependentJudgments)
	}
}
