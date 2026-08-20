package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"gopkg.in/yaml.v3"
)

func wp01TaskResult(status string) *TaskResult {
	return &TaskResult{
		TaskID:  "task-1",
		Agent:   "worker",
		Status:  status,
		Summary: "completed report",
		Details: "grounded details",
		Facts: map[string]any{
			"items": []any{"alpha", "beta"},
			"a/b":   "escaped-key",
		},
		Confidence: 0.9,
		Source:     "submitted",
	}
}

func TestTaskResultAssertOperators(t *testing.T) {
	result := wp01TaskResult(TaskResultStatusSuccess)
	tests := []struct {
		name      string
		assertion TaskResultAssertion
		wantError bool
	}{
		{name: "exists", assertion: TaskResultAssertion{Pointer: "/summary", Op: "exists"}},
		{name: "non_empty", assertion: TaskResultAssertion{Pointer: "/details", Op: "non_empty"}},
		{name: "equals", assertion: TaskResultAssertion{Pointer: "/status", Op: "equals", Value: "success"}},
		{name: "equals failure", assertion: TaskResultAssertion{Pointer: "/status", Op: "equals", Value: "failed"}, wantError: true},
		{name: "min_items", assertion: TaskResultAssertion{Pointer: "/facts/items", Op: "min_items", Value: 2}},
		{name: "contains_scalar", assertion: TaskResultAssertion{Pointer: "/facts/items", Op: "contains_scalar", Value: "beta"}},
		{name: "escaped pointer", assertion: TaskResultAssertion{Pointer: "/facts/a~1b", Op: "equals", Value: "escaped-key"}},
		{name: "missing pointer", assertion: TaskResultAssertion{Pointer: "/artifacts/missing", Op: "exists", Value: nil}, wantError: true},
		{name: "wrong type", assertion: TaskResultAssertion{Pointer: "/confidence", Op: "min_items", Value: 1}, wantError: true},
		{name: "non empty wrong type", assertion: TaskResultAssertion{Pointer: "/confidence", Op: "non_empty"}, wantError: true},
		{name: "contains wrong type", assertion: TaskResultAssertion{Pointer: "/summary", Op: "contains_scalar", Value: "report"}, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExecuteVerificationSpecWithTaskResult(context.Background(), "sh", t.TempDir(), VerificationSpec{
				Type:                 VerifyTaskResultAssert,
				TaskResultAssertions: []TaskResultAssertion{tt.assertion},
			}, result)
			if (err != nil) != tt.wantError {
				t.Fatalf("error = %v, wantError=%v result=%#v", err, tt.wantError, got)
			}
			if got == nil || (tt.wantError && got.ExitCode == 0) || (!tt.wantError && got.ExitCode != 0) {
				t.Fatalf("verification result = %#v, want failure=%v", got, tt.wantError)
			}
		})
	}
}

func TestTaskResultAssertValidationBounds(t *testing.T) {
	tests := []struct {
		name string
		spec VerificationSpec
	}{
		{name: "invalid pointer", spec: VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{{Pointer: "summary", Op: "exists"}}}},
		{name: "unsupported operator", spec: VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{{Pointer: "/summary", Op: "regex", Value: "x"}}}},
		{name: "oversized assertion", spec: VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{{Pointer: "/summary", Op: "equals", Value: strings.Repeat("x", maxTaskResultValueBytes+1)}}}},
		{name: "too many assertions", spec: VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: make([]TaskResultAssertion, maxTaskResultAssertions+1)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ExecuteVerificationSpecWithTaskResult(context.Background(), "sh", t.TempDir(), tt.spec, wp01TaskResult(TaskResultStatusSuccess))
			if err == nil || result == nil || result.ExitCode != -1 || !strings.Contains(err.Error(), "malformed verification spec") {
				t.Fatalf("malformed spec result=%#v err=%v", result, err)
			}
		})
	}
}

func TestTaskResultAssertPartialAndBlockedAreResultSemantics(t *testing.T) {
	for _, status := range []string{TaskResultStatusPartial, TaskResultStatusBlocked} {
		t.Run(status, func(t *testing.T) {
			result, err := ExecuteVerificationSpecWithTaskResult(context.Background(), "sh", t.TempDir(), VerificationSpec{
				Type: VerifyTaskResultAssert,
				TaskResultAssertions: []TaskResultAssertion{{
					Pointer: "/status", Op: "equals", Value: status,
				}},
			}, wp01TaskResult(status))
			if err != nil || result == nil || result.ExitCode != 0 {
				t.Fatalf("status %q was treated as schema failure: result=%#v err=%v", status, result, err)
			}
		})
	}
}

func TestTaskResultAssertFailsClosedWithoutTaskResult(t *testing.T) {
	result, err := ExecuteVerificationSpec(context.Background(), "sh", t.TempDir(), VerificationSpec{
		Type: VerifyTaskResultAssert,
		TaskResultAssertions: []TaskResultAssertion{{
			Pointer: "/status", Op: "exists",
		}},
	})
	if err == nil || result == nil || result.ExitCode == 0 || !strings.Contains(err.Error(), "requires a canonical task result") {
		t.Fatalf("missing task result did not fail closed: result=%#v err=%v", result, err)
	}
}

