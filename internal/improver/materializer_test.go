package improver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/improve/paired"
)

var testBuild = BuildIdentity{BinaryDigest: strings.Repeat("a", 64)}

func putFile(t *testing.T, dir, name, text string) {
	t.Helper()
	target := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareFullOfflineExample(t *testing.T) {
	prepared, err := Prepare("testdata/extraction/experiment.yaml", testBuild)
	if err != nil {
		t.Fatal(err)
	}
	p := prepared.Preview
	if p.Executable || p.RuntimePreflight != "pending" || len(p.Slots) != 2 || len(p.Surfaces) != 1 {
		t.Fatalf("invalid preview: %+v", p)
	}
	if p.Variants[0].VariantDigest == p.Variants[1].VariantDigest ||
		p.Variants[0].FixedTeamDigest != p.Variants[1].FixedTeamDigest {
		t.Fatal("prompt difference not isolated from fixed context")
	}
	copyRoot := filepath.Join(t.TempDir(), "copy")
	source, err := CaptureTree("testdata/extraction")
	if err != nil {
		t.Fatal(err)
	}
	if err := source.WriteNew(copyRoot); err != nil {
		t.Fatal(err)
	}
	other, err := Prepare(filepath.Join(copyRoot, "experiment.yaml"), testBuild)
	if err != nil {
		t.Fatal(err)
	}
	if other.Preview.ComparisonContextDigest != p.ComparisonContextDigest ||
		other.Preview.Variants[0].VariantDigest != p.Variants[0].VariantDigest {
		t.Fatal("physical root changed logical identity")
	}
	if _, err := os.Stat(filepath.Join(copyRoot, "workspace")); !os.IsNotExist(err) {
		t.Fatal("plan created a workspace")
	}
}

func TestCandidateFixedFieldsAndSharedAnchors(t *testing.T) {
	const base = `name: example
agents:
  worker:
    preset: readonly
    tools: {allowed: [submit_result], denied: [view, grep, glob, ls]}
    instructions: inspect
  observer:
    preset: readonly
    instructions: observe
`
	surface := []paired.Surface{{Kind: "inline_instructions", Agent: "worker"}}
	baselineDir := t.TempDir()
	putFile(t, baselineDir, "team.yaml", base)
	baseline, surfaces, err := Materialize(baselineDir, nil, "worker", testBuild.BinaryDigest, surface)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, text string
		valid      bool
	}{
		{"prompt", strings.Replace(base, "instructions: inspect", "instructions: inspect carefully", 1), true},
		{"other_worker", strings.Replace(base, "instructions: observe", "instructions: changed", 1), false},
		{"model", strings.Replace(base, "instructions: inspect", "model: other\n    instructions: inspect", 1), false},
		{"tools", strings.Replace(base, "[submit_result]", "[submit_result, bash]", 1), false},
		{"template", strings.Replace(base, "instructions: inspect", "instructions: '{@ .task @}'", 1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			putFile(t, dir, "team.yaml", test.text)
			candidate, _, err := Materialize(dir, nil, "worker", testBuild.BinaryDigest, surface)
			if err == nil {
				err = CompareCandidate(baseline, candidate, surfaces)
			}
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v error=%v", test.valid, err)
			}
		})
	}
}

func TestMarkdownFrontmatterAndInventoryAreProtected(t *testing.T) {
	surface := []paired.Surface{{Kind: "markdown_body", Agent: "worker"}}
	const agent = "---\nname: worker\ntools: submit_result\n---\nInspect.\n"
	dir := t.TempDir()
	putFile(t, dir, "worker.md", agent)
	base, surfaces, err := Materialize(dir, nil, "worker", testBuild.BinaryDigest, surface)
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, dir, "worker.md", strings.Replace(agent, "Inspect.", "Inspect carefully.", 1))
	candidate, _, err := Materialize(dir, nil, "worker", testBuild.BinaryDigest, surface)
	if err != nil || CompareCandidate(base, candidate, surfaces) != nil {
		t.Fatalf("valid Markdown change rejected: %v", err)
	}
	putFile(t, dir, "script.sh", "echo modified\n")
	candidate, _, err = Materialize(dir, nil, "worker", testBuild.BinaryDigest, surface)
	if err == nil && CompareCandidate(base, candidate, surfaces) == nil {
		t.Fatal("new fixed file accepted")
	}
}

