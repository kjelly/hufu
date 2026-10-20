package paired

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
)

type RuntimeIdentity struct {
	InvocationID string `json:"invocation_id"`
	RunID        string `json:"run_id"`
	BranchID     string `json:"branch_id"`
}

type PairSlot struct {
	Variant   string `json:"variant_slot_id"`
	Case      string `json:"case_id"`
	Replicate int64  `json:"replicate"`
}

type RunManifest struct {
	SchemaVersion           int              `json:"schema_version"`
	ExperimentID            string           `json:"experiment_id"`
	ParentBranch            string           `json:"parent_branch"`
	SlotID                  string           `json:"slot_id"`
	Slot                    PairSlot         `json:"slot"`
	PairKey                 string           `json:"pair_key"`
	Attempt                 int64            `json:"attempt"`
	Generation              int64            `json:"generation"`
	Identity                RuntimeIdentity  `json:"identity"`
	ExecutionMode           string           `json:"execution_mode"`
	Snapshot                ArtifactRef      `json:"snapshot_ref"`
	VariantDigest           string           `json:"variant_digest"`
	ComparisonContextDigest string           `json:"comparison_context_digest"`
	EvidenceDigest          string           `json:"evidence_digest"`
	ExecutionPolicy         ArtifactRef      `json:"execution_policy_ref"`
	ResolvedModels          map[string]Model `json:"resolved_models"`
	Outcome                 string           `json:"outcome"`
	Receipts                []ArtifactRef    `json:"receipt_refs"`
	Verifications           []ArtifactRef    `json:"verification_refs"`
	Usage                   ArtifactRef      `json:"usage_ref"`
	UsageCompleteness       string           `json:"usage_completeness"`
	RetryOf                 *ArtifactRef     `json:"retry_of,omitzero"`
	ReusedFrom              *ArtifactRef     `json:"reused_from,omitzero"`
}

type Gate struct {
	Name     string `json:"name"`
	Expected string `json:"expected"`
	Observed string `json:"observed"`
	State    string `json:"state"`
}

type Coverage struct {
	Planned          int64 `json:"planned"`
	Terminal         int64 `json:"terminal"`
	Scored           int64 `json:"scored"`
	FunctionalFailed int64 `json:"functional_failed"`
	Unknown          int64 `json:"unknown"`
	NotStarted       int64 `json:"not_started"`
}

type BudgetSummary struct {
	SpentTokens    int64 `json:"spent_tokens"`
	ReservedTokens int64 `json:"reserved_tokens"`
	UnknownStages  int64 `json:"unknown_stages"`
	ActiveStages   int64 `json:"active_stages"`
	LimitTokens    int64 `json:"limit_tokens"`
}

// Observation is an adapter's verified projection of canonical evidence. It is
// not an import format: a journal reader must validate all referenced objects
// before passing observations to Evaluate.
type Observation struct {
	Slot                    PairSlot `json:"slot"`
	ComparisonContextDigest string   `json:"comparison_context_digest"`
	Outcome                 string   `json:"outcome"`
	UsageKnown              bool     `json:"usage_known"`
	AttributableTokens      int64    `json:"attributable_tokens"`
	EvidenceValid           bool     `json:"evidence_valid"`
}

type Report struct {
	Kind               string        `json:"kind"`
	SchemaVersion      int           `json:"schema_version"`
	ExecutionMode      string        `json:"execution_mode"`
	Decision           string        `json:"decision"`
	SimulationDecision string        `json:"simulation_decision,omitempty"`
	SelectedVariant    string        `json:"selected_variant"`
	Coverage           Coverage      `json:"coverage"`
	Budget             BudgetSummary `json:"budget"`
	Gates              []Gate        `json:"gates"`
	Observations       []Observation `json:"observations"`
	PlanRef            *ArtifactRef  `json:"plan_ref,omitzero"`
	ManifestRefs       []ArtifactRef `json:"manifest_refs"`
}

type Evaluation struct {
	ExecutionMode           string
	ComparisonContextDigest string
	Cases                   []Case
	Replicates              int64
	// Candidates are bound variants only, in frozen slot order.
	Candidates            []string
	Objective             Objective
	Observations          []Observation
	Budget                BudgetSummary
	ProtectedSurfaceValid bool
	ResumeConsistent      bool
}

type variantScore struct {
	id     string
	passes int64
	tokens int64
}

