package team

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

func writeResultContractSchema(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const reviewResultSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["verdict"],
  "properties": {
    "verdict": {"enum": ["approve", "reject"]},
    "max_tokens": {"type": "integer"},
    "findings": {"type": "array", "items": {"$ref": "#/$defs/finding"}}
  },
  "additionalProperties": false,
  "$defs": {"finding": {"type": "object", "properties": {"summary": {"type": "string"}}}}
}`

func TestCompileResultContractSchemaAcceptsSelfContainedDraft2020(t *testing.T) {
	dir := t.TempDir()
	writeResultContractSchema(t, dir, "schemas/review.json", reviewResultSchema)
	compiled, err := compileResultContractSchema(dir, " ./schemas/review.json ")
	if err != nil {
		t.Fatal(err)
	}
	if compiled.ID != "schemas/review.json" || len(compiled.SchemaSHA256) != 64 || compiled.schema == nil {
		t.Fatalf("compiled = %#v", compiled)
	}
	// Reordered keys and different whitespace canonicalize to the same hash.
	var decoded map[string]any
	if err := json.Unmarshal([]byte(reviewResultSchema), &decoded); err != nil {
		t.Fatal(err)
	}
	reordered, err := json.MarshalIndent(decoded, "", "\t")
	if err != nil {
		t.Fatal(err)
	}
	writeResultContractSchema(t, dir, "schemas/reordered.json", string(reordered))
	again, err := compileResultContractSchema(dir, "schemas/reordered.json")
	if err != nil {
		t.Fatal(err)
	}
	if again.SchemaSHA256 != compiled.SchemaSHA256 {
		t.Fatalf("canonical hash differs for an equivalent schema: %s vs %s", again.SchemaSHA256, compiled.SchemaSHA256)
	}
}

func TestCompileResultContractSchemaRejectsUnsafeSchemas(t *testing.T) {
	outside := t.TempDir()
	writeResultContractSchema(t, outside, "outside.json", `{"type":"object"}`)
	tests := []struct {
		name    string
		path    string
		content string
		setup   func(t *testing.T, dir string)
		want    string
	}{
		{name: "absolute path", path: filepath.Join(outside, "outside.json"), want: "must be relative"},
		{name: "parent traversal", path: "../outside.json", want: "escapes the team directory"},
		{name: "symlink escape", path: "schemas/link.json", setup: func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, "schemas"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(outside, "outside.json"), filepath.Join(dir, "schemas", "link.json")); err != nil {
				t.Skipf("symlink unsupported: %v", err)
			}
		}, want: "outside the team directory"},
		{name: "missing file", path: "schemas/missing.json", want: "resolve schema"},
		{name: "empty path", path: " ", want: "schema path is required"},
		{name: "oversized", path: "big.json", content: `{"description":"` + strings.Repeat("x", resultContractSchemaMaxBytes) + `"}`, want: "byte limit"},
		{name: "non-object root", path: "array.json", content: `[]`, want: "must be a JSON object"},
		{name: "trailing data", path: "trailing.json", content: `{"type":"object"} {}`, want: "trailing data"},
		{name: "id", path: "id.json", content: `{"$id":"https://example.com/x","type":"object"}`, want: "$id is not allowed"},
		{name: "remote ref", path: "remote.json", content: `{"$ref":"https://example.com/schema.json"}`, want: "same document"},
		{name: "sibling file ref", path: "sibling.json", content: `{"$ref":"other.json#/x"}`, want: "same document"},
		{name: "nested schema dialect", path: "nested.json", content: `{"type":"object","properties":{"a":{"$schema":"https://json-schema.org/draft/2020-12/schema"}}}`, want: "only the root may declare $schema"},
		{name: "other dialect", path: "draft7.json", content: `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object"}`, want: "only the root may declare $schema"},
		{name: "redacted property", path: "secret.json", content: `{"type":"object","properties":{"api_key":{"type":"string"}}}`, want: `property "api_key" would be redacted`},
		{name: "redacted required", path: "token.json", content: `{"type":"object","required":["session_token"]}`, want: `required property "session_token" would be redacted`},
		{name: "invalid schema", path: "invalid.json", content: `{"type":5}`, want: "compile schema"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.content != "" {
				writeResultContractSchema(t, dir, tt.path, tt.content)
			}
			if tt.setup != nil {
				tt.setup(t, dir)
			}
			_, err := compileResultContractSchema(dir, tt.path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func resultContractSession(t *testing.T, defs ...*agent.AgentDef) *TeamSession {
	t.Helper()
	dir := t.TempDir()
	writeResultContractSchema(t, dir, "schemas/review.json", reviewResultSchema)
	session := &TeamSession{Dir: dir, Agents: map[string]*agent.AgentDef{}}
	for _, def := range defs {
		session.Agents[strings.ToLower(def.Name)] = def
	}
	return session
}

func TestCompileTeamResultContractsBindsAgentsAndStaticTasks(t *testing.T) {
	reviewer := &agent.AgentDef{Name: "Reviewer", Role: "worker", ResultContract: &agent.ResultContractSpec{Schema: "schemas/review.json", RequireStructured: true}}
	coder := &agent.AgentDef{Name: "coder", Role: "worker"}
	session := resultContractSession(t, reviewer, coder)
	session.Agents["reviewer-file"] = reviewer // a file alias of the same definition
	session.ContractTasks = []TaskDef{
		{ID: "final-review", Agent: "coder", ResultContractSpec: &agent.ResultContractSpec{Schema: "schemas/review.json"}},
		{ID: "plain", Agent: "coder"},
	}
	if err := compileTeamResultContracts(session); err != nil {
		t.Fatal(err)
	}
	if len(session.ResultContracts) != 1 {
		t.Fatalf("compiled schemas = %d, want one shared schema", len(session.ResultContracts))
	}
	ref, ok := session.AgentResultContracts["reviewer"]
	if !ok || ref.ID != "schemas/review.json" || !ref.RequireStructured {
		t.Fatalf("reviewer binding = %#v (present=%v)", ref, ok)
	}
	if _, ok := session.AgentResultContracts["coder"]; ok {
		t.Fatal("an agent without a contract was bound")
	}
	static := session.ContractTasks[0].ResultContract
	if static == nil || static.RequireStructured || static.SchemaSHA256 != ref.SchemaSHA256 {
		t.Fatalf("static binding = %#v", static)
	}
	if session.ContractTasks[1].ResultContract != nil {
		t.Fatal("a static task without a contract was bound")
	}
}

func TestCompileTeamResultContractsRejectsUnsupportedCombinations(t *testing.T) {
	spec := func() *agent.ResultContractSpec { return &agent.ResultContractSpec{Schema: "schemas/review.json"} }
	tests := []struct {
		name  string
		def   *agent.AgentDef
		setup func(session *TeamSession)
		want  string
	}{
		{name: "extra-models", def: &agent.AgentDef{Name: "worker", Role: "worker", ExtraModels: []string{"ollama/b"}, ResultContract: spec()}, want: "extra-models"},
		{name: "coordinator", def: &agent.AgentDef{Name: "lead", Role: "coordinator", ResultContract: spec()}, want: "only to workers"},
		{name: "decision role pin", def: &agent.AgentDef{Name: "judge", Role: "worker", ResultContract: spec()}, setup: func(session *TeamSession) {
			session.Config.Decision.Profiles = map[string]agent.DecisionPolicy{
				"standard": {JudgeRole: &agent.JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}, Pin: &agent.RoutingPin{Agent: "judge"}}},
			}
		}, want: "judge role"},
		{name: "decision role capability", def: &agent.AgentDef{Name: "critic", Role: "worker", ResultContract: spec()}, setup: func(session *TeamSession) {
			session.Config.Decision.Profiles = map[string]agent.DecisionPolicy{
				"standard": {ChallengeRole: &agent.ChallengeRolePolicy{RequiredCapabilities: []string{"adversarial-analysis"}}},
			}
			session.Config.CapabilityRegistry = map[string][]agent.DeclaredCapability{"critic": {{Capability: "adversarial-analysis", Confidence: 1}}}
		}, want: "challenge role"},
		{name: "oversized schema for an external backend", def: &agent.AgentDef{Name: "coder", Role: "worker", SubagentProvider: "codex", ResultContract: &agent.ResultContractSpec{Schema: "schemas/big.json"}}, setup: func(session *TeamSession) {
			writeResultContractSchema(t, session.Dir, "schemas/big.json", `{"type":"object","description":"`+strings.Repeat("x", resultContractExternalSchemaMaxBytes)+`"}`)
		}, want: "external agent backend"},
		{name: "invalid schema", def: &agent.AgentDef{Name: "worker", Role: "worker", ResultContract: &agent.ResultContractSpec{Schema: "schemas/missing.json"}}, want: resultContractInvalidCode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := resultContractSession(t, tt.def)
			if tt.setup != nil {
				tt.setup(session)
			}
			err := compileTeamResultContracts(session)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestLoadTeamRefusesResultContractsUntilPayloadsAreValidated(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte("name: contract-team\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeAgentFile(t, dir, "worker.md", "name: worker\nresult-contract:\n  schema: schemas/review.json\n  require-structured: true", "You are a worker.")
	writeResultContractSchema(t, dir, "schemas/review.json", reviewResultSchema)
	if _, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry); err == nil || !strings.Contains(err.Error(), resultContractUnsupportedCode) {
		t.Fatalf("LoadTeam error = %v, want %s", err, resultContractUnsupportedCode)
	}

	plain := t.TempDir()
	if err := os.WriteFile(filepath.Join(plain, "team.yml"), []byte("name: plain-team\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeAgentFile(t, plain, "worker.md", "name: worker", "You are a worker.")
	if _, err := LoadTeam(plain, nil, nil, DefaultProviderRegistry); err != nil {
		t.Fatalf("LoadTeam without contracts: %v", err)
	}
}

func TestAgentFrontmatterParsesResultContract(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, dir, "reviewer.md", "name: reviewer\nresult-contract:\n  schema: schemas/review.json\n  require-structured: true", "Review.")
	def, err := parseAgentFile(filepath.Join(dir, "reviewer.md"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if def.ResultContract == nil || def.ResultContract.Schema != "schemas/review.json" || !def.ResultContract.RequireStructured {
		t.Fatalf("ResultContract = %#v", def.ResultContract)
	}
	writeAgentFile(t, dir, "bad.md", "name: bad\nresult-contract:\n  schema: schemas/review.json\n  strict: true", "Bad.")
	if _, err := parseAgentFile(filepath.Join(dir, "bad.md"), nil); err == nil {
		t.Fatal("unknown result-contract key was accepted")
	}
}

func TestAdmittedResultContractPrecedence(t *testing.T) {
	staticRef := &ResultContractRef{ID: "schemas/final.json", SchemaSHA256: "static"}
	agentRef := ResultContractRef{ID: "schemas/review.json", SchemaSHA256: "agent", RequireStructured: true}
	def := &agent.AgentDef{Name: "Reviewer"}
	c := &Coordinator{session: &TeamSession{AgentResultContracts: map[string]ResultContractRef{"reviewer": agentRef}}}
	tests := []struct {
		name string
		task TaskDef
		want *ResultContractRef
	}{
		{name: "agent default", task: TaskDef{Agent: "reviewer"}, want: &agentRef},
		{name: "static contract wins", task: TaskDef{Agent: "reviewer", ResultContract: staticRef}, want: staticRef},
		{name: "sidecar never binds", task: TaskDef{Agent: "reviewer", Sidecar: true, ResultContract: staticRef}, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.admittedResultContract(tt.task, def)
			if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
				t.Fatalf("admittedResultContract = %#v, want %#v", got, tt.want)
			}
		})
	}
	if got := c.admittedResultContract(TaskDef{Agent: "coder"}, &agent.AgentDef{Name: "coder"}); got != nil {
		t.Fatalf("agent without a contract bound %#v", got)
	}
}

func TestDecodeModelTaskDefsRejectsResultContractFields(t *testing.T) {
	tests := []string{
		`{"tasks":[{"agent":"worker","goal":"x","result_contract":{"id":"a"}}]}`,
		`{"tasks":[{"agent":"worker","goal":"x","Result-Contract":{"schema":"a"}}]}`,
		`{"tasks":[{"agent":"worker","goal":"x","structured_payload":{}}]}`,
		`{"tasks":[{"agent":"worker","goal":"x","execution":{"result":{"schema":"a"}}}]}`,
		`{"tasks":[{"agent":"worker","goal":"x","Execution":{"Result_Contract":{}}}]}`,
	}
	for _, payload := range tests {
		if _, err := decodeModelTaskDefs([]byte(payload)); err == nil {
			t.Fatalf("decodeModelTaskDefs accepted %s", payload)
		}
	}
	if _, err := decodeModelTaskDefs([]byte(`{"tasks":[{"agent":"worker","goal":"x","execution":{"requires_result":true}}]}`)); err != nil {
		t.Fatalf("ordinary execution contract rejected: %v", err)
	}
}

func TestEffectiveContractHashIncludesStaticResultContract(t *testing.T) {
	base := TaskDef{ID: "review", Agent: "reviewer"}
	withContract := base
	withContract.ResultContractSpec = &agent.ResultContractSpec{Schema: "schemas/review.json"}
	plain, err := effectiveContractHash("review", "reviewer", ExecutionContract{}, "", "", "", 0, nil, nil, false, base)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := effectiveContractHash("review", "reviewer", ExecutionContract{}, "", "", "", 0, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	contracted, err := effectiveContractHash("review", "reviewer", ExecutionContract{}, "", "", "", 0, nil, nil, false, withContract)
	if err != nil {
		t.Fatal(err)
	}
	if plain != legacy {
		t.Fatalf("a contract without a result contract changed its hash: %s vs %s", plain, legacy)
	}
	if contracted == plain {
		t.Fatal("a static result contract did not change the contract hash")
	}
}

func TestResultContractRefIsDurable(t *testing.T) {
	ref := &ResultContractRef{ID: "schemas/review.json", SchemaSHA256: strings.Repeat("a", 64), RequireStructured: true}
	list := NewTaskTracker().TodoList()
	item := list.AddBatch([]TodoSpec{{Agent: "worker", Desc: "review", ResultContract: ref, ExecutionTarget: execution.ExecutionTarget{Backend: "ollama", Model: "m"}}})[0]
	if item.ResultContract == nil || *item.ResultContract != *ref || item.ResultContract == ref {
		t.Fatalf("TodoItem result contract = %#v, want an independent copy of %#v", item.ResultContract, ref)
	}
	payload := taskTransitionPayloadWithCoordinator(item, nil)
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		ResultContract *ResultContractRef `json:"result_contract"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ResultContract == nil || *decoded.ResultContract != *ref {
		t.Fatalf("lifecycle payload result contract = %#v", decoded.ResultContract)
	}
	items := ReduceToTodoList([]RunEvent{{
		ID: "evt-1", Type: string(EventTaskCreated), TaskID: item.ID, Payload: encoded,
	}})
	if len(items) != 1 || items[0].ResultContract == nil || *items[0].ResultContract != *ref {
		t.Fatalf("reduced result contract = %#v", items)
	}
	plain := list.AddBatch([]TodoSpec{{Agent: "worker", Desc: "plain"}})[0]
	plainPayload, err := json.Marshal(taskTransitionPayloadWithCoordinator(plain, nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plainPayload), "result_contract") {
		t.Fatalf("payload without a contract carries result_contract: %s", plainPayload)
	}
}

func TestExecutionPolicySnapshotPinsResultContracts(t *testing.T) {
	session := &TeamSession{
		AgentResultContracts: map[string]ResultContractRef{"reviewer": {ID: "schemas/review.json", SchemaSHA256: "b"}},
		ContractTasks:        []TaskDef{{ID: "final", ResultContract: &ResultContractRef{ID: "schemas/final.json", SchemaSHA256: "a", RequireStructured: true}}},
	}
	got := executionPolicyResultContracts(session)
	want := []ExecutionResultContractPolicySnapshot{
		{Owner: "agent:reviewer", ID: "schemas/review.json", SchemaSHA256: "b"},
		{Owner: "contract:final", ID: "schemas/final.json", SchemaSHA256: "a", RequireStructured: true},
	}
	if len(got) != len(want) {
		t.Fatalf("bindings = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("binding %d = %#v, want %#v", i, got[i], want[i])
		}
	}
	if bindings := executionPolicyResultContracts(&TeamSession{}); bindings != nil {
		t.Fatalf("a team without contracts produced bindings %#v", bindings)
	}
}

func TestDecisionRoleBindingRejectsResultContractAgent(t *testing.T) {
	if err := decisionRoleResultContractIneligibility("judge", &agent.AgentDef{Name: "judge", ResultContract: &agent.ResultContractSpec{Schema: "x.json"}}); err == nil {
		t.Fatal("a result-contract agent was eligible for a decision role")
	}
	if err := decisionRoleResultContractIneligibility("judge", &agent.AgentDef{Name: "judge"}); err != nil {
		t.Fatalf("an ordinary agent was rejected: %v", err)
	}
}
