package evalharness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/improve"
	"gopkg.in/yaml.v3"
)

const workflowRegressionCategory = "workflow-regression"

// LoadSuiteFixture strict-decodes one suite YAML file. Strict decoding
// (KnownFields) fails a fixture that misspells a key instead of silently
// ignoring it -- see TestEvalFixtureStrictDecode.
func LoadSuiteFixture(path string) (*SuiteFixture, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read suite fixture %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var fixture SuiteFixture
	if err := dec.Decode(&fixture); err != nil {
		return nil, fmt.Errorf("decode suite fixture %s: %w", path, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode suite fixture %s: multiple YAML documents are not allowed", path)
		}
		return nil, fmt.Errorf("decode suite fixture %s trailing document: %w", path, err)
	}
	if fixture.Version != 1 {
		return nil, fmt.Errorf("suite fixture %s: unsupported version %d (want 1)", path, fixture.Version)
	}
	if fixture.Mode != "deterministic" {
		return nil, fmt.Errorf("suite fixture %s: unsupported mode %q (want %q)", path, fixture.Mode, "deterministic")
	}
	if strings.TrimSpace(fixture.Name) == "" {
		return nil, fmt.Errorf("suite fixture %s: name is required", path)
	}
	if strings.TrimSpace(fixture.Team) == "" {
		return nil, fmt.Errorf("suite fixture %s: team is required", path)
	}
	seen := make(map[string]bool, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if strings.TrimSpace(c.ID) == "" {
			return nil, fmt.Errorf("suite fixture %s: case with empty id", path)
		}
		if seen[c.ID] {
			return nil, fmt.Errorf("suite fixture %s: duplicate case id %q", path, c.ID)
		}
		if strings.TrimSpace(c.Prompt) == "" {
			return nil, fmt.Errorf("suite fixture %s: case %q: prompt is required", path, c.ID)
		}
		if err := validateExpectSpec(c.Expect); err != nil {
			return nil, fmt.Errorf("suite fixture %s: case %q: %w", path, c.ID, err)
		}
		seen[c.ID] = true
	}
	fixture.Path = path
	return &fixture, nil
}

// BenchmarkFixture projects the authored prompts onto the repository's
// existing improve benchmark contract. Provider scripts and runtime
// assertions remain eval-only data; the shared fixture supplies the canonical
// prompt-set revision used in reports and future baseline/candidate compares.
func (s *SuiteFixture) BenchmarkFixture() improve.BenchmarkFixture {
	cases := make([]improve.BenchmarkCase, 0, len(s.Cases))
	for _, c := range s.Cases {
		cases = append(cases, improve.BenchmarkCase{ID: c.ID, Type: "edge", Prompt: c.Prompt})
	}
	return improve.BenchmarkFixture{
		Version:  1,
		Name:     s.Name,
		Team:     s.Name,
		Category: workflowRegressionCategory,
		Cases:    cases,
	}
}

func validateExpectSpec(expect ExpectSpec) error {
	switch expect.AuditVerdict {
	case "", "pass", "fail", "incomplete":
	default:
		return fmt.Errorf("audit-verdict: unsupported verdict %q", expect.AuditVerdict)
	}
	for index, task := range expect.Tasks {
		switch task.Verification {
		case "", "not-run", "passed", "failed":
		default:
			return fmt.Errorf("tasks[%d].verification: unsupported state %q", index, task.Verification)
		}
		if task.BackendBinding != nil && strings.TrimSpace(task.BackendBinding.Backend) == "" {
			return fmt.Errorf("tasks[%d].backend-binding.backend is required", index)
		}
	}
	eventGroups := []struct {
		name   string
		events EventsExpect
	}{{"events", expect.Events}, {"durable-events", expect.DurableEvents}}
	for _, group := range eventGroups {
		name, events := group.name, group.events
		for index, count := range events.Counts {
			if strings.TrimSpace(count.Type) == "" {
				return fmt.Errorf("%s.counts[%d].type is required", name, index)
			}
			if count.Count < 0 {
				return fmt.Errorf("%s.counts[%d].count must be non-negative", name, index)
			}
		}
		for index, match := range events.Matches {
			if strings.TrimSpace(match.Type) == "" {
				return fmt.Errorf("%s.matches[%d].type is required", name, index)
			}
			if match.Occurrence < 0 {
				return fmt.Errorf("%s.matches[%d].occurrence must be positive when set", name, index)
			}
			if len(match.Fields) == 0 {
				return fmt.Errorf("%s.matches[%d].fields is required", name, index)
			}
		}
	}
	if expect.Evidence != nil {
		if expect.Evidence.MinArtifactRefs != nil && *expect.Evidence.MinArtifactRefs < 0 {
			return fmt.Errorf("evidence.min-artifact-refs must be non-negative")
		}
		for index, result := range expect.Evidence.RequiredResults {
			selectors := 0
			if strings.TrimSpace(result.RequirementID) != "" {
				selectors++
			}
			if result.TaskIndex != nil {
				selectors++
				if *result.TaskIndex < 0 {
					return fmt.Errorf("evidence.required-results[%d].task-index must be non-negative", index)
				}
			}
			if selectors != 1 {
				return fmt.Errorf("evidence.required-results[%d] requires exactly one of requirement-id or task-index", index)
			}
			if result.MinArtifactRefs != nil && *result.MinArtifactRefs < 0 {
				return fmt.Errorf("evidence.required-results[%d].min-artifact-refs must be non-negative", index)
			}
		}
		for index, ref := range expect.Evidence.RequiredArtifactRefs {
			if ref.MinCount <= 0 {
				return fmt.Errorf("evidence.required-artifact-refs[%d].min-count must be positive", index)
			}
			if ref.TaskIndex != nil && *ref.TaskIndex < 0 {
				return fmt.Errorf("evidence.required-artifact-refs[%d].task-index must be non-negative", index)
			}
		}
	}
	return nil
}

