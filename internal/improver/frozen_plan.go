package improver

import (
	"bytes"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/improve/paired"
)

type frozenFile struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
}

type frozenTree struct {
	SchemaVersion int          `json:"schema_version"`
	Files         []frozenFile `json:"files"`
}

func freezeTree(objects Objects, kind string, tree Tree) (paired.ArtifactRef, error) {
	document := frozenTree{SchemaVersion: 1}
	for _, name := range slices.Sorted(maps.Keys(tree.files)) {
		document.Files = append(document.Files, frozenFile{name, bytes.Clone(tree.files[name])})
	}
	return objects.PutJSON(kind, document)
}

func thawTree(objects Objects, ref paired.ArtifactRef, kind string) (Tree, error) {
	if err := ref.Validate(kind); err != nil {
		return Tree{}, err
	}
	var document frozenTree
	if err := readObjectJSON(objects, ref, &document); err != nil {
		return Tree{}, err
	}
	if document.SchemaVersion != 1 || len(document.Files) == 0 || len(document.Files) > paired.MaxFiles {
		return Tree{}, errors.New("invalid frozen tree")
	}
	tree := Tree{files: map[string][]byte{}}
	var size int64
	for _, file := range document.Files {
		if !paired.SafeRelativePath(file.Path) || len(file.Content) > paired.MaxDataBytes {
			return Tree{}, errors.New("invalid frozen file")
		}
		if _, exists := tree.files[file.Path]; exists {
			return Tree{}, errors.New("duplicate frozen file")
		}
		size += int64(len(file.Content))
		if size > paired.MaxTotalBytes {
			return Tree{}, errors.New("frozen tree exceeds byte limit")
		}
		if err := checkSanitized(file.Path, file.Content); err != nil {
			return Tree{}, err
		}
		tree.files[file.Path] = bytes.Clone(file.Content)
	}
	return tree, nil
}

func readObjectJSON(objects Objects, ref paired.ArtifactRef, target any) error {
	raw, err := objects.Read(ref)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target, json.RejectUnknownMembers(true))
}

// FreezePlan publishes the complete closure before the controller creation
// event. Orphaned immutable objects after a failed CAS grant no run authority.
func FreezePlan(prepared Prepared, workspaceID, branchID string, objects Objects, settings RuntimeSettings) (paired.ExperimentPlan, []VariantBinding, error) {
	if err := validateProgramCoverage(prepared); err != nil {
		return paired.ExperimentPlan{}, nil, err
	}
	factory, err := NewRuntimeFactory(prepared, settings)
	if err != nil {
		return paired.ExperimentPlan{}, nil, err
	}
	models, err := factory.Preflight()
	if err != nil {
		return paired.ExperimentPlan{}, nil, err
	}
	plan := paired.ExperimentPlan{
		Kind: "paired_experiment_plan", SchemaVersion: paired.SchemaVersion,
		ExperimentID: prepared.Config.ExperimentID, ParentWorkspaceID: workspaceID, ParentBranchID: branchID,
		Mode: prepared.Config.Mode, ExecutionMode: prepared.Config.ExecutionMode, Profile: prepared.Config.Profile,
		BinaryDigest: prepared.Preview.Build.BinaryDigest, SourceRevision: prepared.Preview.Build.SourceRevision,
		PolicyProjectionVersion: 1, Models: models, Limits: prepared.Config.Limits, Objective: prepared.Config.Objective,
	}
	plan.Config, err = objects.PutJSON("experiment_config", prepared.Config)
	if err != nil {
		return paired.ExperimentPlan{}, nil, err
	}
	plan.Fixture, err = objects.PutJSON("fixture", prepared.Fixture)
	if err != nil {
		return paired.ExperimentPlan{}, nil, err
	}
	inputs, err := freezeTree(objects, "frozen_inputs", prepared.Inputs)
	if err != nil {
		return paired.ExperimentPlan{}, nil, err
	}
	plan.Inputs = []paired.ArtifactRef{inputs}
	var bindings []VariantBinding
	for _, slot := range slices.Sorted(maps.Keys(prepared.Variants)) {
		variant := prepared.Variants[slot]
		ref, err := freezeTree(objects, "team_snapshot", variant.Tree)
		if err != nil {
			return paired.ExperimentPlan{}, nil, err
		}
		if slot == "baseline" {
			plan.BaselineSnapshot = ref
		}
		bindings = append(bindings, VariantBinding{Slot: slot, Snapshot: ref, Digest: variant.VariantDigest, Origin: "frozen_input"})
	}
	initial, err := objects.PutJSON("frozen_variants", bindings)
	if err != nil {
		return paired.ExperimentPlan{}, nil, err
	}
	plan.Inputs = append(plan.Inputs, initial)
	for _, slot := range prepared.Preview.Slots {
		plan.Slots = append(plan.Slots, paired.PairSlot{Variant: slot.Variant, Case: slot.Case, Replicate: slot.Replicate})
	}
	plan.ComparisonContextDigest, err = comparisonDigest(prepared, models)
	if err != nil {
		return paired.ExperimentPlan{}, nil, err
	}
	if err := validateCommittedPlan(plan); err != nil {
		return paired.ExperimentPlan{}, nil, err
	}
	if _, err := LoadPrepared(objects, plan, bindings); err != nil {
		return paired.ExperimentPlan{}, nil, err
	}
	return plan, bindings, nil
}

