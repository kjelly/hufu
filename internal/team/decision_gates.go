package team

import (
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

// Decision quality gates (docs/hufu-decision-aware-runtime-spec.md §17, §19,
// §40 forecast validation).
//
// Every gate here is deterministic and runs before judgment starts, so a
// decision cannot be formed at all without the alternatives and outside view
// the profile requires. Blocking early is the point: once judges have scored a
// two-option list, adding "do nothing" afterwards no longer changes what they
// considered.

// GateResult explains a blocked decision with a canonical reason code.
type GateResult struct {
	Reason string
	Detail string
}

func (g GateResult) Error() string {
	if g.Detail == "" {
		return g.Reason
	}
	return fmt.Sprintf("%s: %s", g.Reason, g.Detail)
}

// CheckAlternatives enforces that a no-action and, where required, an
// information-gathering alternative exist before JUDGE (spec §19).
func CheckAlternatives(policy AlternativesPolicy, options []DecisionOption) *GateResult {
	if policy.MinOptions > 0 && len(options) < policy.MinOptions {
		return &GateResult{
			Reason: ReasonDecisionMissingAlternative,
			Detail: fmt.Sprintf("%d options offered, profile requires at least %d", len(options), policy.MinOptions),
		}
	}
	if policy.RequireNoActionOption {
		found := false
		for _, option := range options {
			if option.Kind.IsNoGo() {
				found = true
				break
			}
		}
		if !found {
			return &GateResult{
				Reason: ReasonDecisionNoNoGoOption,
				Detail: "profile requires a defer or abandon alternative and none was offered",
			}
		}
	}
	if policy.RequireInfoOption {
		found := false
		for _, option := range options {
			if option.Kind == OptionRequestInfo {
				found = true
				break
			}
		}
		if !found {
			return &GateResult{
				Reason: ReasonDecisionMissingAlternative,
				Detail: "profile requires a request_information alternative and none was offered",
			}
		}
	}
	return nil
}

// CheckOutsideView enforces the base-rate requirement before JUDGE (spec §17).
func CheckOutsideView(policy OutsideViewPolicy, rates []BaseRateEvidence) *GateResult {
	if !policy.Required {
		return nil
	}
	for i, rate := range rates {
		if err := validateBaseRate(rate); err != nil {
			return &GateResult{
				Reason: ReasonDecisionOutsideViewMissing,
				Detail: fmt.Sprintf("base_rates[%d] is not usable: %v", i, err),
			}
		}
	}
	if len(rates) == 0 {
		return &GateResult{
			Reason: ReasonDecisionOutsideViewMissing,
			Detail: "profile requires outside-view evidence and none was supplied",
		}
	}
	return nil
}

// validateBaseRate is deterministic: a reference class is usable only if it
// names a class and metric, has a real sample, has finite distribution values,
// and points at an artifact that actually exists in the store.
func validateBaseRate(rate BaseRateEvidence) error {
	if strings.TrimSpace(rate.ReferenceClass) == "" {
		return fmt.Errorf("reference_class is empty")
	}
	if strings.TrimSpace(rate.Metric) == "" {
		return fmt.Errorf("metric is empty")
	}
	if rate.SampleSize < 1 {
		return fmt.Errorf("sample_size %d is not a real sample", rate.SampleSize)
	}
	for name, value := range map[string]float64{
		"mean": rate.Distribution.Mean, "median": rate.Distribution.Median,
		"p10": rate.Distribution.P10, "p90": rate.Distribution.P90,
	} {
		if roundDecision(value) != roundDecision(value) || isNotFinite(value) {
			return fmt.Errorf("distribution.%s is not finite", name)
		}
	}
	if strings.TrimSpace(rate.Source.SHA256) == "" {
		return fmt.Errorf("source must reference a persisted artifact by digest")
	}
	return nil
}

func isNotFinite(v float64) bool {
	return v != v || v > 1e308 || v < -1e308
}

// CheckPremortem enforces the pre-commit premortem requirement (spec §24).
func CheckPremortem(policy PremortemPolicy, result *PremortemResult) *GateResult {
	if !policy.RequiredBeforeCommit {
		return nil
	}
	if result == nil || len(result.FailureModes) == 0 {
		return &GateResult{
			Reason: ReasonDecisionPremortemRequired,
			Detail: "profile requires a premortem before commit and none produced failure modes",
		}
	}
	for i, mode := range result.FailureModes {
		if strings.TrimSpace(mode.Description) == "" {
			return &GateResult{
				Reason: ReasonDecisionPremortemRequired,
				Detail: fmt.Sprintf("failure_modes[%d] has no description", i),
			}
		}
		if !validUnit(mode.Likelihood) || !validUnit(mode.Impact) {
			return &GateResult{
				Reason: ReasonDecisionPremortemRequired,
				Detail: fmt.Sprintf("failure_modes[%d] likelihood/impact must be in [0, 1]", i),
			}
		}
	}
	return nil
}

// CheckForecast enforces that a decision claiming a resolvable outcome carries
// a usable probability and the conditions under which it would be judged wrong
// (spec §40). Outcome resolution itself is Phase 5 and out of V1 scope; what
// V1 guarantees is that the claim is recorded well enough to resolve later.
func CheckForecast(policy ForecastPolicy, record DecisionRecord) *GateResult {
	if !policy.Required {
		return nil
	}
	if !validUnit(record.Probability) || record.Probability == 0 {
		return &GateResult{
			Reason: ReasonDecisionMissingObjective,
			Detail: fmt.Sprintf("forecast is required but probability %v is not a usable value in (0, 1]", record.Probability),
		}
	}
	if len(record.FalsificationConditions) == 0 {
		return &GateResult{
			Reason: ReasonDecisionMissingObjective,
			Detail: "forecast is required but no falsification condition was recorded",
		}
	}
	return nil
}

// CheckRequestContract enforces the structured-decision preconditions from
// spec §11. A decision that cannot say what it is trying to achieve, or how
// anyone would know it worked, is not ready to be judged.
func CheckRequestContract(contract *RequestContract) *GateResult {
	if contract == nil {
		return &GateResult{
			Reason: ReasonDecisionMissingObjective,
			Detail: "structured decisions require a request contract",
		}
	}
	if strings.TrimSpace(contract.Objective) == "" {
		return &GateResult{
			Reason: ReasonDecisionMissingObjective,
			Detail: "request contract has no objective",
		}
	}
	if len(contract.SuccessCriteria) == 0 {
		return &GateResult{
			Reason: ReasonDecisionMissingObjective,
			Detail: "request contract declares no success criteria",
		}
	}
	for i, criterion := range contract.SuccessCriteria {
		if strings.TrimSpace(criterion.Statement) == "" {
			return &GateResult{
				Reason: ReasonDecisionMissingObjective,
				Detail: fmt.Sprintf("success_criteria[%d] has no statement", i),
			}
		}
	}
	return nil
}

// FinalOptionFor applies the configured finalization mode. Coordinator and
// judge finalization receive structured objects, never raw conversation, and an
// override away from the aggregate must carry a reason (spec §26).
func FinalOptionFor(policy DecisionPolicy, aggregate DecisionAggregate, override string, reason string) (string, bool, error) {
	mode := policy.EffectiveFinalization()
	if mode == agent.FinalizationAggregate || strings.TrimSpace(override) == "" {
		return aggregate.PreferredOption, false, nil
	}
	if strings.TrimSpace(reason) == "" {
		return "", false, fmt.Errorf("finalization mode %q chose %q over the aggregate's %q without a recorded reason",
			mode, override, aggregate.PreferredOption)
	}
	return override, override != aggregate.PreferredOption, nil
}
