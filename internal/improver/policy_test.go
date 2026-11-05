package improver

import (
	"bytes"
	json "encoding/json/v2"
	"flag"
	"os"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/team"
)

func TestPolicyProtocolIsFixedDuringComparison(t *testing.T) {
	project := t.TempDir()
	roots, err := RegisterRoots(map[string]string{"subject_root": project})
	if err != nil {
		t.Fatal(err)
	}
	var previous string
	for _, protocol := range []string{"chat-completions", "responses"} {
		policy := testPolicy(t, project, "model")
		policy.Version = 7
		binding := config.APIProtocolBinding{Requested: protocol, Effective: protocol, Source: "explicit", Version: 1}
		policy.Backends[0].APIProtocol = binding
		policy.ModelRoutes[0].APIProtocol = binding
		policy.ConfigurationHash = ""
		raw, err := json.Marshal(policy, json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true))
		if err != nil {
			t.Fatal(err)
		}
		policy.ConfigurationHash = paired.BytesDigest(raw)
		projection, err := ProjectPolicy(policy, roots)
		if err != nil {
			t.Fatal(err)
		}
		if previous != "" && projection.ComparisonDigest == previous {
			t.Fatal("comparison erased the protocol change")
		}
		previous = projection.ComparisonDigest
	}
}

var updatePairedPolicySchema = flag.Bool("update-paired-policy-schema", false, "Update the reviewed paired policy type schema")

func TestPolicyClassificationCoversRuntimeSchema(t *testing.T) {
	for typ, classes := range map[reflect.Type]map[string]policyFieldClass{
		reflect.TypeFor[team.ExecutionPolicySnapshot](): policyFields,
		reflect.TypeFor[agent.AgentDef]():               agentFields,
	} {
		if err := checkClassifiedFields(typ, classes); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := classifiedSchemaText()
	if err != nil {
		t.Fatal(err)
	}
	const file = "testdata/policy_schema.json"
	if *updatePairedPolicySchema {
		if err := os.WriteFile(file, append(raw, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(expected), raw) {
		t.Fatal("runtime schema changed: review fixed/variant/root classifications before updating policy_schema.json")
	}
}

func testPolicy(t *testing.T, project, model string) *team.ExecutionPolicySnapshot {
	t.Helper()
	policy := &team.ExecutionPolicySnapshot{
		Version: 5, TeamMaxConcurrent: 1, DefaultLLMBackend: "hufu-local",
		Backends:    []team.ExecutionBackendPolicySnapshot{{Backend: "hufu-local", Kind: "llm", MaxConcurrent: 1, IdentityHash: "fixed-transport"}},
		ModelRoutes: []team.ExecutionModelRouteSnapshot{{Model: model, Backend: "hufu-local"}},
		ExecutionWorlds: []team.ExecutionWorldPolicySnapshot{{
			Backend: "hufu-local", ExecutionWorld: "local", ProjectRootHash: team.ExecutionPolicyProjectRootHash(project),
		}},
	}
	// Match the durable v5 serialization, whose nil collections are null.
	raw, err := json.Marshal(policy, json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true))
	if err != nil {
		t.Fatal(err)
	}
	policy.ConfigurationHash = paired.BytesDigest(raw)
	if err := team.ValidateExecutionPolicySnapshot(policy); err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestPolicyMapsOnlyRegisteredRoots(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	rootsA, err := RegisterRoots(map[string]string{"subject_root": a})
	if err != nil {
		t.Fatal(err)
	}
	rootsB, err := RegisterRoots(map[string]string{"subject_root": b})
	if err != nil {
		t.Fatal(err)
	}
	policyA, policyB := testPolicy(t, a, "model_a"), testPolicy(t, b, "model_a")
	first, err := ProjectPolicy(policyA, rootsA)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ProjectPolicy(policyB, rootsB)
	if err != nil {
		t.Fatal(err)
	}
	if first.ComparisonDigest != second.ComparisonDigest || first.OriginalConfigurationHash == second.OriginalConfigurationHash {
		t.Fatal("logical root mapping lost comparison or original identity")
	}
	changed, err := ProjectPolicy(testPolicy(t, b, "model_b"), rootsB)
	if err != nil || changed.ComparisonDigest == first.ComparisonDigest {
		t.Fatal("mapping hid a model change")
	}
	if _, err := ProjectPolicy(policyA, rootsB); err == nil {
		t.Fatal("unregistered root hash was erased")
	}
	policyA.ConfigurationHash = policyB.ConfigurationHash
	if _, err := ProjectPolicy(policyA, rootsA); err == nil {
		t.Fatal("tampered original policy accepted")
	}
}
