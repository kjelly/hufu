package team

import (
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"gopkg.in/yaml.v3"
)

func TestCompileDecisionExecutionPlanProjectsEffectiveStages(t *testing.T) {
	policy := enginePolicy(3)
	policy.OptionProposal.Enabled = true
	policy.OutsideView = OutsideViewPolicy{Required: true, ReferenceEvidence: true}
	policy.Aggregation.Method = ""
	policy.Challenge = ChallengePolicy{Enabled: true, Count: 2}
	policy.Revision.Enabled = true
	policy.MaxRounds = 2
	policy.Premortem.Enabled = true
	policy.Forecast.Required = true
	policy.Finalization.Mode = ""

	got, err := CompileDecisionExecutionPlan(policy)
	if err != nil {
		t.Fatal(err)
	}
	want := DecisionExecutionPlan{
		SchemaVersion: 1, Proposal: true, Reference: true, JudgeCount: 3,
		Aggregation: agent.AggregationMeanScore, Challenge: true, ChallengeCount: 2,
		Revision: true, Premortem: true, Forecast: true, Finalization: agent.FinalizationAggregate,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan = %#v, want %#v", got, want)
	}

	policy.Challenge.Count = 0
	plan, err := CompileDecisionExecutionPlan(policy)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Challenge || plan.ChallengeCount != 0 || plan.Revision {
		t.Fatalf("ineligible challenge/revision projected as active: %#v", plan)
	}
}

func TestCompileDecisionExecutionPlanRejectsInvalidPolicy(t *testing.T) {
	if _, err := CompileDecisionExecutionPlan(enginePolicy(0)); err == nil {
		t.Fatal("invalid policy produced an execution plan")
	}
}

func TestDecisionExecutionIsIndependentOfProfileDisplayName(t *testing.T) {
	policy := enginePolicy(2)
	cfg := DecisionConfig{
		DefaultProfile: "alpha",
		ProfileSpecs: map[string]agent.DecisionProfileSpec{
			"alpha": {Policy: &policy},
			"beta":  {Policy: &policy},
		},
	}
	alpha, _, err := ResolveMaterializedDecisionProfile(cfg, "alpha", TaskDef{}, agent.BuiltInDecisionProfileCatalog())
	if err != nil {
		t.Fatal(err)
	}
	beta, _, err := ResolveMaterializedDecisionProfile(cfg, "beta", TaskDef{}, agent.BuiltInDecisionProfileCatalog())
	if err != nil {
		t.Fatal(err)
	}
	alphaPlan, err := CompileDecisionExecutionPlan(alpha.Policy)
	if err != nil {
		t.Fatal(err)
	}
	betaPlan, err := CompileDecisionExecutionPlan(beta.Policy)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(alphaPlan, betaPlan) {
		t.Fatalf("same materialized policy produced different plans: %#v != %#v", alphaPlan, betaPlan)
	}

	type outcome struct {
		finalOption string
		dispatches  []string
		events      []string
	}
	run := func(name string, materialized agent.MaterializedDecisionProfile) outcome {
		t.Helper()
		journal := &memoryJournal{}
		runner := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
			return scoredOpinion(8, 4, "migrate", 0.8), nil
		})
		req := engineRequest(materialized.Policy)
		req.Profile = name
		req.ProfileOrigin = materialized.Origin
		req.ProfileVersion = materialized.Version
		req.ProfileRef = materialized.Ref
		req.PolicyDigest = materialized.PolicyDigest
		record, runErr := newTestEngine(journal, runner, nil).Run(t.Context(), req)
		if runErr != nil {
			t.Fatalf("profile %q: %v", name, runErr)
		}
		return outcome{finalOption: record.FinalOption, dispatches: runner.dispatched(), events: journal.typesOf()}
	}
	alphaOutcome := run("alpha", alpha)
	betaOutcome := run("beta", beta)
	if !reflect.DeepEqual(alphaOutcome, betaOutcome) {
		t.Fatalf("profile display name changed stage behavior:\nalpha=%#v\nbeta=%#v", alphaOutcome, betaOutcome)
	}
}

func TestPresetSchemaCannotGrantAgentOrToolAuthority(t *testing.T) {
	for _, extra := range []string{"    agents: [unauthorized]\n", "    tools: [bash]\n"} {
		doc := []byte("profiles:\n  standard:\n    preset: builtin/standard@v1\n" + extra)
		var cfg DecisionConfig
		if err := yaml.Unmarshal(doc, &cfg); err == nil {
			t.Fatalf("preset accepted authority-bearing field in:\n%s", doc)
		}
	}
}
