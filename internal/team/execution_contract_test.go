package team

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestValidateExecutionContract_Defaults(t *testing.T) {
	task := TaskDef{
		Agent: "dev",
		Goal:  "build feature",
	}

	if err := ValidateExecutionContract(task); err != nil {
		t.Fatalf("ValidateExecutionContract failed for zero-value contract: %v", err)
	}

	c := DefaultExecutionContract(task.Execution)
	if c.Kind != ExecutionKindInline {
		t.Errorf("Default kind = %q, want %q", c.Kind, ExecutionKindInline)
	}
}

func TestValidateExecutionContract_ValidKinds(t *testing.T) {
	kinds := []ExecutionKind{
		"",
		ExecutionKindInline,
		ExecutionKindProcess,
		ExecutionKindInteractive,
		ExecutionKindExternal,
	}

	for _, k := range kinds {
		task := TaskDef{
			Agent: "dev",
			Goal:  "do work",
			Execution: ExecutionContract{
				Kind: k,
			},
		}
		if err := ValidateExecutionContract(task); err != nil {
			t.Errorf("ValidateExecutionContract unexpectedly failed for kind %q: %v", k, err)
		}
	}
}

func TestValidateExecutionContract_InvalidKind(t *testing.T) {
	task := TaskDef{
		Agent: "dev",
		Goal:  "do work",
		Execution: ExecutionContract{
			Kind: ExecutionKind("custom_unknown_kind"),
		},
	}

	err := ValidateExecutionContract(task)
	if err == nil {
		t.Fatal("expected error for invalid execution kind, got nil")
	}
	if !strings.Contains(err.Error(), "invalid execution kind") {
		t.Errorf("expected 'invalid execution kind' error, got %v", err)
	}
}