func TestCaptureRejectsSymlinksAndTraversal(t *testing.T) {
	dir := t.TempDir()
	putFile(t, dir, "regular", "safe")
	if err := os.Symlink(filepath.Join(dir, "regular"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureTree(dir); err == nil {
		t.Fatal("symlink accepted")
	}
	root, err := openSourceRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := readRegular(root, "../outside", 100); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestInlineShorthandAndVersionedManifest(t *testing.T) {
	const header = "name: example\ntools: {allowed: [submit_result], denied: [view, grep, glob, ls]}\n"
	for _, versioned := range []bool{false, true} {
		wrap := func(spec string) string {
			if !versioned {
				return spec
			}
			return "apiVersion: hufu.io/v1alpha1\nkind: AgentTeam\nmetadata: {name: example}\nspec:\n  " +
				strings.ReplaceAll(strings.TrimSuffix(spec, "\n"), "\n", "\n  ") + "\n"
		}
		dir := t.TempDir()
		surface := []paired.Surface{{Kind: "inline_instructions", Agent: "worker"}}
		putFile(t, dir, "team.yaml", wrap(header+"agents: {worker: readonly}\n"))
		base, surfaces, err := Materialize(dir, nil, "worker", testBuild.BinaryDigest, surface)
		if err != nil {
			t.Fatal(err)
		}
		putFile(t, dir, "team.yaml", wrap(header+"agents: {worker: {preset: readonly, instructions: 'Submit the category.'}}\n"))
		candidate, _, err := Materialize(dir, nil, "worker", testBuild.BinaryDigest, surface)
		if err != nil {
			t.Fatal(err)
		}
		if err := CompareCandidate(base, candidate, surfaces); err != nil {
			t.Fatalf("versioned=%v shorthand expansion rejected: %v", versioned, err)
		}
	}
}

func TestSharedAnchorCannotChangeOtherWorker(t *testing.T) {
	const source = `name: shared
agents:
  worker: &definition
    preset: readonly
    tools: {allowed: [submit_result], denied: [view, grep, glob, ls]}
    instructions: Inspect.
  observer:
    <<: *definition
`
	dir := t.TempDir()
	surface := []paired.Surface{{Kind: "inline_instructions", Agent: "worker"}}
	putFile(t, dir, "team.yaml", source)
	base, surfaces, err := Materialize(dir, nil, "worker", testBuild.BinaryDigest, surface)
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, dir, "team.yaml", strings.Replace(source, "Inspect.", "Changed.", 1))
	candidate, _, err := Materialize(dir, nil, "worker", testBuild.BinaryDigest, surface)
	if err != nil {
		t.Fatal(err)
	}
	if CompareCandidate(base, candidate, surfaces) == nil {
		t.Fatal("shared anchor changed another worker undetected")
	}
}

func TestSkillBodyIsExplicitAndMetadataIsFixed(t *testing.T) {
	dir := t.TempDir()
	putFile(t, dir, "worker.md", "---\nname: worker\ntools: submit_result\nskills: guide\n---\nInspect.\n")
	const skillFile = "skills/guide/SKILL.md"
	const skill = "---\nname: guide\ndescription: Frozen guidance.\n---\nUse the requested category.\n"
	putFile(t, dir, skillFile, skill)
	surfaces := []paired.Surface{{Kind: "skill_body", Path: skillFile}}
	base, resolved, err := Materialize(dir, nil, "worker", testBuild.BinaryDigest, surfaces)
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, dir, skillFile, strings.Replace(skill, "Use the requested category.", "Return the category exactly.", 1))
	candidate, _, err := Materialize(dir, nil, "worker", testBuild.BinaryDigest, surfaces)
	if err != nil || CompareCandidate(base, candidate, resolved) != nil || candidate.VariantDigest == base.VariantDigest {
		t.Fatalf("skill body not variable: %v", err)
	}
	putFile(t, dir, skillFile, strings.Replace(skill, "Frozen guidance.", "Different trigger.", 1))
	candidate, _, err = Materialize(dir, nil, "worker", testBuild.BinaryDigest, surfaces)
	if err == nil && CompareCandidate(base, candidate, resolved) == nil {
		t.Fatal("skill metadata changed undetected")
	}
}

func TestFixedExperimentInputsChangeComparisonIdentity(t *testing.T) {
	base, err := Prepare("testdata/extraction/experiment.yaml", testBuild)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := CaptureTree("testdata/extraction")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, file, old, replacement string }{
		{"input", "inputs/case_a.txt", "allowed", "denied"},
		{"schema", "result.schema.json", `"enum": ["allowed", "denied"]`, `"enum": ["allowed", "denied", "unknown"]`},
		{"verifier", "cases.yaml", "value: allowed", "value: denied"},
		{"budget", "experiment.yaml", "per_stage_tokens: 12000", "per_stage_tokens: 11000"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "example")
			if err := tree.WriteNew(dir); err != nil {
				t.Fatal(err)
			}
			raw, _ := tree.Read(test.file)
			putFile(t, dir, test.file, strings.Replace(string(raw), test.old, test.replacement, 1))
			changed, err := Prepare(filepath.Join(dir, "experiment.yaml"), testBuild)
			if err != nil {
				t.Fatal(err)
			}
			if changed.Preview.ComparisonContextDigest == base.Preview.ComparisonContextDigest {
				t.Fatal("fixed comparison input change hidden")
			}
		})
	}
}

