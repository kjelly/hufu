package team

import (
	"context"
	stdjson "encoding/json"
	"encoding/json/v2"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/utils"
)

// Reproduce the retired persistence policy before the events are written. The
// switch disables it before repair; no historical event/checkpoint is edited.
type legacyBudgetRedactor struct{ enabled atomic.Bool }

func (r *legacyBudgetRedactor) RedactJSON(data []byte) ([]byte, error) {
	if !r.enabled.Load() {
		return data, nil
	}
	value, err := decodeUniqueJSON(data)
	if err != nil {
		return nil, err
	}
	return stdjson.Marshal(maskLegacyBudget(value))
}
func (r *legacyBudgetRedactor) RedactText(value string) string {
	if !r.enabled.Load() || !stdjson.Valid([]byte(value)) {
		return value
	}
	out, err := r.RedactJSON([]byte(value))
	if err != nil {
		return value
	}
	return string(out)
}
func maskLegacyBudget(value any) any {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if key == "max_analysis_input_tokens" {
				if _, ok := item.(stdjson.Number); ok {
					v[key] = "[REDACTED]"
					continue
				}
			}
			v[key] = maskLegacyBudget(item)
		}
	case []any:
		for i, item := range v {
			v[i] = maskLegacyBudget(item)
		}
	case string:
		if strings.Contains(v, "max_analysis_input_tokens") && stdjson.Valid([]byte(v)) {
			inner, err := decodeUniqueJSON([]byte(v))
			if err == nil {
				if out, e := stdjson.Marshal(maskLegacyBudget(inner)); e == nil {
					return string(out)
				}
			}
		}
	}
	return value
}

func legacyNativeActionRecoveryFixture(t *testing.T) (*Coordinator, *versionFixture, *ResumeSource, *int) {
	t.Helper()
	redactor := &legacyBudgetRedactor{}
	redactor.enabled.Store(true)
	utils.RegisterSecretRedactor(redactor)
	defer redactor.enabled.Store(false)
	return nativeActionRecoveryFixture(t, `{"scope":{},"max_analysis_input_tokens":12000}`)
}

func TestContractRecoveryActionReconstructsRedactedPayloadAndResumesChild(t *testing.T) {
	testNativeActionRecoveryChild(t, true)
}

