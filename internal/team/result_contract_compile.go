package team

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/utils"
)

const (
	// resultContractSchemaMaxBytes bounds one result contract schema file.
	resultContractSchemaMaxBytes = 256 << 10
	// resultContractExternalSchemaMaxBytes bounds a schema bound to an
	// external agent backend, whose prompt must carry the canonical schema.
	resultContractExternalSchemaMaxBytes = 32 << 10
	resultContractDraft2020URI           = "https://json-schema.org/draft/2020-12/schema"
	resultContractResourceURLPrefix      = "hufu-result-contract:///"
)

// Result contract reason codes. They prefix the load error so operators and
// tests can tell a malformed contract from an unsupported combination.
const (
	resultContractInvalidCode     = "result_contract_invalid"
	resultContractUnsupportedCode = "result_contract_unsupported"
)

// denyResultContractLoader refuses every external schema resource. A result
// contract compiles only from its own document: no network access and no
// sibling-file loading.
type denyResultContractLoader struct{}

func (denyResultContractLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("result contract schemas may not load external resource %q", url)
}

// compileResultContractSchema compiles one team-directory-relative JSON
// Schema file into a result contract.
func compileResultContractSchema(teamDir, schemaPath string) (*CompiledResultContract, error) {
	id, resolved, err := resolveResultContractSchemaPath(teamDir, schemaPath)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("read schema %q: %w", id, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("schema %q is not a regular file", id)
	}
	if info.Size() > resultContractSchemaMaxBytes {
		return nil, fmt.Errorf("schema %q is %d bytes, above the %d byte limit", id, info.Size(), resultContractSchemaMaxBytes)
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("read schema %q: %w", id, err)
	}
	doc, err := decodeResultContractJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("decode schema %q: %w", id, err)
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("schema %q must be a JSON object", id)
	}
	if err := validateResultContractSchemaNode(root, ""); err != nil {
		return nil, fmt.Errorf("schema %q: %w", id, err)
	}
	canonical, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("canonicalize schema %q: %w", id, err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(denyResultContractLoader{})
	url := resultContractResourceURLPrefix + id
	if err := compiler.AddResource(url, doc); err != nil {
		return nil, fmt.Errorf("load schema %q: %w", id, err)
	}
	schema, err := compiler.Compile(url)
	if err != nil {
		return nil, fmt.Errorf("compile schema %q: %w", id, err)
	}
	sum := sha256.Sum256(canonical)
	return &CompiledResultContract{
		ID: id, SchemaSHA256: hex.EncodeToString(sum[:]), CanonicalSchema: canonical, schema: schema,
	}, nil
}

// resolveResultContractSchemaPath confines a schema path to the team
// directory, after symlink resolution, and returns its forward-slash ID.
func resolveResultContractSchemaPath(teamDir, schemaPath string) (string, string, error) {
	trimmed := strings.TrimSpace(schemaPath)
	if trimmed == "" {
		return "", "", errors.New("schema path is required")
	}
	if filepath.IsAbs(trimmed) || strings.HasPrefix(filepath.ToSlash(trimmed), "/") {
		return "", "", fmt.Errorf("schema path %q must be relative to the team directory", schemaPath)
	}
	id := path.Clean(filepath.ToSlash(trimmed))
	if id == "." || id == ".." || strings.HasPrefix(id, "../") {
		return "", "", fmt.Errorf("schema path %q escapes the team directory", schemaPath)
	}
	resolvedTeam, err := filepath.EvalSymlinks(teamDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve team directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(resolvedTeam, filepath.FromSlash(id)))
	if err != nil {
		return "", "", fmt.Errorf("resolve schema %q: %w", id, err)
	}
	if !isWithinRoot(resolvedTeam, resolved) {
		return "", "", fmt.Errorf("schema path %q resolves outside the team directory", schemaPath)
	}
	return id, resolved, nil
}

// decodeResultContractJSON decodes exactly one JSON value, preserving
// numbers so canonicalization never rewrites them.
func decodeResultContractJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the JSON value")
	}
	return value, nil
}

