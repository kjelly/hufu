package improver

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/team"
)

type policyFieldClass string

const (
	fixedPolicy       policyFieldClass = "fixed"
	logicalWorld      policyFieldClass = "logical_world"
	derivedPolicyHash policyFieldClass = "verified_derived_hash"
	variantPrompt     policyFieldClass = "selected_prompt"
	derivedPrompt     policyFieldClass = "derived_prompt_description"
)

// Every top-level field is classified explicitly. Nested fields inherit fixed
// semantics; the recursive schema fixture additionally catches nested changes.
var policyFields = map[string]policyFieldClass{
	"Version": fixedPolicy, "TeamMaxConcurrent": fixedPolicy, "DefaultLLMBackend": fixedPolicy,
	"Backends": fixedPolicy, "ModelRoutes": fixedPolicy, "ExecutionWorlds": logicalWorld,
	"RunInputSchemaHash": fixedPolicy, "RunInputPolicyHash": fixedPolicy, "ResultContracts": fixedPolicy,
	"StaticWorkflowHash": fixedPolicy, "ExecutionRoutes": fixedPolicy, "ActionCatalogHash": fixedPolicy,
	"DecisionPrimitiveHash": fixedPolicy, "ControlDecisionHash": fixedPolicy, "SemanticRetrievalHash": fixedPolicy,
	"Belief": fixedPolicy, "DiagnosticLoop": fixedPolicy, "MCPActionProviders": fixedPolicy,
	"MCPWorkerPolicies": fixedPolicy, "ContextArtifacts": fixedPolicy, "ContextQuery": fixedPolicy,
	"Cost": fixedPolicy, "ConfigurationHash": derivedPolicyHash,
}

var agentFields = map[string]policyFieldClass{
	"Name": fixedPolicy, "FileAlias": fixedPolicy, "Description": fixedPolicy, "Tools": fixedPolicy,
	"Role": fixedPolicy, "System": variantPrompt, "Capabilities": derivedPrompt, "Skills": fixedPolicy,
	"Guard": fixedPolicy, "Timeout": fixedPolicy, "MaxRetries": fixedPolicy, "MaxSteps": fixedPolicy,
	"AllowedPaths": fixedPolicy, "RestrictedPath": fixedPolicy, "NoNet": fixedPolicy, "ForceMCP": fixedPolicy,
	"Shell": fixedPolicy, "MCPTools": fixedPolicy, "Requirements": fixedPolicy, "ProviderURL": fixedPolicy,
	"SideEffect": fixedPolicy, "Recovery": fixedPolicy, "ReconcileTool": fixedPolicy, "ToolRecovery": fixedPolicy,
	"MemoryID": fixedPolicy, "Memory": fixedPolicy, "Generation": fixedPolicy, "ExtraModels": fixedPolicy,
	"SubagentProvider": fixedPolicy, "ResultContract": fixedPolicy, "WorkerWorkspace": fixedPolicy, "ExecutionRoute": fixedPolicy,
}

func checkClassifiedFields(typ reflect.Type, classes map[string]policyFieldClass) error {
	if len(classes) != typ.NumField() {
		return fmt.Errorf("unsupported policy schema: %s field count changed", typ.Name())
	}
	for field := range typ.Fields() {
		if classes[field.Name] == "" {
			return fmt.Errorf("unsupported policy schema: unclassified %s.%s", typ.Name(), field.Name)
		}
	}
	return nil
}

type RegisteredRoots struct {
	byHash map[string]string
}

// RegisterRoots accepts only explicitly owned, absolute roots; no regular
// expression or general path/hash erasure is used during projection.
func RegisterRoots(roots map[string]string) (RegisteredRoots, error) {
	result := RegisteredRoots{byHash: map[string]string{}}
	for logical, physical := range roots {
		if !slices.Contains([]string{"subject_root", "team_root", "control_root"}, logical) ||
			!filepath.IsAbs(physical) || filepath.Clean(physical) != physical {
			return RegisteredRoots{}, errors.New("invalid logical root registration")
		}
		hash := paired.BytesDigest([]byte(physical))
		if _, exists := result.byHash[hash]; exists {
			return RegisteredRoots{}, errors.New("distinct logical roots cannot share a physical root")
		}
		result.byHash[hash] = logical
	}
	return result, nil
}

type PolicyProjection struct {
	Version                   int            `json:"version"`
	OriginalConfigurationHash string         `json:"original_configuration_hash"`
	ComparisonDigest          string         `json:"comparison_digest"`
	Policy                    jsontext.Value `json:"policy"`
}

// ProjectPolicy retains the validated original hash for resume and computes a
// separate comparison digest. Only registered project-root hashes are mapped.
func ProjectPolicy(snapshot *team.ExecutionPolicySnapshot, roots RegisteredRoots) (PolicyProjection, error) {
	if err := checkClassifiedFields(reflect.TypeFor[team.ExecutionPolicySnapshot](), policyFields); err != nil {
		return PolicyProjection{}, err
	}
	if err := team.ValidateExecutionPolicySnapshot(snapshot); err != nil {
		return PolicyProjection{}, err
	}
	if snapshot.Version != 5 {
		return PolicyProjection{}, errors.New("strict comparison requires execution policy version 5")
	}
	copySnapshot := *snapshot
	copySnapshot.ConfigurationHash = ""
	copySnapshot.ExecutionWorlds = slices.Clone(snapshot.ExecutionWorlds)
	for i := range copySnapshot.ExecutionWorlds {
		world := &copySnapshot.ExecutionWorlds[i]
		logical, exists := roots.byHash[world.ProjectRootHash]
		if !exists {
			return PolicyProjection{}, errors.New("unregistered execution-world project root")
		}
		world.ProjectRootHash = paired.BytesDigest([]byte("hufu.paired.logical." + logical))
	}
	raw, err := paired.ProjectionJSON(copySnapshot)
	if err != nil {
		return PolicyProjection{}, err
	}
	digest, err := paired.Digest("execution_policy", jsontext.Value(raw))
	if err != nil {
		return PolicyProjection{}, err
	}
	return PolicyProjection{1, snapshot.ConfigurationHash, digest, raw}, nil
}

// classifiedSchema records nested type/field structure for reviewable coverage.
func classifiedSchema() map[string]string {
	result := map[string]string{}
	collectSchema(reflect.TypeFor[team.ExecutionPolicySnapshot](), "policy", result, map[reflect.Type]bool{})
	collectSchema(reflect.TypeFor[agent.AgentDef](), "agent", result, map[reflect.Type]bool{})
	collectSchema(reflect.TypeFor[agent.TeamConfig](), "team", result, map[reflect.Type]bool{})
	for name, class := range policyFields {
		result["class.policy."+name] = string(class)
	}
	for name, class := range agentFields {
		result["class.agent."+name] = string(class)
	}
	return result
}

func collectSchema(typ reflect.Type, prefix string, output map[string]string, active map[reflect.Type]bool) {
	output[prefix] = typ.String()
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array || typ.Kind() == reflect.Map {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct || active[typ] {
		return
	}
	active[typ] = true
	defer delete(active, typ)
	for field := range typ.Fields() {
		if field.IsExported() {
			collectSchema(field.Type, prefix+"."+field.Name, output, active)
		}
	}
}

// Stable schema text keeps the coverage artifact independent of map ordering.
func classifiedSchemaText() ([]byte, error) {
	schema := classifiedSchema()
	lines := make([]string, 0, len(schema))
	for _, key := range slices.Sorted(maps.Keys(schema)) {
		lines = append(lines, key+" "+schema[key])
	}
	return json.Marshal(lines)
}