func comparisonDigest(prepared Prepared, models map[string]paired.ResolvedModel) (string, error) {
	return paired.Digest("comparison_context", map[string]any{
		"preview": prepared.Preview.ComparisonContextDigest, "models": models,
	})
}

// LoadPrepared uses only immutable objects. It never follows authored source
// paths or reads host configuration, provider credentials, or live metadata.
func LoadPrepared(objects Objects, plan paired.ExperimentPlan, bindings []VariantBinding) (Prepared, error) {
	if err := validateCommittedPlan(plan); err != nil {
		return Prepared{}, err
	}
	if plan.Config.Kind != "experiment_config" || plan.Fixture.Kind != "fixture" || len(plan.Inputs) != 2 {
		return Prepared{}, errors.New("unsupported frozen input closure")
	}
	prepared := Prepared{Variants: map[string]Variant{}}
	if err := readObjectJSON(objects, plan.Config, &prepared.Config); err != nil {
		return Prepared{}, err
	}
	if err := readObjectJSON(objects, plan.Fixture, &prepared.Fixture); err != nil {
		return Prepared{}, err
	}
	if err := validateFrozenConfiguration(prepared.Config, prepared.Fixture, plan); err != nil {
		return Prepared{}, err
	}
	cfg := prepared.Config
	var err error
	prepared.Inputs, err = thawTree(objects, plan.Inputs[0], "frozen_inputs")
	if err != nil {
		return Prepared{}, err
	}
	schema, ok := prepared.Inputs.Read("compiled/paired.schema.json")
	if !ok {
		return Prepared{}, errors.New("frozen result schema is missing")
	}
	prepared.Schema = schema
	for _, item := range prepared.Fixture.Cases {
		raw, ok := prepared.Inputs.Read("cases/" + item.ID + ".txt")
		if !ok || len(raw) == 0 || !utf8.Valid(raw) || len(raw) > paired.MaxTextBytes {
			return Prepared{}, fmt.Errorf("frozen input missing or invalid: %s", item.ID)
		}
	}
	if cfg.ExecutionMode == "fixture" {
		raw, ok := prepared.Inputs.Read("fixture_program.json")
		if !ok {
			return Prepared{}, errors.New("frozen fixture program missing")
		}
		program, err := paired.ParseProgram(raw)
		if err != nil {
			return Prepared{}, err
		}
		prepared.Program = &program
	}
	baseline, err := thawTree(objects, plan.BaselineSnapshot, "team_snapshot")
	if err != nil {
		return Prepared{}, err
	}
	base, surfaces, err := materializeTree(baseline, cfg.Vars, cfg.Worker, plan.BinaryDigest, cfg.AllowedSurfaces)
	if err != nil {
		return Prepared{}, err
	}
	prepared.Variants["baseline"] = base
	initial, err := FrozenBindings(objects, plan)
	if err != nil {
		return Prepared{}, err
	}
	seen := map[string]VariantBinding{}
	for _, binding := range append(initial, bindings...) {
		if previous, exists := seen[binding.Slot]; exists {
			if previous != binding {
				return Prepared{}, errors.New("conflicting variant binding")
			}
			continue
		}
		seen[binding.Slot] = binding
		tree, err := thawTree(objects, binding.Snapshot, "team_snapshot")
		if err != nil {
			return Prepared{}, err
		}
		variant, _, err := materializeTree(tree, cfg.Vars, cfg.Worker, plan.BinaryDigest, cfg.AllowedSurfaces)
		if err != nil {
			return Prepared{}, err
		}
		if variant.VariantDigest != binding.Digest {
			return Prepared{}, errors.New("frozen variant digest mismatch")
		}
		if binding.Slot == "baseline" && binding.Snapshot != plan.BaselineSnapshot {
			return Prepared{}, errors.New("baseline binding differs from plan")
		}
		if err := CompareCandidate(base, variant, surfaces); err != nil {
			return Prepared{}, err
		}
		prepared.Variants[binding.Slot] = variant
	}
	prepared.Preview = PlanPreview{
		Build:    BuildIdentity{BinaryDigest: plan.BinaryDigest, SourceRevision: plan.SourceRevision},
		Surfaces: surfaces,
	}
	if err := prepared.finishPreview(); err != nil {
		return Prepared{}, err
	}
	digest, err := comparisonDigest(prepared, plan.Models)
	if err != nil || digest != plan.ComparisonContextDigest {
		return Prepared{}, errors.New("frozen comparison context differs from plan")
	}
	var slots []paired.PairSlot
	for _, slot := range prepared.Preview.Slots {
		slots = append(slots, paired.PairSlot{Variant: slot.Variant, Case: slot.Case, Replicate: slot.Replicate})
	}
	if !slices.Equal(slots, plan.Slots) {
		return Prepared{}, errors.New("frozen pair slots differ from plan")
	}
	if err := validateProgramCoverage(prepared); err != nil {
		return Prepared{}, err
	}
	return prepared, nil
}

