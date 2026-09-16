package team

// DecisionExecutionPlan is a non-authoritative projection of the stages a
// policy makes eligible. Runtime inputs and gate outcomes can still skip an
// eligible stage; the engine's explicit stage flow remains authoritative.
type DecisionExecutionPlan struct {
	SchemaVersion  int
	Proposal       bool
	Reference      bool
	JudgeCount     int
	Aggregation    string
	Challenge      bool
	ChallengeCount int
	Revision       bool
	Premortem      bool
	Forecast       bool
	Finalization   string
}

const decisionExecutionPlanSchemaVersion = 1

// CompileDecisionExecutionPlan validates policy and projects its effective
// stage eligibility without consulting profile names, catalogs, or routing
// authority.
func CompileDecisionExecutionPlan(policy DecisionPolicy) (DecisionExecutionPlan, error) {
	if err := policy.Validate(); err != nil {
		return DecisionExecutionPlan{}, err
	}
	challenge := policy.Challenge.Enabled && policy.Challenge.Count > 0
	challengeCount := 0
	if challenge {
		challengeCount = policy.Challenge.Count
	}
	return DecisionExecutionPlan{
		SchemaVersion:  decisionExecutionPlanSchemaVersion,
		Proposal:       policy.OptionProposal.Enabled,
		Reference:      policy.OutsideView.Required && policy.OutsideView.ReferenceEvidence,
		JudgeCount:     policy.IndependentJudgments,
		Aggregation:    policy.EffectiveAggregation(),
		Challenge:      challenge,
		ChallengeCount: challengeCount,
		Revision:       policy.Revision.Enabled && policy.EffectiveMaxRounds() >= 2 && challenge,
		Premortem:      policy.Premortem.Enabled,
		Forecast:       policy.Forecast.Required,
		Finalization:   policy.EffectiveFinalization(),
	}, nil
}