// Evaluate compares exact pair coverage and objective evidence without model
// calls. Baseline functional failures are scored zero, not discarded.
func Evaluate(input Evaluation) (Report, error) {
	if err := validateEvaluation(input); err != nil {
		return Report{}, err
	}
	report := Report{
		Kind: "paired_experiment_report", SchemaVersion: SchemaVersion,
		ExecutionMode: input.ExecutionMode, Decision: "inconclusive",
		SelectedVariant: "baseline", Budget: input.Budget, Observations: slices.Clone(input.Observations),
	}
	variants := append([]string{"baseline"}, input.Candidates...)
	expected := make(map[PairSlot]bool)
	for _, variant := range variants {
		for _, item := range input.Cases {
			for replicate := int64(1); replicate <= input.Replicates; replicate++ {
				expected[PairSlot{variant, item.ID, replicate}] = true
			}
		}
	}
	report.Coverage.Planned = int64(len(expected))
	observed, pairing, integrity, baselineValid := collectObservations(input, expected, &report.Coverage)
	coverage := report.Coverage.Scored == report.Coverage.Planned && pairing
	resource := input.Budget.UnknownStages == 0 && input.Budget.ReservedTokens == 0 &&
		input.Budget.SpentTokens <= input.Budget.LimitTokens
	report.Gates = []Gate{
		booleanGate("pairing_integrity", pairing),
		booleanGate("baseline_evidence_valid", baselineValid),
		booleanGate("evidence_integrity", integrity),
		booleanGate("protected_surface", input.ProtectedSurfaceValid),
		booleanGate("required_coverage", coverage),
		booleanGate("quality_non_regression", false),
		booleanGate("objective_gain", false),
		booleanGate("resource_limits", resource),
		booleanGate("resume_consistency", input.ResumeConsistent),
	}
	if !coverage {
		report.Gates[4].State, report.Gates[4].Observed = "unknown", "incomplete scored coverage"
	}
	if !baselineValid {
		report.Gates[1].State, report.Gates[1].Observed = "unknown", "baseline evidence incomplete"
	}
	if input.Budget.UnknownStages > 0 || input.Budget.ReservedTokens > 0 {
		report.Gates[7].State, report.Gates[7].Observed = "unknown", "unsettled allocation"
	}
	if input.Budget.ActiveStages > 0 || !input.ResumeConsistent {
		report.Decision = "blocked"
		return report, nil
	}
	if !pairing || !integrity || !baselineValid || !coverage || input.Budget.UnknownStages > 0 || input.Budget.ReservedTokens > 0 {
		return report, nil
	}
	if !input.ProtectedSurfaceValid || !resource {
		report.Decision = simulatedDecision(&report, "rejected")
		return report, nil
	}
	return selectCandidate(input, observed, report)
}

func validateEvaluation(input Evaluation) error {
	if input.ExecutionMode != "fixture" && input.ExecutionMode != "llm" {
		return errors.New("evaluation: invalid execution_mode")
	}
	if input.ComparisonContextDigest == "" || input.Replicates < 1 || input.Replicates > MaxReplicates ||
		len(input.Cases) < 1 || len(input.Cases) > MaxCases {
		return errors.New("evaluation: invalid context or case/replicate limits")
	}
	for _, ids := range [][]string{caseIDs(input.Cases), append([]string{"baseline"}, input.Candidates...)} {
		seen := map[string]bool{}
		for _, id := range ids {
			if !ValidID(id) || seen[id] {
				return errors.New("evaluation: invalid or duplicate identity")
			}
			seen[id] = true
		}
	}
	if len(input.Candidates) > 3 || input.Budget.LimitTokens <= 0 || input.Budget.SpentTokens < 0 ||
		input.Budget.ReservedTokens < 0 || input.Budget.UnknownStages < 0 || input.Budget.ActiveStages < 0 {
		return errors.New("evaluation: invalid budget or candidate count")
	}
	return input.Objective.Validate()
}

func caseIDs(cases []Case) []string {
	ids := make([]string, len(cases))
	for i, item := range cases {
		ids[i] = item.ID
	}
	return ids
}