// validateResultContractSchemaNode enforces the result contract subset of
// JSON Schema that makes compilation deterministic and self-contained:
// no $id, references only to fragments of the same document, only the
// Draft 2020-12 dialect, and no payload property name that event redaction
// would rewrite (a redacted payload would no longer match its hash).
func validateResultContractSchemaNode(node any, pointer string) error {
	switch value := node.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := value[key]
			location := pointer + "/" + escapeJSONPointerToken(key)
			switch key {
			case "$id":
				return fmt.Errorf("%s: $id is not allowed; a result contract is identified by its file", location)
			case "$ref", "$dynamicRef", "$recursiveRef":
				ref, ok := child.(string)
				if !ok || !strings.HasPrefix(ref, "#") {
					return fmt.Errorf("%s: %s must reference a fragment of the same document (\"#...\")", location, key)
				}
			case "$schema":
				dialect, ok := child.(string)
				if pointer != "" || !ok || strings.TrimSuffix(dialect, "#") != resultContractDraft2020URI {
					return fmt.Errorf("%s: only the root may declare $schema, and only %q", location, resultContractDraft2020URI)
				}
			case "properties":
				if properties, ok := child.(map[string]any); ok {
					for name := range properties {
						if utils.IsRedactedJSONKey(name) {
							return fmt.Errorf("%s: property %q would be redacted in durable events; rename it", location, name)
						}
					}
				}
			case "required":
				if required, ok := child.([]any); ok {
					for _, entry := range required {
						if name, ok := entry.(string); ok && utils.IsRedactedJSONKey(name) {
							return fmt.Errorf("%s: required property %q would be redacted in durable events; rename it", location, name)
						}
					}
				}
			}
			if err := validateResultContractSchemaNode(child, location); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range value {
			if err := validateResultContractSchemaNode(child, fmt.Sprintf("%s/%d", pointer, index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func escapeJSONPointerToken(token string) string {
	return strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1")
}

// compileTeamResultContracts compiles every result contract declared by the
// team's agents and static contract tasks, and rejects the combinations the
// runtime does not support. Compiled schemas are keyed by contract ID; each
// declaration binds one of them with its own require-structured setting.
func compileTeamResultContracts(session *TeamSession) error {
	if session == nil {
		return nil
	}
	schemas := make(map[string]*CompiledResultContract)
	compile := func(owner string, spec *agent.ResultContractSpec) (ResultContractRef, *CompiledResultContract, error) {
		compiled, err := compileResultContractSchema(session.Dir, spec.Schema)
		if err != nil {
			return ResultContractRef{}, nil, fmt.Errorf("%s: %s: %w", resultContractInvalidCode, owner, err)
		}
		if existing := schemas[compiled.ID]; existing != nil {
			compiled = existing
		} else {
			schemas[compiled.ID] = compiled
		}
		return compiled.ref(spec.RequireStructured), compiled, nil
	}
	decisionAgents := decisionRoleEligibleAgents(session)
	checkBinding := func(owner string, def *agent.AgentDef, compiled *CompiledResultContract) error {
		if def == nil {
			return nil
		}
		if role := strings.ToLower(strings.TrimSpace(def.Role)); role == "coordinator" || role == "orchestrator" {
			return fmt.Errorf("%s: %s: result contracts apply only to workers, not %s agents", resultContractUnsupportedCode, owner, role)
		}
		if len(def.ExtraModels) > 0 {
			return fmt.Errorf("%s: %s: agent %q combines a result contract with extra-models", resultContractUnsupportedCode, owner, def.Name)
		}
		if reason, ok := decisionAgents[strings.ToLower(def.Name)]; ok {
			return fmt.Errorf("%s: %s: agent %q may be bound to the %s and cannot declare a result contract", resultContractUnsupportedCode, owner, def.Name, reason)
		}
		if agentUsesExternalBackend(session, def) && len(compiled.CanonicalSchema) > resultContractExternalSchemaMaxBytes {
			return fmt.Errorf("%s: %s: schema %q is %d canonical bytes, above the %d byte limit for an external agent backend", resultContractUnsupportedCode, owner, compiled.ID, len(compiled.CanonicalSchema), resultContractExternalSchemaMaxBytes)
		}
		return nil
	}

	agentRefs := make(map[string]ResultContractRef)
	for _, def := range uniqueSessionAgentDefs(session) {
		if def.ResultContract == nil {
			continue
		}
		owner := fmt.Sprintf("agent %q", def.Name)
		ref, compiled, err := compile(owner, def.ResultContract)
		if err != nil {
			return err
		}
		if err := checkBinding(owner, def, compiled); err != nil {
			return err
		}
		agentRefs[strings.ToLower(def.Name)] = ref
	}
	for i := range session.ContractTasks {
		task := &session.ContractTasks[i]
		if task.ResultContractSpec == nil {
			continue
		}
		owner := fmt.Sprintf("contract task %q", task.ID)
		ref, compiled, err := compile(owner, task.ResultContractSpec)
		if err != nil {
			return err
		}
		if err := checkBinding(owner, sessionAgentDef(session, task.Agent), compiled); err != nil {
			return err
		}
		task.ResultContract = &ref
	}
	session.ResultContracts = schemas
	session.AgentResultContracts = agentRefs
	return nil
}

// uniqueSessionAgentDefs returns each loaded agent definition once (the
// agent map holds file and name aliases), ordered by name.
func uniqueSessionAgentDefs(session *TeamSession) []*agent.AgentDef {
	seen := make(map[*agent.AgentDef]struct{}, len(session.Agents))
	defs := make([]*agent.AgentDef, 0, len(session.Agents))
	for _, def := range session.Agents {
		if def == nil {
			continue
		}
		if _, duplicate := seen[def]; duplicate {
			continue
		}
		seen[def] = struct{}{}
		defs = append(defs, def)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}

func sessionAgentDef(session *TeamSession, name string) *agent.AgentDef {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return nil
	}
	if def := session.Agents[name]; def != nil {
		return def
	}
	for _, def := range session.Agents {
		if def != nil && strings.EqualFold(def.Name, name) {
			return def
		}
	}
	return nil
}

// agentUsesExternalBackend reports whether an agent's configured provider
// hint names an external agent backend rather than the local LLM runtime.
func agentUsesExternalBackend(session *TeamSession, def *agent.AgentDef) bool {
	provider := strings.TrimSpace(def.SubagentProvider)
	if provider == "" && session != nil {
		provider = strings.TrimSpace(session.Config.SubagentProviderDefault)
	}
	return provider != "" && !strings.EqualFold(provider, localSubagentProviderName) && !strings.EqualFold(provider, "local")
}

// decisionRoleEligibleAgents returns, keyed by lower-case agent name, the
// agents the decision runtime could bind to a JUDGE, CHALLENGE (and REVISE,
// which reuses JUDGE bindings), or reference role: a role's pinned agent and
// every eligible worker whose declared capabilities satisfy the role's
// required capabilities. Roles are bound at run time, so this is the
// load-time superset; decision role binding re-checks eligibility.
func decisionRoleEligibleAgents(session *TeamSession) map[string]string {
	eligibleAgents := make(map[string]string)
	if session == nil || len(session.Config.Decision.Profiles) == 0 {
		return eligibleAgents
	}
	registry := NewCapabilityRegistry(session.Agents, session.Config.CapabilityRegistry).
		WithScoringWeights(session.Config.RoutingPolicy.Scoring.Weights)
	workers := sessionEligibleWorkerIDs(session)
	mark := func(agentID, reason string) {
		def := sessionAgentDef(session, agentID)
		if def == nil {
			return
		}
		if _, exists := eligibleAgents[strings.ToLower(def.Name)]; !exists {
			eligibleAgents[strings.ToLower(def.Name)] = reason
		}
	}
	consider := func(profileName, roleName string, required []string, pin *agent.RoutingPin) {
		reason := fmt.Sprintf("decision profile %q %s role", profileName, roleName)
		if pin != nil && strings.TrimSpace(pin.Agent) != "" {
			mark(pin.Agent, reason)
		}
		if len(required) == 0 {
			return
		}
		candidates, err := registry.Resolve(context.Background(), CapabilityQuery{Required: required}, workers)
		if err != nil {
			return
		}
		for _, candidate := range candidates {
			if candidate.Score > 0 {
				mark(candidate.AgentID, reason)
			}
		}
	}
	profileNames := make([]string, 0, len(session.Config.Decision.Profiles))
	for name := range session.Config.Decision.Profiles {
		profileNames = append(profileNames, name)
	}
	sort.Strings(profileNames)
	for _, name := range profileNames {
		profile := session.Config.Decision.Profiles[name]
		if role := profile.JudgeRole; role != nil {
			consider(name, "judge", role.RequiredCapabilities, role.Pin)
		}
		if role := profile.ChallengeRole; role != nil {
			consider(name, "challenge", role.RequiredCapabilities, role.Pin)
		}
		if role := profile.OutsideView.Role; role != nil {
			consider(name, "reference", role.RequiredCapabilities, role.Pin)
		}
	}
	return eligibleAgents
}

// sessionEligibleWorkerIDs mirrors Coordinator.eligibleWorkerIDs for load
// time, before a coordinator exists.
func sessionEligibleWorkerIDs(session *TeamSession) []string {
	allowed := session.Config.Delegation.AllowedWorkers
	names := make([]string, 0, len(session.Agents))
	for name := range session.Agents {
		if len(allowed) > 0 && !containsFold(allowed, name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), want) {
			return true
		}
	}
	return false
}

// decisionRoleResultContractIneligibility is the run-time counterpart of
// the load-time decision role check: an agent that declares a result
// contract is never bound to a decision role.
func decisionRoleResultContractIneligibility(roleName string, def *agent.AgentDef) error {
	if def != nil && def.ResultContract != nil {
		return fmt.Errorf("%s role routing: agent %q declares a result contract and cannot serve a decision role", roleName, def.Name)
	}
	return nil
}