func validateProgramCoverage(prepared Prepared) error {
	if prepared.Config.ExecutionMode != "fixture" {
		return nil
	}
	if prepared.Program == nil {
		return errors.New("fixture program is required")
	}
	available := map[string]bool{}
	for _, script := range prepared.Program.Scripts {
		if script.Attempt == 1 {
			available[script.Stage+"/"+script.VariantSlotID+"/"+script.CaseID] = true
		}
	}
	variants := []string{"baseline"}
	if prepared.Config.Mode == "manual" {
		for _, candidate := range prepared.Config.CandidateDirs {
			variants = append(variants, candidate.ID)
		}
	} else {
		variants = append(variants, "procedure", "domain", "composition")
		for _, stage := range []string{"procedure", "domain", "compose", "reflect"} {
			if !available[stage+"//"] {
				return fmt.Errorf("fixture program missing initial %s script", stage)
			}
		}
	}
	for _, variant := range variants {
		for _, item := range prepared.Fixture.Cases {
			if !available["executor/"+variant+"/"+item.ID] {
				return fmt.Errorf("fixture program missing initial executor script for %s/%s", variant, item.ID)
			}
		}
	}
	return nil
}

func validateFrozenConfiguration(cfg paired.Config, fixture paired.Fixture, plan paired.ExperimentPlan) error {
	if err := fixture.Validate(); err != nil {
		return err
	}
	if err := cfg.ValidateEnvelope(len(fixture.Cases)); err != nil {
		return err
	}
	if cfg.ExperimentID != plan.ExperimentID || cfg.Mode != plan.Mode || cfg.ExecutionMode != plan.ExecutionMode ||
		cfg.Profile != plan.Profile || cfg.Limits != plan.Limits || !reflect.DeepEqual(cfg.Objective, plan.Objective) {
		return errors.New("frozen config differs from committed plan")
	}
	return nil
}

func FrozenBindings(objects Objects, plan paired.ExperimentPlan) ([]VariantBinding, error) {
	if len(plan.Inputs) != 2 || plan.Inputs[1].Kind != "frozen_variants" {
		return nil, errors.New("initial variant closure is missing")
	}
	var bindings []VariantBinding
	if err := readObjectJSON(objects, plan.Inputs[1], &bindings); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	planned := map[string]bool{}
	for _, slot := range plan.Slots {
		planned[slot.Variant] = true
	}
	for _, binding := range bindings {
		if !planned[binding.Slot] || seen[binding.Slot] || binding.Origin != "frozen_input" || binding.Snapshot.Validate("team_snapshot") != nil {
			return nil, errors.New("invalid initial variant closure")
		}
		seen[binding.Slot] = true
	}
	if !seen["baseline"] || plan.Mode == "manual" && len(bindings) != 2 || plan.Mode == "refine" && len(bindings) != 1 {
		return nil, errors.New("incomplete initial variant closure")
	}
	return bindings, nil
}
