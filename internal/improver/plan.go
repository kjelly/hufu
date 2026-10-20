package improver

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/improve/paired"
)

type BuildIdentity struct {
	BinaryDigest   string `json:"binary_digest"`
	SourceRevision string `json:"source_revision,omitempty"`
}

func CurrentBuildIdentity() (BuildIdentity, error) {
	executable, err := os.Executable()
	if err != nil {
		return BuildIdentity{}, err
	}
	file, err := os.Open(executable)
	if err != nil {
		return BuildIdentity{}, err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return BuildIdentity{}, err
	}
	result := BuildIdentity{BinaryDigest: hex.EncodeToString(hash.Sum(nil))}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				result.SourceRevision = setting.Value
			}
		}
	}
	return result, nil
}

type Slot struct {
	Variant   string `json:"variant_slot_id"`
	Case      string `json:"case_id"`
	Replicate int64  `json:"replicate"`
}

type VariantPreview struct {
	ID              string `json:"variant_slot_id"`
	VariantDigest   string `json:"variant_digest"`
	FixedTeamDigest string `json:"fixed_team_digest"`
	Inventory       []File `json:"inventory"`
}

type PlanPreview struct {
	Kind                    string                  `json:"kind"`
	SchemaVersion           int                     `json:"schema_version"`
	ExperimentID            string                  `json:"experiment_id"`
	ExecutionMode           string                  `json:"execution_mode"`
	Mode                    string                  `json:"mode"`
	RuntimePreflight        string                  `json:"runtime_preflight"`
	Executable              bool                    `json:"executable"`
	Build                   BuildIdentity           `json:"build"`
	ConfigDigest            string                  `json:"config_digest"`
	ComparisonContextDigest string                  `json:"comparison_context_digest"`
	Models                  map[string]paired.Model `json:"requested_models"`
	Surfaces                []ResolvedSurface       `json:"surfaces"`
	Variants                []VariantPreview        `json:"variants"`
	Inputs                  []File                  `json:"inputs"`
	Slots                   []Slot                  `json:"slots"`
	Limits                  paired.Limits           `json:"limits"`
	Objective               paired.Objective        `json:"objective"`
}

// Prepared owns frozen content for a later explicit run. Preview alone is not
// executable: it has no runtime policy, parent branch or child identity.
type Prepared struct {
	Preview  PlanPreview
	Config   paired.Config
	Fixture  paired.Fixture
	Program  *paired.Program
	Variants map[string]Variant
	Inputs   Tree
	Schema   []byte
}

func Prepare(configPath string, build BuildIdentity) (Prepared, error) {
	if err := (paired.ArtifactRef{Kind: "binary", Digest: build.BinaryDigest}).Validate("binary"); err != nil {
		return Prepared{}, fmt.Errorf("build identity: %w", err)
	}
	absolute, err := filepath.Abs(configPath)
	if err != nil {
		return Prepared{}, err
	}
	root, err := openSourceRoot(filepath.Dir(absolute))
	if err != nil {
		return Prepared{}, err
	}
	defer func() { _ = root.Close() }()
	raw, err := readRegular(root, filepath.Base(absolute), paired.MaxDataBytes)
	if err != nil {
		return Prepared{}, err
	}
	cfg, err := paired.ParseConfig(raw)
	if err != nil {
		return Prepared{}, err
	}
	result := Prepared{Config: cfg, Variants: map[string]Variant{}, Inputs: Tree{files: map[string][]byte{"config.yaml": raw}}}
	result.Preview = PlanPreview{
		Kind: "paired_plan_preview", SchemaVersion: 1, ExperimentID: cfg.ExperimentID,
		ExecutionMode: cfg.ExecutionMode, Mode: cfg.Mode, RuntimePreflight: "pending",
		Build: build, Models: maps.Clone(cfg.Models), Limits: cfg.Limits, Objective: cfg.Objective,
	}
	result.Preview.ConfigDigest, err = paired.Digest("config", cfg)
	if err != nil {
		return Prepared{}, err
	}
	if err := result.captureInputs(root); err != nil {
		return Prepared{}, err
	}
	if err := cfg.ValidateEnvelope(len(result.Fixture.Cases)); err != nil {
		return Prepared{}, err
	}
	if err := result.captureVariants(filepath.Dir(absolute), build.BinaryDigest); err != nil {
		return Prepared{}, err
	}
	if err := result.finishPreview(); err != nil {
		return Prepared{}, err
	}
	return result, nil
}

func inputPath(value string) (string, error) {
	name := strings.TrimPrefix(value, "./")
	if !paired.SafeRelativePath(name) {
		return "", fmt.Errorf("input %q: normalized relative path required", value)
	}
	return name, nil
}