func TestSourceLimitsAndSanitization(t *testing.T) {
	dir := t.TempDir()
	putFile(t, dir, "oversized.txt", strings.Repeat("x", paired.MaxDataBytes+1))
	if _, err := CaptureTree(dir); err == nil {
		t.Fatal("oversized source accepted")
	}
	for _, sample := range []struct {
		name, body string
		allowed    bool
	}{
		{"config.yaml", "limits: {per_stage_tokens: 12, pipeline_tokens: 24}\n", true},
		{"config.yaml", "limits: {per_stage_tokens: private_credential}\n", false},
		{"config.json", `{"api_key":"private_credential_value"}`, false},
		{"prompt.txt", "Use <category> and return JSON.", true},
		{"config.yaml", "instructions: Use <category> and return JSON.\n", true},
	} {
		if err := checkSanitized(sample.name, []byte(sample.body)); (err == nil) != sample.allowed {
			t.Fatalf("%s allowed=%v: %v", sample.name, sample.allowed, err)
		}
	}
}

func TestTemplatesAreValidatedBeforeReferencedResourcesLoad(t *testing.T) {
	dir := t.TempDir()
	const manifest = `name: example
description: '{@ .description @}'
agents:
  worker:
    preset: readonly
    tools: {allowed: [submit_result], denied: [view, grep, glob, ls]}
    instructions: '{@ .task @}'
`
	putFile(t, dir, "team.yaml", manifest)
	surfaces := []paired.Surface{{Kind: "inline_instructions", Agent: "worker"}}
	vars := map[string]string{"description": "Fixed description.", "task": "Return the category."}
	base, resolved, err := Materialize(dir, vars, "worker", testBuild.BinaryDigest, surfaces)
	if err != nil {
		t.Fatal(err)
	}
	if base.Agents["worker"].System != vars["task"] {
		t.Fatal("baseline template was not rendered with frozen vars")
	}
	putFile(t, dir, "team.yaml", strings.Replace(manifest, "{@ .task @}", "Return the category exactly.", 1))
	candidate, _, err := Materialize(dir, vars, "worker", testBuild.BinaryDigest, surfaces)
	if err != nil || CompareCandidate(base, candidate, resolved) != nil {
		t.Fatalf("literal candidate replacing template rejected: %v", err)
	}
	vars["description"] = "injected'\naction-providers: {unsafe: {type: command, command: [forbidden]}}\n#"
	_, _, err = Materialize(dir, vars, "worker", testBuild.BinaryDigest, surfaces)
	if err == nil || !strings.Contains(err.Error(), "unsupported_profile: team.action-providers") {
		t.Fatalf("template capability was not rejected before resource compilation: %v", err)
	}
}

func TestWorkerNameAndSourceAliasAreDistinct(t *testing.T) {
	dir := t.TempDir()
	putFile(t, dir, "extract.md", "---\nname: Classifier\ntools: submit_result\n---\nReturn category.\n")
	variant, surfaces, err := Materialize(dir, nil, "Classifier", testBuild.BinaryDigest,
		[]paired.Surface{{Kind: "markdown_body", Agent: "Classifier"}})
	if err != nil {
		t.Fatal(err)
	}
	if surfaces[0].Path != "extract.md" || surfaces[0].Agent != "Classifier" ||
		variant.Agents["Classifier"].Name != "Classifier" || variant.Agents["Helper"].Name != "Helper" {
		t.Fatalf("lost name, alias or fixed roster: %+v, %+v", surfaces, variant.Agents)
	}
}
