package evalharness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/improve"
)

func TestEvalFixtureStrictDecode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cases.yaml")
	const badYAML = `version: 1
name: bad-fixture
team: ./team
mode: deterministic
cases:
  - id: c1
    prompt: "hi"
    provider-fixture: fixtures/c1.json
    expectt:
      run-outcome: completed
`
	if err := os.WriteFile(path, []byte(badYAML), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := LoadSuiteFixture(path); err == nil {
		t.Fatal("LoadSuiteFixture: expected error for misspelled field \"expectt\", got nil")
	}
}

func TestEvalSuiteReusesImproveBenchmarkFixture(t *testing.T) {
	fixturePath := filepath.Join(repoRoot(t), "evals", "core-lifecycle", "cases.yaml")
	fixture, err := LoadSuiteFixture(fixturePath)
	if err != nil {
		t.Fatalf("LoadSuiteFixture: %v", err)
	}
	benchmark := fixture.BenchmarkFixture()
	path, revision, err := improve.CreateBenchmark(t.TempDir(), benchmark)
	if err != nil {
		t.Fatalf("CreateBenchmark: %v", err)
	}
	loaded, err := improve.LoadBenchmark(path)
	if err != nil {
		t.Fatalf("LoadBenchmark: %v", err)
	}
	if got := improve.BenchmarkRevision(loaded); got != revision {
		t.Fatalf("loaded benchmark revision = %q, want %q", got, revision)
	}
	if len(loaded.Cases) != 1 || loaded.Cases[0].Prompt != fixture.Cases[0].Prompt {
		t.Fatalf("benchmark cases = %#v, want suite prompt preserved", loaded.Cases)
	}
}

func TestDiscoverSuiteFixturesFromEvalRoot(t *testing.T) {
	paths, err := DiscoverSuiteFixtures(filepath.Join(repoRoot(t), "evals"))
	if err != nil {
		t.Fatalf("DiscoverSuiteFixtures: %v", err)
	}
	if len(paths) != 22 {
		t.Fatalf("len(paths) = %d, want 22 suite fixtures: %v", len(paths), paths)
	}
	for _, path := range paths {
		if filepath.Base(path) != "cases.yaml" {
			t.Fatalf("discovered nested non-suite YAML %q", path)
		}
	}
}

func TestEvalFixtureStrictDecodeAcceptsValidFixture(t *testing.T) {
	fixturePath := filepath.Join(repoRoot(t), "evals", "core-lifecycle", "cases.yaml")
	fixture, err := LoadSuiteFixture(fixturePath)
	if err != nil {
		t.Fatalf("LoadSuiteFixture: %v", err)
	}
	if fixture.Name != "core-lifecycle" {
		t.Errorf("Name = %q, want %q", fixture.Name, "core-lifecycle")
	}
	if len(fixture.Cases) != 1 {
		t.Fatalf("len(Cases) = %d, want 1", len(fixture.Cases))
	}
}

func TestEvalFixtureRejectsUnsupportedVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cases.yaml")
	const yamlDoc = `version: 2
name: future-fixture
team: ./team
mode: deterministic
cases: []
`
	if err := os.WriteFile(path, []byte(yamlDoc), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := LoadSuiteFixture(path); err == nil {
		t.Fatal("LoadSuiteFixture: expected error for unsupported version 2, got nil")
	}
}

func TestEvalFixtureRejectsDuplicateCaseID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cases.yaml")
	const yamlDoc = `version: 1
name: dup-fixture
team: ./team
mode: deterministic
cases:
  - id: same
    prompt: "a"
    provider-fixture: fixtures/a.json
  - id: same
    prompt: "b"
    provider-fixture: fixtures/b.json
`
	if err := os.WriteFile(path, []byte(yamlDoc), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := LoadSuiteFixture(path); err == nil {
		t.Fatal("LoadSuiteFixture: expected error for duplicate case id, got nil")
	}
}

func TestEvalFixtureRejectsInvalidAssertionState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cases.yaml")
	const yamlDoc = `version: 1
name: invalid-assertion
team: ./team
mode: deterministic
cases:
  - id: c1
    prompt: hi
    provider-fixture: fixtures/c1.json
    expect:
      tasks:
        - verification: maybe
`
	if err := os.WriteFile(path, []byte(yamlDoc), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := LoadSuiteFixture(path); err == nil {
		t.Fatal("LoadSuiteFixture: expected error for invalid verification state, got nil")
	}
}

