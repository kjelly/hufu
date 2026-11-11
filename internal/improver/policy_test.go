package improver

import (
	"bytes"
	json "encoding/json/v2"
	"flag"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/media"
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

func TestPolicyCollectorResponseChannelIsFixedDuringComparison(t *testing.T) {
	project := t.TempDir()
	roots, err := RegisterRoots(map[string]string{"subject_root": project})
	if err != nil {
		t.Fatal(err)
	}
	var previous string
	for _, channel := range []agent.CollectorResponseChannel{agent.CollectorResponseFunction, agent.CollectorResponseTextJSON, agent.CollectorResponseAuto} {
		policy := testPolicy(t, project, "model")
		policy.ResultCollection = &team.ExecutionResultCollectionPolicySnapshot{Version: 1, Default: agent.ResultTransportRuntime,
			CollectorResponseChannel: channel, CollectorSchemaRevision: 1, TurnLimit: 2, GenerationSHA256: strings.Repeat("a", 64)}
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
			t.Fatal("comparison erased a collector response channel change")
		}
		previous = projection.ComparisonDigest
	}
}

func TestPolicyLaunchSettingsAreFixedDuringComparison(t *testing.T) {
	project := t.TempDir()
	roots, err := RegisterRoots(map[string]string{"subject_root": project})
	if err != nil {
		t.Fatal(err)
	}
	var previous string
	for _, timeout := range []int64{30, 60} {
		policy := testPolicy(t, project, "model")
		policy.Launch = &team.LaunchSettings{Version: 1, Timeout: timeout, Agents: map[string]team.LaunchAgentSettings{}}
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
			t.Fatal("comparison erased a launch setting change")
		}
		previous = projection.ComparisonDigest
	}
}

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

func TestPolicyRetainsMediaAuthorization(t *testing.T) {
	root := t.TempDir()
	roots, err := RegisterRoots(map[string]string{"subject_root": root})
	if err != nil {
		t.Fatal(err)
	}
	project := func(bound int64) PolicyProjection {
		t.Helper()
		policy := testPolicy(t, root, "model_a")
		limits, err := (media.Policy{Enabled: true, AllowedTargets: []media.Target{{Backend: "hufu-local", Model: "model_a", IdentityHash: "fixed-transport", ImageTokenUpperBound: bound}}}).Resolve()
		if err != nil {
			t.Fatal(err)
		}
		policy.Media = &team.MediaPolicySnapshot{Version: media.Version, Policy: limits}
		policy.Media.Hash, err = media.Digest(policy.Media)
		if err != nil {
			t.Fatal(err)
		}
		policy.ConfigurationHash = ""
		raw, err := json.Marshal(policy, json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true))
		if err != nil {
			t.Fatal(err)
		}
		policy.ConfigurationHash = paired.BytesDigest(raw)
		result, err := ProjectPolicy(policy, roots)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first, same, changed := project(8192), project(8192), project(16384)
	if first.ComparisonDigest != same.ComparisonDigest || first.ComparisonDigest == changed.ComparisonDigest {
		t.Fatal("policy projection erased or destabilized fixed media authorization")
	}
	var projected struct {
		Media *team.MediaPolicySnapshot `json:"media"`
	}
	if err := json.Unmarshal(first.Policy, &projected); err != nil {
		t.Fatal(err)
	}
	if projected.Media == nil || projected.Media.Policy.AllowedTargets[0].ImageTokenUpperBound != 8192 || projected.Media.Policy.AllowedTargets[0].IdentityHash != "fixed-transport" {
		t.Fatal("projection lost media limits or pinned target identity")
	}
}