func collectObservations(input Evaluation, expected map[PairSlot]bool, coverage *Coverage) (map[PairSlot]Observation, bool, bool, bool) {
	observed := map[PairSlot]Observation{}
	pairing, integrity, baselineValid := true, true, true
	var attributable int64
	for _, item := range input.Observations {
		if !expected[item.Slot] {
			pairing = false
			continue
		}
		if _, duplicate := observed[item.Slot]; duplicate {
			pairing = false
			continue
		}
		observed[item.Slot] = item
		if item.ComparisonContextDigest != input.ComparisonContextDigest {
			pairing = false
		}
		if !item.EvidenceValid {
			integrity = false
		}
		if item.AttributableTokens < 0 || item.AttributableTokens > math.MaxInt64-attributable {
			integrity = false
		} else {
			attributable += item.AttributableTokens
		}
		valid := item.EvidenceValid && item.UsageKnown && item.AttributableTokens >= 0 &&
			slices.Contains([]string{"passed", "failed", "timed_out"}, item.Outcome)
		if item.Slot.Variant == "baseline" && !valid {
			baselineValid = false
		}
		if valid {
			coverage.Scored++
			coverage.Terminal++
			if item.Outcome != "passed" {
				coverage.FunctionalFailed++
			}
		} else {
			coverage.Unknown++
		}
	}
	coverage.NotStarted = coverage.Planned - int64(len(observed))
	if attributable > input.Budget.SpentTokens {
		integrity = false
	}
	for slot := range expected {
		if _, exists := observed[slot]; !exists && slot.Variant == "baseline" {
			baselineValid = false
		}
	}
	return observed, pairing, integrity, baselineValid
}

func booleanGate(name string, passed bool) Gate {
	state := "failed"
	if passed {
		state = "passed"
	}
	return Gate{Name: name, Expected: "passed", Observed: state, State: state}
}

func simulatedDecision(report *Report, decision string) string {
	if report.ExecutionMode == "fixture" {
		report.SimulationDecision = decision
		return "simulation_only"
	}
	return decision
}

func selectCandidate(input Evaluation, observed map[PairSlot]Observation, report Report) (Report, error) {
	baseline, err := scoreVariant("baseline", input, observed)
	if err != nil {
		return Report{}, err
	}
	best := baseline
	anyNonRegression := len(input.Candidates) == 0
	anyGain := false
	for _, id := range input.Candidates {
		score, err := scoreVariant(id, input, observed)
		if err != nil {
			return Report{}, err
		}
		if !nonRegression(id, input, observed) {
			continue
		}
		anyNonRegression = true
		if !objectiveGain(input.Objective, baseline, score) {
			continue
		}
		anyGain = true
		if best.id == "baseline" || betterCandidate(input.Objective.Kind, score, best) {
			best = score
		}
	}
	decision := "no_change"
	if anyGain {
		decision = "eligible_for_review"
	} else if !anyNonRegression {
		decision = "rejected"
	}
	report.SelectedVariant = best.id
	report.Gates[5] = booleanGate("quality_non_regression", anyNonRegression)
	report.Gates[6] = booleanGate("objective_gain", anyGain)
	report.Decision = simulatedDecision(&report, decision)
	return report, nil
}

func scoreVariant(id string, input Evaluation, observations map[PairSlot]Observation) (variantScore, error) {
	result := variantScore{id: id}
	for _, item := range input.Cases {
		for r := int64(1); r <= input.Replicates; r++ {
			observation := observations[PairSlot{id, item.ID, r}]
			if observation.AttributableTokens > math.MaxInt64-result.tokens {
				return variantScore{}, fmt.Errorf("variant %s: token total overflow", id)
			}
			result.tokens += observation.AttributableTokens
			if observation.Outcome == "passed" {
				result.passes++
			}
		}
	}
	return result, nil
}

func nonRegression(id string, input Evaluation, observations map[PairSlot]Observation) bool {
	for _, item := range input.Cases {
		for r := int64(1); r <= input.Replicates; r++ {
			control := observations[PairSlot{"baseline", item.ID, r}]
			candidate := observations[PairSlot{id, item.ID, r}]
			if (item.Critical || control.Outcome == "passed") && candidate.Outcome != "passed" {
				return false
			}
		}
	}
	return true
}

func objectiveGain(objective Objective, baseline, candidate variantScore) bool {
	if objective.Kind == "quality_gain" {
		return candidate.passes-baseline.passes >= *objective.MinPassGain
	}
	if baseline.tokens == 0 || candidate.tokens >= baseline.tokens {
		return false
	}
	// Cross products must not overflow on a provider's large real usage.
	saved := new(big.Int).Mul(big.NewInt(baseline.tokens-candidate.tokens), big.NewInt(100))
	required := new(big.Int).Mul(big.NewInt(baseline.tokens), big.NewInt(*objective.MinTokenReductionPercent))
	return saved.Cmp(required) >= 0
}

func betterCandidate(objective string, candidate, incumbent variantScore) bool {
	if objective == "quality_gain" {
		return candidate.passes > incumbent.passes || candidate.passes == incumbent.passes && candidate.tokens < incumbent.tokens
	}
	return candidate.tokens < incumbent.tokens || candidate.tokens == incumbent.tokens && candidate.passes > incumbent.passes
}