func TestContractRecoveryPayloadProofRejectsAlterations(t *testing.T) {
	c, f, source, _ := legacyNativeActionRecoveryFixture(t)
	ctx := WithActionPayloadReconstruction(WithReadOnlyActionRepair(WithMetadataOnlyContractRecovery(t.Context()), "8"))
	plan, err := c.AssessContractRecovery(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	before := mustReadEvents(t, f.events)
	for _, name := range []string{"stored_hash", "materialized_hash", "snapshot", "paths", "payload", "missing_mode"} {
		t.Run(name, func(t *testing.T) {
			var changed ContractRecoveryPlan
			if err := json.Unmarshal(mustProjectionJSON(t, plan), &changed); err != nil {
				t.Fatal(err)
			}
			proof := changed.Tasks[0].PayloadReconstruction
			if proof == nil {
				t.Fatal("no payload reconstruction proof")
			}
			switch name {
			case "stored_hash":
				proof.StoredSHA256 = runInputHash([]byte("other"))
			case "materialized_hash":
				proof.MaterializedSHA256 = runInputHash([]byte("other"))
			case "snapshot":
				proof.SnapshotSHA256 = runInputHash([]byte("other"))
			case "paths":
				proof.Paths = []string{"/scope"}
			case "payload":
				proof.Payload = strings.ReplaceAll(proof.Payload, "12000", "12001")
			case "missing_mode":
				changed.ActionRepair = nil
			}
			changed.SHA256, err = recoveryPlanHash(changed)
			if err != nil {
				t.Fatal(err)
			}
			event := RunEvent{SchemaVersion: eventStoreSchemaVersion, Type: string(EventContractRecovery), BranchID: "child", Payload: mustProjectionJSON(t, changed)}
			if _, err := validateContractRecoveryLineage(event, before, source.Session.ExecutionPolicySnapshot); err == nil {
				t.Fatal("altered reconstruction accepted")
			}
		})
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.AssessContractRecovery(cancelled, source); err == nil {
		t.Fatal("cancelled reconstruction accepted")
	}
	if !sameCanonicalEvidence(before, mustReadEvents(t, f.events)) {
		t.Fatal("rejected preview modified history")
	}
}

func TestReconstructedPayloadPathsRestrictsPublicNumericMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, old, next string
		valid           bool
	}{
		{"public", `{"max_analysis_input_tokens":"[REDACTED]","scope":2}`, `{"max_analysis_input_tokens":12000,"scope":2}`, true},
		{"nested", `{"items":[{"max_summary_output_tokens":"[REDACTED]"}]}`, `{"items":[{"max_summary_output_tokens":42}]}`, true},
		{"credential", `{"api_token":"[REDACTED]"}`, `{"api_token":12000}`, false},
		{"sensitive_scope", `{"max_auth_input_tokens":"[REDACTED]"}`, `{"max_auth_input_tokens":12000}`, false},
		{"string", `{"max_analysis_input_tokens":"[REDACTED]"}`, `{"max_analysis_input_tokens":"12000"}`, false},
		{"negative", `{"max_analysis_input_tokens":"[REDACTED]"}`, `{"max_analysis_input_tokens":-1}`, false},
		{"scope", `{"max_analysis_input_tokens":"[REDACTED]","scope":2}`, `{"max_analysis_input_tokens":12000,"scope":3}`, false},
		{"duplicate", `{"max_analysis_input_tokens":"[REDACTED]","max_analysis_input_tokens":"[REDACTED]"}`, `{"max_analysis_input_tokens":12000}`, false},
		{"unchanged", `{"max_analysis_input_tokens":12000}`, `{"max_analysis_input_tokens":12000}`, false},
		{"added", `{}`, `{"max_analysis_input_tokens":12000}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths, err := reconstructedPayloadPaths(tc.old, tc.next)
			if (err == nil) != tc.valid {
				t.Fatalf("paths=%v err=%v", paths, err)
			}
		})
	}
}

func TestContractRecoveryPayloadReconstructionHonorsActiveRedaction(t *testing.T) {
	c, f, source, _ := legacyNativeActionRecoveryFixture(t)
	ctx := WithActionPayloadReconstruction(WithReadOnlyActionRepair(WithMetadataOnlyContractRecovery(t.Context()), "8"))
	plan, err := c.AssessContractRecovery(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	before := mustReadEvents(t, f.events)
	redactor := &legacyBudgetRedactor{}
	redactor.enabled.Store(true)
	utils.RegisterSecretRedactor(redactor)
	defer redactor.enabled.Store(false)
	if _, err := c.ApplyContractRecovery(ctx, source, "protected-budget", plan.SHA256); err == nil {
		t.Fatal("active secret protection was bypassed")
	}
	if !sameCanonicalEvidence(before, mustReadEvents(t, f.events)) {
		t.Fatal("redaction refusal published a child")
	}
}

func TestContractRecoveryPayloadRequiresOriginalMaterialization(t *testing.T) {
	c, _, source, _ := legacyNativeActionRecoveryFixture(t)
	ctx := WithActionPayloadReconstruction(WithReadOnlyActionRepair(t.Context(), "8"))
	for _, name := range []string{"missing_hash", "missing_bindings", "changed_bindings", "snapshot", "scope", "static_payload", "unbound"} {
		t.Run(name, func(t *testing.T) {
			item := cloneTodoItem(source.Session.Tasks[0])
			definition := cloneTaskDef(c.session.ContractTasks[0])
			switch name {
			case "missing_hash":
				item.MaterializedActionPayloadHash = ""
			case "missing_bindings":
				item.ActionInputBindings = nil
			case "changed_bindings":
				item.ActionInputBindings[0].Target = "/other"
			case "snapshot":
				item.RunInputSnapshotID = "another-snapshot"
			case "scope":
				item.BoundInputs["scope"] = runInputHash([]byte("other"))
			case "static_payload":
				definition.Action.Payload = `{"scope":{},"max_analysis_input_tokens":12001}`
			case "unbound":
				definition.Action.InputBindings = nil
				item.ActionInputBindings = nil
			}
			if _, _, err := prepareRecoveredActionPayload(ctx, definition, item, source.Session); err == nil {
				t.Fatal("unverified materialization accepted")
			}
		})
	}
}
