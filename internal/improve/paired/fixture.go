package paired

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
)

type Fixture struct {
	SchemaVersion int    `yaml:"schema_version" json:"schema_version"`
	ID            string `yaml:"id" json:"id"`
	ResultSchema  string `yaml:"result_schema" json:"result_schema"`
	Cases         []Case `yaml:"cases" json:"cases"`
}

type Case struct {
	ID       string   `yaml:"id" json:"id"`
	Input    string   `yaml:"input" json:"input"`
	Critical bool     `yaml:"critical" json:"critical"`
	Verify   Verifier `yaml:"verify" json:"verify"`
}

// Verifier deliberately exposes only the task-result assertion language.
type Verifier struct {
	Type       string                      `yaml:"type" json:"type"`
	Mode       string                      `yaml:"mode" json:"mode"`
	Assertions []agent.TaskResultAssertion `yaml:"task-result-assertions" json:"task_result_assertions"`
}

func (v Verifier) RuntimeSpec() agent.VerificationSpec {
	// The paired contract calls the gate "blocking"; the task verifier's
	// equivalent mode is "success" (as opposed to observation/expected_failure).
	mode := v.Mode
	if mode == "blocking" {
		mode = "success"
	}
	assertions := slices.Clone(v.Assertions)
	for i := range assertions {
		// The fixture addresses the submitted value. Canonical TaskResult
		// wraps it with the runtime-owned schema identity and content digest.
		if assertions[i].Pointer == "/structured_payload" || strings.HasPrefix(assertions[i].Pointer, "/structured_payload/") {
			assertions[i].Pointer = "/structured_payload/value" + strings.TrimPrefix(assertions[i].Pointer, "/structured_payload")
		}
	}
	return agent.VerificationSpec{Type: agent.VerificationType(v.Type), Mode: mode, TaskResultAssertions: assertions}
}

func ParseFixture(raw []byte) (Fixture, error) {
	var fixture Fixture
	if err := DecodeYAML(raw, &fixture); err != nil {
		return Fixture{}, err
	}
	if err := fixture.Validate(); err != nil {
		return Fixture{}, err
	}
	return fixture, nil
}

func (f Fixture) Validate() error {
	if f.SchemaVersion != SchemaVersion || !ValidID(f.ID) || strings.TrimSpace(f.ResultSchema) == "" {
		return errors.New("fixture: requires schema_version 1, valid id and result_schema")
	}
	if len(f.Cases) < 1 || len(f.Cases) > MaxCases {
		return fmt.Errorf("fixture.cases: must contain between 1 and %d entries", MaxCases)
	}
	seen := map[string]bool{}
	for i, item := range f.Cases {
		if !ValidID(item.ID) || seen[item.ID] || strings.TrimSpace(item.Input) == "" {
			return fmt.Errorf("fixture.cases[%d]: requires unique valid id and input", i)
		}
		seen[item.ID] = true
		if err := item.Verify.Validate(); err != nil {
			return fmt.Errorf("fixture.cases[%d].verify: %w", i, err)
		}
	}
	return nil
}

func (v Verifier) Validate() error {
	if v.Type != "task_result_assert" || v.Mode != "blocking" {
		return errors.New("requires blocking task_result_assert")
	}
	for i, assertion := range v.Assertions {
		if assertion.Pointer != "/structured_payload" && !strings.HasPrefix(assertion.Pointer, "/structured_payload/") {
			return fmt.Errorf("assertion %d: pointer must address structured_payload", i)
		}
		if !slices.Contains([]string{"exists", "non_empty", "equals", "min_items", "contains_scalar"}, assertion.Op) {
			return fmt.Errorf("assertion %d: unsupported op %q", i, assertion.Op)
		}
	}
	return team.ValidateVerificationSpec(v.RuntimeSpec())
}