func TestTaskResultAssertFingerprintIsOrderIndependent(t *testing.T) {
	first := VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{
		{Pointer: "/summary", Op: "non_empty"},
		{Pointer: "/status", Op: "equals", Value: "success"},
	}}
	second := VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{
		{Pointer: "/status", Op: "equals", Value: "success"},
		{Pointer: "/summary", Op: "non_empty"},
	}}
	dir := t.TempDir()
	firstResult, firstErr := ExecuteVerificationSpecWithTaskResult(context.Background(), "sh", dir, first, wp01TaskResult(TaskResultStatusSuccess))
	secondResult, secondErr := ExecuteVerificationSpecWithTaskResult(context.Background(), "sh", dir, second, wp01TaskResult(TaskResultStatusSuccess))
	if firstErr != nil || secondErr != nil || firstResult.Fingerprint != secondResult.Fingerprint {
		t.Fatalf("assertion order changed fingerprint: first=%#v/%v second=%#v/%v", firstResult, firstErr, secondResult, secondErr)
	}
	if verificationSpecCacheKey(&first) != verificationSpecCacheKey(&second) {
		t.Fatal("assertion order changed task cache contract identity")
	}
}

func TestTaskResultAssertParserAndJSONContract(t *testing.T) {
	const source = `type: task_result_assert
task-result-assertions:
  - pointer: /status
    op: equals
    value: success
  - pointer: /facts/items
    op: min_items
    value: 2
`
	var fromYAML agent.VerificationSpec
	if err := yaml.Unmarshal([]byte(source), &fromYAML); err != nil {
		t.Fatal(err)
	}
	if fromYAML.Type != agent.VerifyTaskResultAssert || len(fromYAML.TaskResultAssertions) != 2 || fromYAML.TaskResultAssertions[1].Pointer != "/facts/items" {
		t.Fatalf("YAML contract = %#v", fromYAML)
	}
	encoded, err := json.Marshal(fromYAML)
	if err != nil {
		t.Fatal(err)
	}
	var fromJSON agent.VerificationSpec
	if err := json.Unmarshal(encoded, &fromJSON); err != nil {
		t.Fatal(err)
	}
	if fromJSON.Type != agent.VerifyTaskResultAssert || len(fromJSON.TaskResultAssertions) != 2 || fromJSON.TaskResultAssertions[0].Op != "equals" {
		t.Fatalf("JSON contract = %#v", fromJSON)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte("name: parser\nacceptance:\n  verifications:\n    - "+strings.ReplaceAll(strings.TrimSpace(source), "\n", "\n      ")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML: %v", err)
	}
	if cfg.AcceptanceSpec == nil || len(cfg.AcceptanceSpec.Verifications) != 1 || cfg.AcceptanceSpec.Verifications[0].Type != agent.VerifyTaskResultAssert {
		t.Fatalf("parsed team contract = %#v", cfg.AcceptanceSpec)
	}
}

func TestTaskResultAssertCoordinatorPassesCanonicalResult(t *testing.T) {
	c := &Coordinator{projectDir: t.TempDir()}
	result, err := c.verifyTaskDeliverableWithSpecAndResult(context.Background(), nil, TaskDef{
		VerifySpec: &VerificationSpec{
			Type: VerifyTaskResultAssert,
			TaskResultAssertions: []TaskResultAssertion{{
				Pointer: "/status", Op: "equals", Value: TaskResultStatusSuccess,
			}},
		},
	}, nil, wp01TaskResult(TaskResultStatusSuccess))
	if err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("coordinator did not pass canonical task result: result=%#v err=%v", result, err)
	}
}

func TestTaskResultAssertPolicyLint(t *testing.T) {
	valid := LintVerifier(VerificationSpec{
		Type: VerifyTaskResultAssert,
		TaskResultAssertions: []TaskResultAssertion{{
			Pointer: "/status", Op: "equals", Value: TaskResultStatusSuccess,
		}},
	}, "")
	if len(valid) != 0 {
		t.Fatalf("valid task_result_assert produced lint findings: %#v", valid)
	}
	invalid := LintVerifier(VerificationSpec{
		Type: VerifyTaskResultAssert,
		TaskResultAssertions: []TaskResultAssertion{{
			Pointer: "/status", Op: "unsupported",
		}},
	}, "")
	if len(invalid) != 1 || invalid[0].Code != FindingVerifierInvalid {
		t.Fatalf("invalid task_result_assert lint = %#v", invalid)
	}
}

func TestTaskResultAssertAcceptanceContextFailsClosed(t *testing.T) {
	c := &Coordinator{
		projectDir: t.TempDir(),
		session:    &TeamSession{Config: agent.TeamConfig{Shell: "sh"}},
		acceptanceSpec: &AcceptanceSpec{Verifications: []VerificationSpec{{
			Type:                 VerifyTaskResultAssert,
			TaskResultAssertions: []TaskResultAssertion{{Pointer: "/status", Op: "exists"}},
		}}},
	}
	result, err := c.runAcceptance(context.Background())
	if err == nil || result == nil || result.Passed || result.State != AcceptanceFailed {
		t.Fatalf("acceptance without task result did not fail closed: result=%#v err=%v", result, err)
	}
}