func TestEvalFixtureRejectsAmbiguousEvidenceSelector(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cases.yaml")
	const yamlDoc = `version: 1
name: invalid-evidence
team: ./team
mode: deterministic
cases:
  - id: c1
    prompt: hi
    provider-fixture: fixtures/c1.json
    expect:
      evidence:
        required-results:
          - requirement-id: task:1
            task-index: 0
`
	if err := os.WriteFile(path, []byte(yamlDoc), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := LoadSuiteFixture(path); err == nil {
		t.Fatal("LoadSuiteFixture: expected error for ambiguous evidence selector, got nil")
	}
}

func TestProviderFixtureRequiresExactlyOneValidStepResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "provider.json")
	const invalid = `{"steps":[{"content":"text","tool_call":{"name":"finish","arguments":"{}"}}]}`
	if err := os.WriteFile(path, []byte(invalid), 0o644); err != nil {
		t.Fatalf("write provider fixture: %v", err)
	}
	if _, err := LoadProviderFixture(path); err == nil {
		t.Fatal("LoadProviderFixture accepted a step with both content and tool_call")
	}

	const invalidArguments = `{"steps":[{"tool_call":{"name":"finish","arguments":"[]"}}]}`
	if err := os.WriteFile(path, []byte(invalidArguments), 0o644); err != nil {
		t.Fatalf("rewrite provider fixture: %v", err)
	}
	if _, err := LoadProviderFixture(path); err == nil {
		t.Fatal("LoadProviderFixture accepted non-object tool arguments")
	}
}

func TestProviderFixtureRejectsTrailingJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider.json")
	if err := os.WriteFile(path, []byte(`{"steps":[{"content":"done"}]} {}`), 0o644); err != nil {
		t.Fatalf("write provider fixture: %v", err)
	}
	if _, err := LoadProviderFixture(path); err == nil {
		t.Fatal("LoadProviderFixture accepted a trailing JSON value")
	}
}

func TestProviderFixtureForbiddenStepRequiresMatch(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		wantErr bool
	}{
		{name: "forbidden with match", fixture: `{"steps":[{"match":{"contains":"secret"},"forbidden":true,"content":"leaked"},{"content":"done"}]}`},
		{name: "forbidden without match", fixture: `{"steps":[{"forbidden":true,"content":"leaked"}]}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "provider.json")
			if err := os.WriteFile(path, []byte(tt.fixture), 0o644); err != nil {
				t.Fatalf("write provider fixture: %v", err)
			}
			if _, err := LoadProviderFixture(path); (err != nil) != tt.wantErr {
				t.Fatalf("LoadProviderFixture error = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}

// A forbidden step is a trap: consuming it is a finding, leaving it unused
// is the expected outcome. Ordinary steps keep the unused-step finding.
func TestScriptFindingsReportUnusedStepsAndConsumedTraps(t *testing.T) {
	trap := ProviderStep{Match: &StepMatch{Contains: "secret"}, Forbidden: true, Content: "leaked"}
	tests := []struct {
		name       string
		requests   []string
		wantDims   []string
		wantActual string
	}{
		{name: "trap untouched and script complete", requests: []string{"hello"}},
		{name: "trap consumed", requests: []string{"the secret value", "hello"}, wantDims: []string{"provider-fixture"}, wantActual: `a request contained "secret"`},
		{name: "ordinary step unused", wantDims: []string{"provider-fixture"}, wantActual: "unused step"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newScriptedProvider(&ProviderFixture{Steps: []ProviderStep{trap, {Content: "done"}}})
			for _, request := range tt.requests {
				if _, err := provider.nextStep([]byte(request)); err != nil {
					t.Fatalf("nextStep(%q): %v", request, err)
				}
			}
			findings := provider.scriptFindings()
			if len(findings) != len(tt.wantDims) {
				t.Fatalf("findings = %+v, want %d", findings, len(tt.wantDims))
			}
			for i, finding := range findings {
				if finding.Dimension != tt.wantDims[i] || !strings.Contains(finding.Actual, tt.wantActual) {
					t.Fatalf("finding %d = %+v, want dimension %q containing %q", i, finding, tt.wantDims[i], tt.wantActual)
				}
			}
		})
	}
}
