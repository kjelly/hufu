package improver

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode"

	"github.com/kjelly/hufu/internal/improve/paired"
)

// StageSpec is derived from the frozen plan, never from model prose. A group
// includes the entire required validation envelope for a proposed variant.
type StageSpec struct {
	ID    string          `json:"id"`
	Kind  string          `json:"kind"`
	Group string          `json:"group"`
	Slot  paired.PairSlot `json:"slot"`
}

func plannedStages(plan paired.ExperimentPlan) ([]StageSpec, []string, error) {
	if err := validateCommittedPlan(plan); err != nil {
		return nil, nil, err
	}
	groups := []string{"baseline"}
	for _, slot := range plan.Slots {
		if !slices.Contains(groups, slot.Variant) {
			groups = append(groups, slot.Variant)
		}
	}
	var stages []StageSpec
	for _, group := range groups {
		if plan.Mode == "refine" && group != "baseline" {
			kind := group
			if group == "composition" {
				kind = "compose"
			}
			stages = append(stages, StageSpec{ID: "proposal_" + group, Kind: kind, Group: group})
		}
		for i, slot := range plan.Slots {
			if slot.Variant == group {
				stages = append(stages, StageSpec{ID: fmt.Sprintf("probe_%04d", i+1), Kind: "executor", Group: group, Slot: slot})
			}
		}
	}
	if plan.Mode == "refine" {
		stages = append(stages, StageSpec{ID: "reflection", Kind: "reflect", Group: "reflection"})
	}
	initial := groups[:2]
	var minimum int64
	var minimumStages int64
	for _, stage := range stages {
		if slices.Contains(initial, stage.Group) {
			minimumStages++
			if plan.Limits.PerStageTokens > math.MaxInt64-minimum {
				return nil, nil, errors.New("plan: initial envelope overflow")
			}
			minimum += plan.Limits.PerStageTokens
		}
	}
	if minimum > plan.Limits.PipelineTokens {
		return nil, nil, errors.New("plan: initial envelope exceeds pipeline budget")
	}
	if plan.Limits.PerStageSeconds > plan.Limits.PipelineSeconds/minimumStages {
		return nil, nil, errors.New("plan: initial envelope exceeds pipeline deadline")
	}
	return stages, initial, nil
}

func validateCommittedPlan(p paired.ExperimentPlan) error {
	if p.Kind != "paired_experiment_plan" || p.SchemaVersion != paired.SchemaVersion ||
		!paired.ValidID(p.ExperimentID) || !opaqueID(p.ParentBranchID) || !opaqueID(p.ParentWorkspaceID) ||
		p.Profile != "structured_only" || p.PolicyProjectionVersion != 1 {
		return errors.New("plan: invalid committed identity or profile")
	}
	for _, digest := range []string{p.BinaryDigest, p.ComparisonContextDigest} {
		if err := (paired.ArtifactRef{Kind: "digest", Digest: digest}).Validate("digest"); err != nil {
			return err
		}
	}
	if err := p.Limits.Validate(); err != nil {
		return err
	}
	if err := p.Objective.Validate(); err != nil {
		return err
	}
	for _, ref := range append([]paired.ArtifactRef{p.Config, p.Fixture, p.BaselineSnapshot}, p.Inputs...) {
		if err := ref.Validate(ref.Kind); err != nil {
			return err
		}
	}
	if p.Mode != "manual" && p.Mode != "refine" || p.ExecutionMode != "fixture" && p.ExecutionMode != "llm" {
		return errors.New("plan: invalid mode")
	}
	roles := []string{"executor"}
	if p.Mode == "refine" {
		roles = append(roles, "proposer", "composer", "reflector")
	}
	if len(p.Models) != len(roles) {
		return errors.New("plan: unexpected models")
	}
	for _, role := range roles {
		model := p.Models[role]
		if model.Generation == nil || strings.TrimSpace(model.Model) == "" ||
			p.ExecutionMode == "fixture" && (model.Backend != "fixture" || model.Model != "scripted") ||
			p.ExecutionMode == "llm" && model.Backend != "hufu-local" {
			return errors.New("plan: unresolved model or generation")
		}
		if err := (paired.ArtifactRef{Kind: "policy", Digest: model.PolicyDigest}).Validate("policy"); err != nil {
			return err
		}
	}
	return validatePlanSlots(p)
}

func validatePlanSlots(p paired.ExperimentPlan) error {
	variants := []string{"baseline"}
	control := map[paired.PairSlot]bool{}
	cases := map[string]map[int64]bool{}
	seen := map[paired.PairSlot]bool{}
	if len(p.Slots) < 2 || len(p.Slots) > 4*paired.MaxCases*paired.MaxReplicates {
		return errors.New("plan: invalid slot count")
	}
	for _, slot := range p.Slots {
		if !paired.ValidID(slot.Variant) || !paired.ValidID(slot.Case) || slot.Replicate < 1 ||
			slot.Replicate > paired.MaxReplicates || seen[slot] {
			return errors.New("plan: invalid or duplicate slot")
		}
		seen[slot] = true
		if slot.Variant == "baseline" {
			control[slot] = true
			if cases[slot.Case] == nil {
				cases[slot.Case] = map[int64]bool{}
			}
			cases[slot.Case][slot.Replicate] = true
		} else if !slices.Contains(variants, slot.Variant) {
			variants = append(variants, slot.Variant)
		}
	}
	replicates := 0
	for _, values := range cases {
		if replicates == 0 {
			replicates = len(values)
		}
		if len(values) != replicates {
			return errors.New("plan: inconsistent replicate count")
		}
		for n := range replicates {
			if !values[int64(n+1)] {
				return errors.New("plan: replicate sequence has gaps")
			}
		}
	}
	if len(cases) > paired.MaxCases {
		return errors.New("plan: case limit exceeded")
	}
	if len(control) == 0 || p.Mode == "manual" && len(variants) != 2 ||
		p.Mode == "refine" && !slices.Equal(variants, []string{"baseline", "procedure", "domain", "composition"}) {
		return errors.New("plan: incomplete variant slots")
	}
	if len(p.Slots) != len(control)*len(variants) || int64(2*len(control)) > p.Limits.MaxProbeRuns {
		return errors.New("plan: incomplete required coverage or probe budget")
	}
	for slot := range control {
		for _, variant := range variants {
			slot.Variant = variant
			if !seen[slot] {
				return errors.New("plan: unpaired case or replicate")
			}
		}
	}
	return nil
}

func opaqueID(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || r == '/' || r == '\\'
	})
}
