package paired

import (
	"math"
	"strings"
	"testing"
)

const validConfig = `schema_version: 1
experiment_id: extraction_pair
mode: manual
execution_mode: fixture
profile: structured_only
baseline_dir: ./team
candidate_dirs: [{id: candidate_1, path: ./candidate}]
fixture: ./cases.yaml
worker: extractor
allowed_surfaces: [{kind: inline_instructions, agent: extractor}]
models: {executor: {backend: fixture, model: scripted}}
fixture_program: ./responses.json
limits:
  per_stage_tokens: 12000
  per_stage_seconds: 60
  pipeline_tokens: 60000
  pipeline_seconds: 300
  max_probe_runs: 4
  stage_retries: 1
objective: {kind: quality_gain, min_pass_gain: 1}
`

func TestParseConfigRejectsAmbiguousContracts(t *testing.T) {
	for _, test := range []struct{ name, old, replacement string }{
		{"unknown", "profile:", "profil:"},
		{"duplicate", "schema_version: 1", "schema_version: 1\nschema_version: 1"},
		{"null", "mode: manual", "mode: null"},
		{"wrong_scalar", "worker: extractor", "worker: 123"},
		{"wrong_bool", "stage_retries: 1", "stage_retries: true"},
		{"fractional", "stage_retries: 1", "stage_retries: 1.0"},
		{"overflow", "pipeline_tokens: 60000", "pipeline_tokens: 9223372036854775808"},
		{"duration_overflow", "pipeline_seconds: 300", "pipeline_seconds: 9223372036854775807"},
		{"bad_enum", "mode: manual", "mode: MANUAL"},
		{"missing_mode", "mode: manual\n", ""},
		{"reserved", "id: candidate_1", "id: baseline"},
		{"wrong_backend", "backend: fixture", "backend: codex"},
		{"union", "min_pass_gain: 1", "min_pass_gain: 1, min_token_reduction_percent: 5"},
		{"surface_union", "agent: extractor}", "agent: extractor, path: skills/x/SKILL.md}"},
		{"missing_model", "models: {executor: {backend: fixture, model: scripted}}", "models: {}"},
		{"alias_cycle", "models: {executor: {backend: fixture, model: scripted}}", "models: &cycle {executor: *cycle}"},
		{"trailing", "min_pass_gain: 1}", "min_pass_gain: 1}\n---\n{}"},
		{"bad_vars", "worker: extractor", "worker: extractor\nvars: {count: 3}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := strings.Replace(validConfig, test.old, test.replacement, 1)
			if raw == validConfig {
				t.Fatal("test did not modify config")
			}
			if _, err := ParseConfig([]byte(raw)); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestConfigDefaultsAndEnvelope(t *testing.T) {
	cfg, err := ParseConfig([]byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Replicates != 1 || cfg.Vars == nil {
		t.Fatalf("defaults lost: %+v", cfg)
	}
	if err := cfg.ValidateEnvelope(1); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateEnvelope(3); err == nil {
		t.Fatal("partial comparison budget accepted")
	}
	cfg.Limits.PerStageTokens = math.MaxInt64 / 2
	cfg.Limits.PipelineTokens = math.MaxInt64
	if err := cfg.ValidateEnvelope(2); err == nil {
		t.Fatal("multiplication overflow admitted")
	}
}

func TestRefinementRequiresExactRoles(t *testing.T) {
	cfg, err := ParseConfig([]byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Mode, cfg.CandidateDirs = "refine", nil
	cfg.Refinement = &Refinement{"procedure.txt", "domain.txt", "compose.txt", "reflect.txt"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("missing refinement models accepted")
	}
	for _, role := range []string{"proposer", "composer", "reflector"} {
		cfg.Models[role] = Model{"fixture", "scripted"}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.CandidateDirs = []Candidate{}
	if err := cfg.Validate(); err == nil {
		t.Fatal("explicit candidate list accepted in refine mode")
	}
}

func TestFixtureUsesRuntimeAssertionValidation(t *testing.T) {
	const valid = `schema_version: 1
id: extraction
result_schema: result.schema.json
cases:
  - id: one
    input: input.txt
    verify:
      type: task_result_assert
      mode: blocking
      task-result-assertions:
        - {pointer: /structured_payload/category, op: equals, value: allowed}
`
	if _, err := ParseFixture([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{
		{"/structured_payload/category", "/summary"},
		{"op: equals", "op: executes"},
		{"mode: blocking", "mode: advisory"},
		{"value: allowed", "value: [allowed]"},
		{"op: equals, value: allowed", "op: min_items, value: -1"},
		{"type: task_result_assert", "type: command_exit"},
		{"input: input.txt", "input: input.txt\n    command: echo success"},
	} {
		if _, err := ParseFixture([]byte(strings.Replace(valid, pair[0], pair[1], 1))); err == nil {
			t.Fatalf("accepted invalid fixture: %v", pair)
		}
	}
}
