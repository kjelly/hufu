package team

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

func TestWorkerWorkspaceSpecDecodesStrictly(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{name: "isolated", yaml: "mode: isolated\nintegrate: on-verified\n"},
		{name: "shared", yaml: "mode: shared\n"},
		{name: "unknown key", yaml: "mode: isolated\nintegrate: on-verified\nisolation: true\n", want: "unknown key"},
		{name: "isolated without integrate", yaml: "mode: isolated\n", want: "requires integrate"},
		{name: "shared with integrate", yaml: "mode: shared\nintegrate: on-verified\n", want: "applies only"},
		{name: "unknown mode", yaml: "mode: sandbox\n", want: "unknown mode"},
		{name: "not a mapping", yaml: "isolated\n", want: "must be a mapping"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var spec agent.WorkerWorkspaceSpec
			err := yaml.Unmarshal([]byte(tt.yaml), &spec)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestWorkerWorkspaceSpecPrecedence(t *testing.T) {
	isolated := &agent.WorkerWorkspaceSpec{Mode: agent.WorkerWorkspaceIsolated, Integrate: agent.WorkerWorkspaceIntegrateOnVerified}
	shared := &agent.WorkerWorkspaceSpec{Mode: agent.WorkerWorkspaceShared}
	session := &TeamSession{Config: agent.TeamConfig{WorkerWorkspace: isolated}}
	if !workerWorkspaceSpecFor(session, &agent.AgentDef{Name: "coder", Role: "worker"}).Isolated() {
		t.Fatal("team default did not apply to a worker")
	}
	if workerWorkspaceSpecFor(session, &agent.AgentDef{Name: "coder", Role: "worker", WorkerWorkspace: shared}).Isolated() {
		t.Fatal("agent setting did not override the team default")
	}
	if workerWorkspaceSpecFor(session, &agent.AgentDef{Name: "lead", Role: "coordinator"}) != nil {
		t.Fatal("a coordinator inherited the worker workspace default")
	}
}

func isolatedSpec() *agent.WorkerWorkspaceSpec {
	return &agent.WorkerWorkspaceSpec{Mode: agent.WorkerWorkspaceIsolated, Integrate: agent.WorkerWorkspaceIntegrateOnVerified}
}

func TestValidateTeamWorkerWorkspacesRejectsUnsupportedAgents(t *testing.T) {
	tests := []struct {
		name  string
		def   *agent.AgentDef
		setup func(*TeamSession)
		want  string
	}{
		{name: "extra-models", def: &agent.AgentDef{Name: "coder", Role: "worker", Tools: "write", ExtraModels: []string{"ollama/b"}, WorkerWorkspace: isolatedSpec()}, want: "extra-models"},
		{name: "external backend", def: &agent.AgentDef{Name: "coder", Role: "worker", Tools: "write", SubagentProvider: "codex", WorkerWorkspace: isolatedSpec()}, want: "external agent backends"},
		{name: "mcp tools", def: &agent.AgentDef{Name: "coder", Role: "worker", Tools: "write", MCPTools: map[string]agent.MCPToolConfig{"x": {}}, WorkerWorkspace: isolatedSpec()}, want: "MCP tools"},
		{name: "phase workflow", def: &agent.AgentDef{Name: "coder", Role: "worker", Tools: "write", WorkerWorkspace: isolatedSpec()}, setup: func(s *TeamSession) {
			s.Config.Workflow.Phases = []string{"plan", "apply"}
		}, want: "phase workflow"},
		{name: "sudo", def: &agent.AgentDef{Name: "coder", Role: "worker", Tools: "write,sudo", WorkerWorkspace: isolatedSpec()}, want: "sudo"},
		{name: "scp", def: &agent.AgentDef{Name: "coder", Role: "worker", Tools: "write,scp", WorkerWorkspace: isolatedSpec()}, want: "scp"},
		{name: "terminal", def: &agent.AgentDef{Name: "coder", Role: "worker", Tools: "terminal_start", WorkerWorkspace: isolatedSpec()}, want: "terminal_start"},
		{name: "lua", def: &agent.AgentDef{Name: "coder", Role: "worker", Tools: "lua", WorkerWorkspace: isolatedSpec()}, want: "lua"},
		{name: "golang", def: &agent.AgentDef{Name: "coder", Role: "worker", Tools: "golang", WorkerWorkspace: isolatedSpec()}, want: "golang"},
		{name: "all tools", def: &agent.AgentDef{Name: "coder", Role: "worker", Tools: "all", WorkerWorkspace: isolatedSpec()}, want: "tools: all"},
		{name: "coordinator", def: &agent.AgentDef{Name: "lead", Role: "coordinator", WorkerWorkspace: isolatedSpec()}, want: "only to workers"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := &TeamSession{Agents: map[string]*agent.AgentDef{strings.ToLower(tt.def.Name): tt.def}}
			if tt.setup != nil {
				tt.setup(session)
			}
			err := validateTeamWorkerWorkspaces(session)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
	ok := &TeamSession{Agents: map[string]*agent.AgentDef{"coder": {Name: "coder", Role: "worker", Tools: "view,write,edit,bash,grep,glob,ls", WorkerWorkspace: isolatedSpec()}}}
	if err := validateTeamWorkerWorkspaces(ok); err != nil {
		t.Fatalf("a supported isolated coder was rejected: %v", err)
	}
}

func TestLoadTeamRefusesIsolatedWorkspacesUntilAttemptsRunInWorlds(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte("name: isolated-team\nworker-workspace:\n  mode: isolated\n  integrate: on-verified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeAgentFile(t, dir, "coder.md", "name: coder\ntools: view,write,edit,bash", "Code.")
	if _, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry); err == nil || !strings.Contains(err.Error(), workspaceIsolationUnsupportedCode) {
		t.Fatalf("LoadTeam error = %v, want %s", err, workspaceIsolationUnsupportedCode)
	}
	writeAgentFile(t, dir, "bad.md", "name: bad\nworker-workspace:\n  mode: isolated\n  integrate: on-verified\n  typo: true", "Bad.")
	if _, err := parseAgentFile(filepath.Join(dir, "bad.md"), nil); err == nil {
		t.Fatal("a malformed worker-workspace block was accepted")
	}
}

func isolatedAdmissionCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	c := newDirectTypedCoordinator(t, "view,write", nil, nil)
	registry := NewExecutionRegistry()
	if err := registry.Register(fakeLanguageModelBackend{fakeExecutionBackend{name: "local", kind: execution.BackendKindLLM}}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(fakeExecutionBackend{name: "external", kind: execution.BackendKindAgent}); err != nil {
		t.Fatal(err)
	}
	c.SetExecutionRegistry(registry)
	c.session.Config.WorkerWorkspace = isolatedSpec()
	c.session.Scope = WorkspaceScope{Managed: true, ControlRoot: t.TempDir(), SubjectRoot: t.TempDir()}
	return c
}

func TestAdmittedWorkerWorkspace(t *testing.T) {
	def := &agent.AgentDef{Name: "worker", Role: "worker"}
	local := execution.ExecutionTarget{Backend: "local", Model: "m"}
	writer := TaskDef{Agent: "worker", SideEffect: SideEffectWorkspaceWrite, ResolvedExecutionTarget: local}
	tests := []struct {
		name   string
		mutate func(*Coordinator, *TaskDef)
		mode   agent.WorkerWorkspaceMode
		want   string
	}{
		{name: "writer runs isolated", mode: agent.WorkerWorkspaceIsolated},
		{name: "read-only task runs shared", mutate: func(_ *Coordinator, task *TaskDef) { task.SideEffect = SideEffectNone }, mode: agent.WorkerWorkspaceShared},
		{name: "external write", mutate: func(_ *Coordinator, task *TaskDef) { task.SideEffect = SideEffectExternalWrite }, want: "cannot be contained"},
		{name: "agent backend", mutate: func(_ *Coordinator, task *TaskDef) {
			task.ResolvedExecutionTarget = execution.ExecutionTarget{Backend: "external", Model: "m"}
		}, want: "external agent backend"},
		{name: "structured steps", mutate: func(_ *Coordinator, task *TaskDef) {
			task.Execution.Steps = []ExecutionStep{{ID: "s"}}
		}, want: "structured steps"},
		{name: "fan-out", mutate: func(_ *Coordinator, task *TaskDef) { task.ModelTopology = []string{"a", "b"} }, want: "fan-out"},
		{name: "unmanaged workspace", mutate: func(c *Coordinator, _ *TaskDef) { c.session.Scope.Managed = false }, want: "managed workspace"},
		{name: "overlapping roots", mutate: func(c *Coordinator, _ *TaskDef) {
			c.session.Scope.ControlRoot = filepath.Join(c.session.Scope.SubjectRoot, ".hufu")
		}, want: "overlap"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := isolatedAdmissionCoordinator(t)
			task := writer
			if tt.mutate != nil {
				tt.mutate(c, &task)
			}
			policy, err := c.admittedWorkerWorkspace(task, def)
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("error = %v, want %q", err, tt.want)
				}
				return
			}
			if err != nil || policy == nil || policy.EffectiveMode != tt.mode || policy.Integrate != agent.WorkerWorkspaceIntegrateOnVerified {
				t.Fatalf("policy = %#v, err = %v", policy, err)
			}
		})
	}
	c := isolatedAdmissionCoordinator(t)
	if policy, err := c.admittedWorkerWorkspace(TaskDef{Agent: "worker", Sidecar: true, SideEffect: SideEffectWorkspaceWrite}, def); policy != nil || err != nil {
		t.Fatalf("sidecar policy = %#v, %v", policy, err)
	}
	c.session.Config.WorkerWorkspace = nil
	if policy, err := c.admittedWorkerWorkspace(writer, def); policy != nil || err != nil {
		t.Fatalf("shared default policy = %#v, %v", policy, err)
	}
}

func TestIsolatedTaskResourceScopeIsARootReadClaim(t *testing.T) {
	isolated, err := isolatedTaskResourceScope(nil, SideEffectWorkspaceWrite)
	if err != nil {
		t.Fatal(err)
	}
	read, _ := NewWorkspacePathResourceClaim(".", ResourceRead)
	if isolated.Version != taskResourceScopeIsolatedSnapshotVersion || isolated.Isolation != resourceScopeIsolationCopy || !slices.Contains(isolated.Claims, read) {
		t.Fatalf("isolated snapshot = %#v", isolated)
	}
	shared, err := wholeRootTaskResourceScope(nil, SideEffectWorkspaceWrite)
	if err != nil {
		t.Fatal(err)
	}
	if !claimsConflict(isolated.Claims, shared.Claims) {
		t.Fatal("an isolated writer does not conflict with a shared writer")
	}
	if claimsConflict(isolated.Claims, isolated.Claims) {
		t.Fatal("two isolated writers conflict")
	}
	if shared.Isolation != "" || shared.Version != taskResourceScopeSnapshotVersion {
		t.Fatalf("shared snapshot changed shape: %#v", shared)
	}

	mixed := *isolated
	mixed.Version = taskResourceScopeSnapshotVersion
	mixed.Digest = taskResourceScopeDigest(&mixed)
	if err := validateTaskResourceScopeSnapshot(&mixed, SideEffectWorkspaceWrite); err == nil {
		t.Fatal("a version 1 snapshot with isolation was accepted")
	}
	unmarked := *isolated
	unmarked.Isolation = ""
	unmarked.Digest = taskResourceScopeDigest(&unmarked)
	if err := validateTaskResourceScopeSnapshot(&unmarked, SideEffectWorkspaceWrite); err == nil {
		t.Fatal("a version 2 snapshot without isolation was accepted")
	}
	readOnlyWriter := *shared
	readOnlyWriter.Claims = []ResourceClaim{read}
	readOnlyWriter.Digest = taskResourceScopeDigest(&readOnlyWriter)
	if err := validateTaskResourceScopeSnapshot(&readOnlyWriter, SideEffectWorkspaceWrite); err == nil {
		t.Fatal("a shared writer with only a read claim was accepted")
	}

	c := isolatedAdmissionCoordinator(t)
	todo := &TodoItem{ID: "1", Agent: "worker", SideEffect: SideEffectWorkspaceWrite, WorkerWorkspace: &WorkerWorkspacePolicy{Mode: agent.WorkerWorkspaceIsolated, EffectiveMode: agent.WorkerWorkspaceIsolated}}
	snapshot, err := c.resolveNewTaskResourceScope(TaskDef{Agent: "worker", SideEffect: SideEffectWorkspaceWrite}, todo, ResolvedWorkerTools{})
	if err != nil || snapshot.Isolation != resourceScopeIsolationCopy {
		t.Fatalf("admitted isolated scope = %#v, %v", snapshot, err)
	}
}

func TestWorkerWorkspacePolicyIsDurable(t *testing.T) {
	policy := &WorkerWorkspacePolicy{Mode: agent.WorkerWorkspaceIsolated, Integrate: agent.WorkerWorkspaceIntegrateOnVerified, EffectiveMode: agent.WorkerWorkspaceIsolated}
	list := NewTaskTracker().TodoList()
	item := list.AddBatch([]TodoSpec{{Agent: "worker", Desc: "code", WorkerWorkspace: policy}})[0]
	if item.WorkerWorkspace == nil || *item.WorkerWorkspace != *policy || item.WorkerWorkspace == policy {
		t.Fatalf("TodoItem worker workspace = %#v", item.WorkerWorkspace)
	}
	encoded, err := json.Marshal(taskTransitionPayloadWithCoordinator(item, nil))
	if err != nil {
		t.Fatal(err)
	}
	items := ReduceToTodoList([]RunEvent{{ID: "evt-1", Type: string(EventTaskCreated), TaskID: item.ID, Payload: encoded}})
	if len(items) != 1 || items[0].WorkerWorkspace == nil || *items[0].WorkerWorkspace != *policy {
		t.Fatalf("reduced worker workspace = %#v", items)
	}
	if task := taskDefFromTodoItem(items[0]); task.WorkerWorkspace == nil || !task.WorkerWorkspace.isolated() {
		t.Fatalf("resumed task worker workspace = %#v", task.WorkerWorkspace)
	}
	if _, err := decodeModelTaskDefs([]byte(`{"tasks":[{"agent":"worker","goal":"x","worker_workspace":{"mode":"isolated"}}]}`)); err == nil {
		t.Fatal("a coordinator payload set worker_workspace")
	}
}

func TestIsolatedResolvedToolsUnsupported(t *testing.T) {
	if err := isolatedResolvedToolsUnsupported(ResolvedWorkerTools{AuthorizedNames: []string{"view", "write", "bash"}}); err != nil {
		t.Fatalf("supported tools rejected: %v", err)
	}
	if err := isolatedResolvedToolsUnsupported(ResolvedWorkerTools{AuthorizedNames: []string{"view", "terminal_start"}}); err == nil {
		t.Fatal("a terminal tool was accepted")
	}
	if err := isolatedResolvedToolsUnsupported(ResolvedWorkerTools{DynamicTargets: []DynamicToolTarget{{}}}); err == nil {
		t.Fatal("dynamic MCP targets were accepted")
	}
}