func TestValidateExecutionContract_RequiresVerification(t *testing.T) {
	tests := []struct {
		name       string
		kind       ExecutionKind
		reqVerify  bool
		verify     string
		verifyMode string
		wantErr    bool
	}{
		{
			name:      "interactive without verify fails",
			kind:      ExecutionKindInteractive,
			reqVerify: true,
			verify:    "",
			wantErr:   true,
		},
		{
			name:       "interactive with verifyMode none fails",
			kind:       ExecutionKindInteractive,
			reqVerify:  true,
			verify:     "",
			verifyMode: "none",
			wantErr:    true,
		},
		{
			name:       "interactive with verifyMode none even with command fails",
			kind:       ExecutionKindInteractive,
			reqVerify:  true,
			verify:     "go test ./...",
			verifyMode: "none",
			wantErr:    true,
		},
		{
			name:      "interactive with verify command passes",
			kind:      ExecutionKindInteractive,
			reqVerify: true,
			verify:    "go test ./...",
			wantErr:   false,
		},
		{
			name:       "interactive with valid verifyMode and verify command passes",
			kind:       ExecutionKindInteractive,
			reqVerify:  true,
			verify:     "go test ./...",
			verifyMode: "strict",
			wantErr:    false,
		},
		{
			name:      "interactive without reqVerify passes even if verify is empty",
			kind:      ExecutionKindInteractive,
			reqVerify: false,
			verify:    "",
			wantErr:   false,
		},
		{
			name:      "external without verify fails",
			kind:      ExecutionKindExternal,
			reqVerify: true,
			verify:    "",
			wantErr:   true,
		},
		{
			name:      "external with verify passes",
			kind:      ExecutionKindExternal,
			reqVerify: true,
			verify:    "curl -f http://localhost:8080/health",
			wantErr:   false,
		},
		{
			name:      "inline with reqVerify passes without verify",
			kind:      ExecutionKindInline,
			reqVerify: true,
			verify:    "",
			wantErr:   false,
		},
		{
			name:      "interactive with typed file verifier passes",
			kind:      ExecutionKindInteractive,
			reqVerify: true,
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := TaskDef{
				Agent:      "dev",
				Goal:       "perform action",
				Verify:     tt.verify,
				VerifyMode: tt.verifyMode,
				Execution: ExecutionContract{
					Kind:                 tt.kind,
					RequiresVerification: tt.reqVerify,
				},
			}
			if tt.name == "interactive with typed file verifier passes" {
				task.VerifySpec = &VerificationSpec{Type: VerifyFileExists, Path: "report.md"}
			}

			err := ValidateExecutionContract(task)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateExecutionContract() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateExecutionContract_RequiresVerificationRejectsNonAssertingOrMalformedTypedVerifier(t *testing.T) {
	base := TaskDef{
		Agent: "dev",
		Goal:  "perform external action",
		Execution: ExecutionContract{
			Kind:                 ExecutionKindExternal,
			RequiresVerification: true,
		},
	}

	tests := []struct {
		name string
		spec VerificationSpec
	}{
		{name: "observation", spec: VerificationSpec{Type: VerifyFileExists, Path: "report.md", Mode: "observation"}},
		{name: "missing path", spec: VerificationSpec{Type: VerifyFileExists}},
		{name: "json assertion without assertions", spec: VerificationSpec{Type: VerifyJSONAssert, Path: "report.json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := base
			task.VerifySpec = &tt.spec
			if err := ValidateExecutionContract(task); err == nil {
				t.Fatal("expected invalid typed verifier to be rejected")
			}
		})
	}
}

func TestValidateExecutionContract_IgnoresGoalAndDesc(t *testing.T) {
	// Must not read Goal or Constraints text to deduce execution risks
	goals := []string{
		"interactive shell session needed",
		"external infra mutation restart server",
		"requires verification strictly",
		"trec drive run without verify",
		"",
	}

	for _, g := range goals {
		task := TaskDef{
			Agent:       "worker",
			Goal:        g,
			Constraints: "do interactive external verification trec drive",
			Execution: ExecutionContract{
				Kind: ExecutionKindInline,
			},
		}

		if err := ValidateExecutionContract(task); err != nil {
			t.Errorf("ValidateExecutionContract should ignore Goal/Desc text, but failed for goal %q: %v", g, err)
		}
	}
}

func TestExecutionContract_JSONAndYAML(t *testing.T) {
	jsonPayload := `{
		"agent": "worker",
		"goal": "deploy app",
		"execution": {
			"kind": "external",
			"requires_result": true,
			"requires_verification": true,
			"allows_replay": false
		}
	}`

	var taskJSON TaskDef
	if err := json.Unmarshal([]byte(jsonPayload), &taskJSON); err != nil {
		t.Fatalf("json unmarshal failed: %v", err)
	}

	if taskJSON.Execution.Kind != ExecutionKindExternal {
		t.Errorf("JSON kind = %q, want %q", taskJSON.Execution.Kind, ExecutionKindExternal)
	}
	if !taskJSON.Execution.RequiresResult {
		t.Error("JSON requires_result should be true")
	}
	if !taskJSON.Execution.RequiresVerification {
		t.Error("JSON requires_verification should be true")
	}
	if taskJSON.Execution.AllowsReplay == nil || *taskJSON.Execution.AllowsReplay {
		t.Error("JSON allows_replay should be false")
	}

	yamlPayload := `
agent: worker
goal: deploy app
execution:
  kind: interactive
  requires-result: true
  requires-verification: true
  allows-replay: true
`
	var taskYAML TaskDef
	if err := yaml.Unmarshal([]byte(yamlPayload), &taskYAML); err != nil {
		t.Fatalf("yaml unmarshal failed: %v", err)
	}

	if taskYAML.Execution.Kind != ExecutionKindInteractive {
		t.Errorf("YAML kind = %q, want %q", taskYAML.Execution.Kind, ExecutionKindInteractive)
	}
	if !taskYAML.Execution.RequiresResult {
		t.Error("YAML requires-result should be true")
	}
	if !taskYAML.Execution.RequiresVerification {
		t.Error("YAML requires-verification should be true")
	}
	if taskYAML.Execution.AllowsReplay == nil || !*taskYAML.Execution.AllowsReplay {
		t.Error("YAML allows-replay should be true")
	}
}

func TestExecutionContract_YAMLLegacyStrictResult(t *testing.T) {
	// Top-level YAML strict-result
	topLevelYAML := `
agent: dev
goal: work
strict-result: true
`
	var taskTop TaskDef
	if err := yaml.Unmarshal([]byte(topLevelYAML), &taskTop); err != nil {
		t.Fatalf("unmarshal top level YAML strict-result failed: %v", err)
	}
	if !taskTop.Execution.RequiresResult {
		t.Error("YAML top-level strict-result should map to Execution.RequiresResult = true")
	}

	// Execution-nested YAML strict-result
	nestedYAML := `
agent: dev
goal: work
execution:
  strict-result: true
`
	var taskNested TaskDef
	if err := yaml.Unmarshal([]byte(nestedYAML), &taskNested); err != nil {
		t.Fatalf("unmarshal nested YAML strict-result failed: %v", err)
	}
	if !taskNested.Execution.RequiresResult {
		t.Error("YAML nested strict-result should map to Execution.RequiresResult = true")
	}
}

func TestExecutionContract_SpecFieldsOnly(t *testing.T) {
	// 1. Verify ExecutionContract struct exports ONLY the 4 specification fields
	contractType := reflect.TypeOf(ExecutionContract{})
	expectedFields := map[string]bool{
		"Kind":                 true,
		"RequiresResult":       true,
		"RequiresVerification": true,
		"AllowsReplay":         true,
	}

	if contractType.NumField() != len(expectedFields) {
		t.Fatalf("ExecutionContract has %d fields, want exactly %d spec fields", contractType.NumField(), len(expectedFields))
	}

	for i := 0; i < contractType.NumField(); i++ {
		fieldName := contractType.Field(i).Name
		if !expectedFields[fieldName] {
			t.Errorf("ExecutionContract has unexpected exported field: %s", fieldName)
		}
	}

	// 2. Verify buildAgentTaskProperties execution sub-properties map contains ONLY spec fields
	props := buildAgentTaskProperties([]string{"worker"}, true, "/tmp/shared")
	execProp := props["execution"].(map[string]any)
	execSubProps := execProp["properties"].(map[string]any)

	expectedSchemaKeys := map[string]bool{
		"kind":                  true,
		"requires_result":       true,
		"requires_verification": true,
		"allows_replay":         true,
	}

	if len(execSubProps) != len(expectedSchemaKeys) {
		t.Fatalf("execution schema properties has %d keys, want exactly %d", len(execSubProps), len(expectedSchemaKeys))
	}

	for k := range execSubProps {
		if !expectedSchemaKeys[k] {
			t.Errorf("execution schema contains non-spec key: %s", k)
		}
	}

	// 3. Verify buildAgentTaskProperties top-level map does NOT expose non-spec strict_result key
	if _, hasStrictResult := props["strict_result"]; hasStrictResult {
		t.Error("buildAgentTaskProperties task schema unexpectedly exposes top-level 'strict_result' key")
	}
	if _, hasStrictResultDash := props["strict-result"]; hasStrictResultDash {
		t.Error("buildAgentTaskProperties task schema unexpectedly exposes top-level 'strict-result' key")
	}
}

func TestCoordinatorExecuteTasks_RejectsInvalidExecutionContract(t *testing.T) {
	c := &Coordinator{}
	tasks := []TaskDef{
		{
			Agent: "worker",
			Goal:  "do work",
			Execution: ExecutionContract{
				Kind:                 ExecutionKindInteractive,
				RequiresVerification: true,
				// Missing Verify command
			},
		},
	}

	_, err := c.ExecuteTasks(context.Background(), tasks)
	if err == nil {
		t.Fatal("ExecuteTasks should reject task with invalid execution contract")
	}
	if !strings.Contains(err.Error(), "requires an objective verifier contract") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestValidateExecutionContract_RejectsNonAssertingVerifier(t *testing.T) {
	kinds := []ExecutionKind{
		ExecutionKindInline,
		ExecutionKindProcess,
		ExecutionKindInteractive,
		ExecutionKindExternal,
	}

	for _, k := range kinds {
		t.Run(string(k), func(t *testing.T) {
			task := TaskDef{
				Agent:  "worker",
				Goal:   "do work",
				Verify: "test -f artifact || echo FAIL",
				Execution: ExecutionContract{
					Kind: k,
				},
			}
			err := ValidateExecutionContract(task)
			if err == nil {
				t.Fatalf("expected ValidateExecutionContract to reject || echo FAIL for kind %q", k)
			}
			if !strings.Contains(err.Error(), "verifier contract error") {
				t.Errorf("expected 'verifier contract error', got: %v", err)
			}
		})
	}
}

func TestValidateExecutionContract_ObservationExempt(t *testing.T) {
	task := TaskDef{
		Agent:      "worker",
		Goal:       "observe work",
		Verify:     "test -f artifact || echo FAIL",
		VerifyMode: "observation",
		Execution: ExecutionContract{
			Kind: ExecutionKindInline,
		},
	}
	if err := ValidateExecutionContract(task); err != nil {
		t.Fatalf("observation mode verifier should be exempt from anti-pattern rejection, got: %v", err)
	}
}