// TeamDir resolves the suite's Team path relative to the fixture file.
func (s *SuiteFixture) TeamDir() string {
	return resolveFixtureRelative(s.Path, s.Team)
}

// ProviderFixturePath resolves a case's ProviderFixture path relative to the
// suite fixture file that declared it.
func (s *SuiteFixture) ProviderFixturePath(c CaseFixture) string {
	return resolveFixtureRelative(s.Path, c.ProviderFixture)
}

func resolveFixtureRelative(fixturePath, target string) string {
	if target == "" {
		return ""
	}
	if filepath.IsAbs(target) {
		return target
	}
	return filepath.Join(filepath.Dir(fixturePath), target)
}

// LoadProviderFixture strict-decodes a case's provider-fixture JSON file.
func LoadProviderFixture(path string) (*ProviderFixture, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read provider fixture %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var fixture ProviderFixture
	if err := dec.Decode(&fixture); err != nil {
		return nil, fmt.Errorf("decode provider fixture %s: %w", path, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode provider fixture %s: trailing JSON value is not allowed", path)
		}
		return nil, fmt.Errorf("decode provider fixture %s trailing value: %w", path, err)
	}
	if len(fixture.Steps) == 0 {
		return nil, fmt.Errorf("provider fixture %s: at least one step is required", path)
	}
	for index, step := range fixture.Steps {
		hasContent := step.Content != ""
		hasToolCall := step.ToolCall != nil
		if hasContent == hasToolCall {
			return nil, fmt.Errorf("provider fixture %s: steps[%d] requires exactly one non-empty content or tool_call", path, index)
		}
		if step.Match != nil && strings.TrimSpace(step.Match.Contains) == "" {
			return nil, fmt.Errorf("provider fixture %s: steps[%d].match.contains is required", path, index)
		}
		if step.ToolCall == nil {
			continue
		}
		if strings.TrimSpace(step.ToolCall.Name) == "" {
			return nil, fmt.Errorf("provider fixture %s: steps[%d].tool_call.name is required", path, index)
		}
		var arguments map[string]json.RawMessage
		if err := json.Unmarshal([]byte(step.ToolCall.Arguments), &arguments); err != nil || arguments == nil {
			return nil, fmt.Errorf("provider fixture %s: steps[%d].tool_call.arguments must be one JSON object", path, index)
		}
	}
	return &fixture, nil
}

// DiscoverSuiteFixtures finds every *.yaml/*.yml suite fixture directly under
// dir. If dir itself has no fixture, it treats dir as a suite root and searches
// each immediate child directory. It never descends farther, so a suite's
// team/team.yaml is not mistaken for a suite fixture.
func DiscoverSuiteFixtures(dir string) ([]string, error) {
	paths, entries, err := discoverDirectSuiteFixtures(dir)
	if err != nil {
		return nil, err
	}
	if len(paths) > 0 {
		return paths, nil
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		childPaths, _, childErr := discoverDirectSuiteFixtures(filepath.Join(dir, entry.Name()))
		if childErr != nil {
			return nil, childErr
		}
		paths = append(paths, childPaths...)
	}
	sort.Strings(paths)
	return paths, nil
}

func discoverDirectSuiteFixtures(dir string) ([]string, []os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("read suite directory %s: %w", dir, err)
	}
	var paths []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		paths = append(paths, filepath.Join(dir, entry.Name()))
	}
	sort.Strings(paths)
	return paths, entries, nil
}
