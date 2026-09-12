package evalharness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

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
	if fixture.Version != 1 {
		return nil, fmt.Errorf("suite fixture %s: unsupported version %d (want 1)", path, fixture.Version)
	}
	if fixture.Mode != "deterministic" {
		return nil, fmt.Errorf("suite fixture %s: unsupported mode %q (want %q)", path, fixture.Mode, "deterministic")
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
		if err := validateExpectSpec(c.Expect); err != nil {
			return nil, fmt.Errorf("suite fixture %s: case %q: %w", path, c.ID, err)
		}
		seen[c.ID] = true
	}
	fixture.Path = path
	return &fixture, nil
}

func validateExpectSpec(expect ExpectSpec) error {
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
	return &fixture, nil
}

// DiscoverSuiteFixtures finds every *.yaml/*.yml suite fixture directly
// under dir (suite subdirectories are not searched recursively: each suite
// directory is expected to hold its own fixture file(s) alongside its
// bundled team and provider-fixture files), sorted by path for stable
// output.
func DiscoverSuiteFixtures(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read suite directory %s: %w", dir, err)
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
	return paths, nil
}