func (p *Prepared) captureInputs(root *os.Root) error {
	name, err := inputPath(p.Config.Fixture)
	if err != nil {
		return err
	}
	raw, err := readRegular(root, name, paired.MaxDataBytes)
	if err != nil {
		return err
	}
	p.Inputs.files["fixture/"+name] = raw
	p.Fixture, err = paired.ParseFixture(raw)
	if err != nil {
		return err
	}
	fixtureRoot, err := root.OpenRoot(filepath.FromSlash(path.Dir(name)))
	if err != nil {
		return err
	}
	defer func() { _ = fixtureRoot.Close() }()
	schemaName, err := inputPath(p.Fixture.ResultSchema)
	if err != nil {
		return err
	}
	schema, closure, err := captureSchema(fixtureRoot, schemaName)
	if err != nil {
		return err
	}
	p.Schema = schema
	for key, value := range closure.files {
		p.Inputs.files["schema/"+key] = value
	}
	p.Inputs.files["compiled/paired.schema.json"] = schema
	for _, item := range p.Fixture.Cases {
		input, err := inputPath(item.Input)
		if err != nil {
			return err
		}
		text, err := readRegular(fixtureRoot, input, paired.MaxTextBytes)
		if err != nil {
			return err
		}
		if !utf8.Valid(text) || strings.TrimSpace(string(text)) == "" {
			return fmt.Errorf("case %s: nonempty UTF-8 input required", item.ID)
		}
		p.Inputs.files["cases/"+item.ID+".txt"] = text
	}
	return p.captureAuxiliaryInputs(root)
}

func (p *Prepared) captureAuxiliaryInputs(root *os.Root) error {
	if p.Config.ExecutionMode == "fixture" {
		name, err := inputPath(p.Config.FixtureProgram)
		if err != nil {
			return err
		}
		raw, err := readRegular(root, name, paired.MaxDataBytes)
		if err != nil {
			return err
		}
		program, err := paired.ParseProgram(raw)
		if err != nil {
			return err
		}
		p.Program = &program
		p.Inputs.files["fixture_program.json"] = raw
	}
	if prompts := p.Config.Refinement; prompts != nil {
		for role, source := range map[string]string{
			"procedure": prompts.ProcedurePrompt, "domain": prompts.DomainPrompt,
			"compose": prompts.ComposerPrompt, "reflect": prompts.ReflectorPrompt,
		} {
			name, err := inputPath(source)
			if err != nil {
				return err
			}
			raw, err := readRegular(root, name, paired.MaxTextBytes)
			if err != nil {
				return err
			}
			if err := validateReplacement(string(raw)); err != nil {
				return fmt.Errorf("%s prompt: %w", role, err)
			}
			p.Inputs.files["prompts/"+role+".txt"] = raw
		}
	}
	return nil
}

func (p *Prepared) captureVariants(configDir, buildDigest string) error {
	sourcePath := func(name string) string {
		if filepath.IsAbs(name) {
			return name
		}
		return filepath.Join(configDir, name)
	}
	baseline, surfaces, err := Materialize(sourcePath(p.Config.BaselineDir), p.Config.Vars, p.Config.Worker, buildDigest, p.Config.AllowedSurfaces)
	if err != nil {
		return fmt.Errorf("baseline: %w", err)
	}
	p.Variants["baseline"] = baseline
	p.Preview.Surfaces = surfaces
	for _, declared := range p.Config.CandidateDirs {
		candidate, _, err := Materialize(sourcePath(declared.Path), p.Config.Vars, p.Config.Worker, buildDigest, p.Config.AllowedSurfaces)
		if err != nil {
			return fmt.Errorf("candidate %s: %w", declared.ID, err)
		}
		if err := CompareCandidate(baseline, candidate, surfaces); err != nil {
			return fmt.Errorf("candidate %s: %w", declared.ID, err)
		}
		p.Variants[declared.ID] = candidate
	}
	return nil
}

func (p *Prepared) finishPreview() error {
	names := []string{"baseline"}
	if p.Config.Mode == "manual" {
		names = append(names, p.Config.CandidateDirs[0].ID)
	} else {
		names = append(names, "procedure", "domain", "composition")
	}
	var total int64
	count := len(p.Inputs.files)
	for _, item := range p.Inputs.Inventory() {
		total += item.Size
	}
	for _, name := range names {
		if v, exists := p.Variants[name]; exists {
			p.Preview.Variants = append(p.Preview.Variants, VariantPreview{name, v.VariantDigest, v.FixedDigest, v.Inventory})
			count += len(v.Inventory)
			for _, item := range v.Inventory {
				total += item.Size
			}
		}
		for _, item := range p.Fixture.Cases {
			for replicate := int64(1); replicate <= p.Config.Replicates; replicate++ {
				p.Preview.Slots = append(p.Preview.Slots, Slot{name, item.ID, replicate})
			}
		}
	}
	if count > paired.MaxFiles || total > paired.MaxTotalBytes {
		return errors.New("plan: total closure limits exceeded")
	}
	p.Preview.Inputs = p.Inputs.Inventory()
	// Config source paths and candidate bytes are intentionally absent from
	// this logical comparison projection. The committed runtime plan later
	// adds actual generation, policy and identity bindings.
	fixedInputs := slices.DeleteFunc(slices.Clone(p.Preview.Inputs), func(file File) bool { return file.Path == "config.yaml" })
	digest, err := paired.Digest("comparison_preview", map[string]any{
		"inputs": fixedInputs, "fixture": p.Fixture, "fixed_team": p.Variants["baseline"].FixedDigest,
		"models": p.Config.Models, "limits": p.Config.Limits, "objective": p.Config.Objective,
		"replicates": p.Config.Replicates, "build": p.Preview.Build,
		"profile": p.Config.Profile, "execution_mode": p.Config.ExecutionMode,
	})
	if err != nil {
		return err
	}
	p.Preview.ComparisonContextDigest = digest
	return nil
}
