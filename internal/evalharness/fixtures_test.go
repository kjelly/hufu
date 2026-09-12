package evalharness

import (
	"os"
	"path/filepath"
	"testing"
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
