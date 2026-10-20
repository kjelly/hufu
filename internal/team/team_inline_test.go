package team

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team/preset"
	"gopkg.in/yaml.v3"
)

func inlineManifestFixture(body string, versioned bool) string {
	if !versioned {
		return body
	}
	return "apiVersion: hufu.io/v1alpha1\nkind: AgentTeam\nmetadata: {name: inline-team}\nspec:\n" + indentContractFixture(body, 2)
}

func TestTeamInlineAgentParity(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		for _, name := range preset.Names() {
			t.Run(fmt.Sprintf("%s/versioned=%v", name, versioned), func(t *testing.T) {
				compact, expanded := t.TempDir(), t.TempDir()
				body := "Perform the Worker role for this team."
				manifest := "name: inline-team\nmodel: ollama/team-model\n"
				writeLintTestFile(t, filepath.Join(compact, "team.yaml"), inlineManifestFixture(manifest+"agents:\n  Worker: ' "+strings.ToUpper(name)+" '\n", versioned))
				writeLintTestFile(t, filepath.Join(expanded, "team.yaml"), manifest)
				writeAgentFile(t, expanded, "Worker.md", "description: Worker\npreset: "+name, body)
				a, err := CompileTeam(compact, nil, nil, DefaultProviderRegistry)
				if err != nil {
					t.Fatal(err)
				}
				b, err := CompileTeam(expanded, nil, nil, DefaultProviderRegistry)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(a.RuntimeSession().Agents, b.RuntimeSession().Agents) {
					t.Fatalf("AgentDefs differ: inline=%+v markdown=%+v", a.RuntimeSession().Agents["worker"], b.RuntimeSession().Agents["worker"])
				}
				if a.Agents["worker"].Name.Source != SourceTeam || a.Agents["worker"].Tools.Source != SourcePreset {
					t.Fatalf("missing inline provenance: %+v", a.Agents["worker"])
				}
			})
		}
	}
}

func TestTeamInlineMappingParity(t *testing.T) {
	root := t.TempDir()
	fields := "preset: coding\ndescription: Implements changes\nmodel: ollama/own-model\nreasoning-effort: high\ntools:\n  allowed: [math]\n  denied: [bash]\nskills: [example]\nallowed-paths: [" + root + "]\nno-net: true\ntimeout: 35\nmax-steps: 6\nmax-retries: 0\n"
	for _, versioned := range []bool{false, true} {
		compact, expanded := t.TempDir(), t.TempDir()
		manifest := "name: inline-team\nvars: {task: Implement}\nno-net: true\nworker-model: ollama/team-model\n"
		writeLintTestFile(t, filepath.Join(compact, "team.yaml"), inlineManifestFixture(manifest+"agents:\n  coder:\n"+indentContractFixture(fields+"instructions: |\n  {@ .task @} changes.\n  Verify the result.\n", 4), versioned))
		writeLintTestFile(t, filepath.Join(expanded, "team.yaml"), manifest)
		writeAgentFile(t, expanded, "coder.md", fields, "{@ .task @} changes.\nVerify the result.")
		a, err := CompileTeam(compact, nil, nil, DefaultProviderRegistry)
		if err != nil {
			t.Fatal(err)
		}
		b, err := CompileTeam(expanded, nil, nil, DefaultProviderRegistry)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a.RuntimeSession().Agents, b.RuntimeSession().Agents) {
			t.Fatalf("mapping changed AgentDef: inline=%+v markdown=%+v", a.RuntimeSession().Agents["coder"], b.RuntimeSession().Agents["coder"])
		}
		worker := a.RuntimeSession().Agents["coder"]
		if worker.MaxRetries != 0 || worker.Timeout != 35 || worker.MaxSteps != 6 || !worker.NoNet || slices.Contains(strings.Split(worker.Tools, ","), "bash") || !slices.Contains(strings.Split(worker.Tools, ","), "math") {
			t.Fatalf("lost explicit policies: %+v", worker)
		}
	}
}

func TestTeamInlineCannotUndoTeamToolPolicy(t *testing.T) {
	dir := t.TempDir()
	writeLintTestFile(t, filepath.Join(dir, "team.yaml"), "no-net: true\ntools: {denied: [math]}\nagents:\n  worker:\n    preset: readonly\n    no-net: false\n    tools: {allowed: [fetch, math]}\n    instructions: Use `fetch` tool. Use `math` tool.\n")
	result, err := LintTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil || !result.Complete {
		t.Fatalf("lint=%+v err=%v", result, err)
	}
	for _, name := range []string{"fetch", "math"} {
		if !slices.ContainsFunc(result.Findings, func(f TeamLintFinding) bool {
			return f.Code == FindingPromptDeniedTool && f.Agent == "worker" && strings.Contains(f.Message, name)
		}) {
			t.Fatalf("inline grant bypassed team policy for %s: %+v", name, result.Findings)
		}
	}
}

func TestTeamInlineStrictDecoding(t *testing.T) {
	invalid := []string{
		"agents: null", "agents: []", "agents: readonly", "agents: {worker: null}", "agents: {worker: 42}", "agents: {worker: false}", "agents: {worker: []}",
		"agents: {worker: {}}", "agents: {worker: {preset: ''}}", "agents: {worker: {preset: true}}", "agents: {worker: {preset: readonly, description: ' '}}",
		"agents: {worker: {preset: readonly, instructions: ''}}", "agents: {worker: {preset: readonly, model: 7}}", "agents: {worker: {preset: readonly, reasoning-effort: false}}",
		"agents: {worker: {preset: readonly, skills: [3]}}", "agents: {worker: {preset: readonly, allowed-paths: null}}", "agents: {worker: {preset: readonly, no-net: 'true'}}",
		"agents: {worker: {preset: readonly, timeout: -1}}", "agents: {worker: {preset: readonly, max-steps: 1.5}}", "agents: {worker: {preset: readonly, max-retries: 18446744073709551616}}",
		"agents: {worker: {preset: readonly, tools: ''}}", "agents: {worker: {preset: readonly, tools: [null]}}", "agents: {worker: {preset: readonly, tools: ALL}}",
		"agents: {worker: {preset: readonly, tools: {allow: view}}}", "agents: {worker: {preset: readonly, tools: {allowed: 'view, '}}}",
		"agents: {worker: readonly, Worker: coding}", "agents: {worker: readonly, worker: coding}", "agents: {7: readonly}", "agents: {'../bad': readonly}",
		"agents: &cycle {worker: *cycle}",
		"vars: {empty: &empty null}\nagents: *empty",
	}
	for _, field := range []string{"role", "name", "system", "prompt", "permissions", "provider", "execute-after", "requires", "result-contract", "worker-workspace", "execution-route", "memory", "mcp-tools", "side_effect"} {
		invalid = append(invalid, "agents: {worker: {preset: readonly, "+field+": value}}")
	}
	for _, input := range invalid {
		for _, versioned := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/versioned=%v", input, versioned), func(t *testing.T) {
				dir := t.TempDir()
				writeLintTestFile(t, filepath.Join(dir, "team.yaml"), inlineManifestFixture(input+"\n", versioned))
				if _, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry); err == nil {
					t.Fatal("invalid inline declaration accepted")
				}
				if _, err := LintTeam(dir, nil, nil, DefaultProviderRegistry); err == nil {
					t.Fatal("structural failure must be a load error")
				}
			})
		}
	}
}

func TestTeamInlineSemanticFailures(t *testing.T) {
	for _, test := range []struct{ id, value, code string }{
		{"helper", "readonly", FindingInlineAgentInvalid}, {"CoOrDiNaToR", "readonly", FindingInlineAgentInvalid}, {"orchestrator-helper", "readonly", FindingInlineAgentInvalid},
		{"worker", "coding-reviewed", FindingInlineAgentInvalid}, {"worker", "unknown", FindingInlineAgentInvalid},
		{"worker", "{preset: readonly, tools: {denied: [view, grep, glob, ls]}}", FindingInlineAgentToolsEmpty},
	} {
		for _, versioned := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%s/versioned=%v", test.id, test.value, versioned), func(t *testing.T) {
				dir := t.TempDir()
				writeLintTestFile(t, filepath.Join(dir, "team.yaml"), inlineManifestFixture("agents:\n  "+test.id+": "+test.value+"\n", versioned))
				if _, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry); err == nil {
					t.Fatal("unsafe worker compiled")
				}
				result, err := LintTeam(dir, nil, nil, DefaultProviderRegistry)
				if err != nil || result.Complete || len(result.Findings) != 1 {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				f := result.Findings[0]
				prefix, line := "", 2
				if versioned {
					prefix, line = "spec.", 6
				}
				field := prefix + "agents." + test.id
				if test.code == FindingInlineAgentToolsEmpty {
					field += ".tools"
				}
				if f.Code != test.code || f.File != "team.yaml" || f.Line != line || f.FieldPath != field || f.Suggestion == "" {
					t.Fatalf("finding=%+v", f)
				}
			})
		}
	}
}

func TestTeamInlineThreeSourceComposition(t *testing.T) {
	for _, test := range []struct{ manifest, file, md string }{
		{"preset: coding-reviewed\nagents: {reviewer: review}\n", "", ""},
		{"agents: {architect: readonly}\n", "ARCHITECT.md", "preset: readonly"},
		{"agents: {architect: readonly}\n", "custom.md", "name: architect\npreset: readonly"},
		{"agents: {architect: readonly}\n", "architect.md", "name: custom\npreset: readonly"},
	} {
		dir := t.TempDir()
		writeLintTestFile(t, filepath.Join(dir, "team.yaml"), test.manifest)
		if test.file != "" {
			writeAgentFile(t, dir, test.file, test.md, "Inspect.")
		}
		if _, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry); err == nil || !strings.Contains(err.Error(), "collision") {
			t.Fatalf("collision not rejected: %v", err)
		}
		result, err := LintTeam(dir, nil, nil, DefaultProviderRegistry)
		if err != nil || result.Complete || !slices.ContainsFunc(result.Findings, func(f TeamLintFinding) bool { return f.Code == FindingDuplicateAgent }) {
			t.Fatalf("collision lint=%+v err=%v", result, err)
		}
	}
	dir := t.TempDir()
	writeLintTestFile(t, filepath.Join(dir, "team.yaml"), "preset: coding-reviewed\nagents: {architect: readonly}\n")
	writeAgentFile(t, dir, "security.md", "name: auditor\npreset: review", "Review.")
	writeAgentFile(t, dir, "coordinator.md", "role: coordinator", "Coordinate.")
	spec, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	session := spec.RuntimeSession()
	if session.Config.Vars["AGENT_COUNT"] != "5" || session.Config.Vars["AGENT_NAMES"] != "Helper, architect, auditor, developer, reviewer" || session.Agents["security"] != session.Agents["auditor"] {
		t.Fatalf("incorrect alias/roster: %+v", session.Config.Vars)
	}
	if (&Coordinator{session: session}).GetOrchestratorDef().Name != "coordinator" {
		t.Fatal("custom Markdown coordinator lost")
	}
}

func TestTeamInlineTemplateTiming(t *testing.T) {
	for _, test := range []struct {
		manifest string
		valid    bool
		want     string
	}{
		{"vars: {task: inspect}\nagents: {worker: {preset: readonly, instructions: '{@ .task @}'}}\n", true, "override"},
		{"agents: {worker: {preset: readonly, instructions: '{@ .TEAM_NAME @}'}}\n", false, ""},
		{"vars: {TEAM_NAME: supplied}\nagents: {worker: {preset: readonly, instructions: '{@ .TEAM_NAME @}'}}\n", true, "supplied"},
		{"vars: {AGENT_COUNT: '7'}\nagents: {worker: {preset: readonly, instructions: '{@ .AGENT_COUNT @}'}}\n", true, "7"},
		{"vars: {recursive: '{@ .missing @}'}\nagents: {worker: {preset: readonly, instructions: '{@ .recursive @}'}}\n", false, ""},
		{"agents: {worker: {preset: readonly, instructions: 'unclosed @}'}}\n", false, ""},
	} {
		dir := t.TempDir()
		writeLintTestFile(t, filepath.Join(dir, "team.yaml"), test.manifest)
		spec, err := CompileTeam(dir, map[string]string{"task": "override"}, nil, DefaultProviderRegistry)
		if !test.valid {
			if err == nil {
				t.Fatalf("invalid template accepted: %s", test.manifest)
			}
			continue
		}
		if err != nil || spec.RuntimeSession().Agents["worker"].System != test.want {
			t.Fatalf("template=%s spec=%+v err=%v", test.manifest, spec, err)
		}
	}
	// Legacy Markdown still receives the loader's automatic TEAM_NAME.
	dir := t.TempDir()
	writeLintTestFile(t, filepath.Join(dir, "team.yaml"), "name: inline-team\nagents: {inline: readonly}\n")
	writeAgentFile(t, dir, "legacy.md", "preset: readonly", "{@ .TEAM_NAME @}")
	spec, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil || spec.RuntimeSession().Agents["legacy"].System != "inline-team" {
		t.Fatalf("Markdown timing changed: %v", err)
	}
}

func TestTeamInlineBindingsAndDefaults(t *testing.T) {
	manifest := "name: inline-team\nworker-memory: {mode: session, max-items: 8}\nworker-workspace: {mode: isolated, integrate: on-verified}\nexecution-route: coding\ndelegation:\n  allowed-workers: [runtime-engineer, network-engineer, critic]\n  bind-task-goal-contracts: true\ncontracts:\n  - id: inspect\n    agent: runtime-engineer\n    goal: Inspect runtime\n    result-contract: {schema: schemas/review.json, require-structured: true}\n" + actionCatalogTestProviders + "action-catalog:\n" + actionCatalogTestEntry
	workers := "agents:\n  runtime-engineer: readonly\n  network-engineer: readonly\n  critic: readonly\n"
	dir := t.TempDir()
	writeResultContractSchema(t, dir, "schemas/review.json", reviewResultSchema)
	writeLintTestFile(t, filepath.Join(dir, "team.yaml"), manifest+workers)
	spec, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	session := spec.RuntimeSession()
	for _, name := range []string{"runtime-engineer", "network-engineer", "critic"} {
		def := session.Agents[name]
		if def.Memory.Mode != agent.WorkerMemorySession || def.Memory.MaxItems != 8 || def.MemoryID != name || !workerWorkspaceSpecFor(session, def).Isolated() {
			t.Fatalf("defaults did not reach %s: %+v", name, def)
		}
	}
	bound, compiled, err := CompileTaskGoalContracts(session, []TaskDef{{Agent: "runtime-engineer", ContractID: "inspect", Goal: "Inspect runtime"}})
	if err != nil || len(compiled) != 1 || len(bound) != 1 || bound[0].ResultContract == nil || !bound[0].ResultContract.RequireStructured {
		t.Fatalf("contract binding failed: bound=%+v compiled=%+v err=%v", bound, compiled, err)
	}
	if catalog, ok := session.ActionCatalog.Lookup("collect-debug-bundle"); !ok || catalog.Agent != "runtime-engineer" {
		t.Fatalf("action catalog did not bind: %+v", session.ActionCatalog)
	}
	routes := map[string]config.ExecutionRouteConfig{"coding": {Candidates: []string{"ollama/route-model"}}}
	c := routeRegistryCoordinator(t, session.Agents, routes)
	c.session = session
	session.ExecutionRouteConfigs = routes
	if err := c.bindExecutionRoutes(); err != nil || session.AgentExecutionRoutes["runtime-engineer"] != "coding" {
		t.Fatalf("route not inherited: %v %+v", err, session.AgentExecutionRoutes)
	}
	// Validation must still reject nonexistent contracts or unreachable catalog workers.
	for _, bad := range []string{
		strings.Replace(manifest+workers, "agent: runtime-engineer", "agent: ghost", 1),
		strings.Replace(manifest+workers, "allowed-workers: [runtime-engineer, network-engineer, critic]", "allowed-workers: [network-engineer, critic]", 1),
		strings.Replace(manifest+workers, "    agent: runtime-engineer\n    side-effect:", "    agent: ghost\n    side-effect:", 1),
	} {
		writeLintTestFile(t, filepath.Join(dir, "team.yaml"), bad)
		if _, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry); err == nil {
			t.Fatal("invalid downstream binding accepted")
		}
	}
}

func TestTeamInlineProviderAndPolicyParity(t *testing.T) {
	c, _, capture := newWorkerProviderSurfaceCoordinator(t, "inline-provider", "direct")
	runtimeWorkspace := c.session.Workspace
	manifest := "name: inline-team\nmodel: inline-provider\nmax-tokens: '128'\nmax-retries: 0\ntimeout: 10\n"
	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	writeLintTestFile(t, filepath.Join(dirs[0], "team.yaml"), manifest)
	writeAgentFile(t, dirs[0], "worker.md", "description: worker\npreset: readonly", "Perform the worker role for this team.")
	writeAgentFile(t, dirs[0], "observer.md", "description: observer\npreset: readonly", "Perform the observer role for this team.")
	writeLintTestFile(t, filepath.Join(dirs[1], "team.yaml"), manifest+"agents: {worker: readonly, observer: readonly}\n")
	writeLintTestFile(t, filepath.Join(dirs[2], "team.yaml"), manifest+"agents: {observer: readonly, worker: readonly}\n")
	var baseline *ExecutionPolicySnapshot
	var decisions []bool
	for _, dir := range dirs {
		spec, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
		if err != nil {
			t.Fatal(err)
		}
		c.session = spec.RuntimeSession()
		c.session.Workspace = runtimeWorkspace
		state, err := newExecutionPolicyState(c)
		if err != nil {
			t.Fatal(err)
		}
		if baseline == nil {
			baseline = state.snapshot
		} else if baseline.ConfigurationHash != state.snapshot.ConfigurationHash {
			t.Fatal("format changed execution policy hash")
		}
		original := *c.session.Agents["worker"]
		var current []bool
		for _, change := range []func(*agent.AgentDef){
			func(*agent.AgentDef) {},
			func(def *agent.AgentDef) { def.Tools = "view" },
			func(def *agent.AgentDef) { def.Generation.Model = "inline-other-model" },
			func(def *agent.AgentDef) { def.NoNet = true },
		} {
			*c.session.Agents["worker"] = original
			change(c.session.Agents["worker"])
			matches, err := c.executionPolicySnapshotMatchesCurrent(baseline)
			if err != nil {
				t.Fatal(err)
			}
			current = append(current, matches)
		}
		if !current[0] || current[2] {
			t.Fatalf("unchanged configuration or model drift admitted incorrectly: %v", current)
		}
		if decisions != nil && !slices.Equal(decisions, current) {
			t.Fatalf("format changed drift decisions: Markdown=%v inline=%v", decisions, current)
		}
		decisions = current
		*c.session.Agents["worker"] = original
	}
	if _, err := c.RunDirectAgent(t.Context(), "worker", "complete inline fixture"); err != nil {
		t.Fatal(err)
	}
	assertWorkerProviderRequiredContext(t, capture, "complete inline fixture")
	if !capture.submitted {
		t.Fatal("inline worker did not submit a typed result")
	}
}

func TestTeamInlinePromptAndSkillLint(t *testing.T) {
	for _, scalar := range []string{
		"Use `not_registered` tool. Use `missing_skill` skill.",
		"|\n      Use `not_registered` tool.\n      Use `missing_skill` skill.",
		">\n      Use `not_registered` tool.\n      Use `missing_skill` skill.",
		`"Use \u0060not_registered\u0060 tool.\nUse \u0060missing_skill\u0060 skill."`,
		"'{@ .directive @}'",
	} {
		for _, versioned := range []bool{false, true} {
			dir := t.TempDir()
			body := "agents:\n  worker:\n    preset: readonly\n    skills: [missing_required_skill]\n    instructions: " + scalar + "\n"
			writeLintTestFile(t, filepath.Join(dir, "team.yaml"), inlineManifestFixture(body, versioned))
			result, err := LintTeam(dir, map[string]string{"directive": "Use `not_registered` tool. Use `missing_skill` skill."}, nil, DefaultProviderRegistry)
			if err != nil || !result.Complete {
				t.Fatalf("lint=%+v err=%v", result, err)
			}
			for _, code := range []string{FindingPromptUnknownTool, FindingPromptUnknownSkill, FindingRequiredSkillMissing} {
				index := slices.IndexFunc(result.Findings, func(f TeamLintFinding) bool { return f.Code == code })
				if index < 0 {
					t.Fatalf("missing %s for %s: %+v", code, scalar, result)
				}
				f := result.Findings[index]
				line, prefix := 5, ""
				if code == FindingRequiredSkillMissing {
					line = 4
				}
				if versioned {
					line += 4
					prefix = "spec."
				}
				if f.File != "team.yaml" || f.Line != line || f.Agent != "worker" {
					t.Fatalf("incorrect source: %+v", f)
				}
				if code != FindingRequiredSkillMissing && f.FieldPath != prefix+"agents.worker.instructions" {
					t.Fatalf("incorrect prompt field: %+v", f)
				}
				if code == FindingRequiredSkillMissing && f.FieldPath != prefix+"agents.worker.skills[0]" {
					t.Fatalf("incorrect skill field: %+v", f)
				}
			}
		}
	}
}

func TestTeamInlineAliasPromptSource(t *testing.T) {
	for _, workers := range []string{
		"  worker:\n    preset: readonly\n    instructions: *prompt\n",
		"  worker:\n    <<: *definition\n",
	} {
		dir := t.TempDir()
		manifest := "vars:\n  prompt: &prompt 'Use `not_registered` tool.'\n  definition: &definition\n    preset: readonly\n    instructions: *prompt\nagents:\n" + workers
		writeLintTestFile(t, filepath.Join(dir, "team.yaml"), manifest)
		result, err := LintTeam(dir, nil, nil, DefaultProviderRegistry)
		if err != nil {
			t.Fatal(err)
		}
		index := slices.IndexFunc(result.Findings, func(f TeamLintFinding) bool { return f.Code == FindingPromptUnknownTool })
		if index < 0 || result.Findings[index].FieldPath != "agents.worker.instructions" || result.Findings[index].Line == 0 {
			t.Fatalf("alias prompt was not located: %+v", result)
		}
	}
}

func TestTeamInlineDecodeErrorSource(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		dir := t.TempDir()
		writeLintTestFile(t, filepath.Join(dir, "team.yaml"), inlineManifestFixture("agents:\n  worker:\n    preset: readonly\n    no-net: 'true'\n", versioned))
		_, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
		path, line := "agents.worker.no-net", "line 4"
		if versioned {
			path, line = "spec."+path, "line 8"
		}
		if err == nil || !strings.Contains(err.Error(), "team.yaml") || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), line) {
			t.Fatalf("missing precise source: %v", err)
		}
	}
}

func TestTeamInlineMergePrecedence(t *testing.T) {
	dir := t.TempDir()
	manifest := "vars:\n  first: &first {preset: readonly, model: ollama/first}\n  second: &second {preset: coding, model: ollama/second}\nagents:\n  merged: {<<: [*first, *second]}\n  explicit: {<<: [*first, *second], model: ollama/explicit}\n"
	writeLintTestFile(t, filepath.Join(dir, "team.yaml"), manifest)
	spec, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	for id, model := range map[string]string{"merged": "ollama/first", "explicit": "ollama/explicit"} {
		worker := spec.RuntimeSession().Agents[id]
		if worker.Generation.Model != model || slices.Contains(strings.Split(worker.Tools, ","), "bash") {
			t.Fatalf("merge changed yaml.v3 precedence for %s: %+v", id, worker)
		}
	}
}

func TestTeamInlineCapabilityRouting(t *testing.T) {
	dir := t.TempDir()
	manifest := `agents: {novice: readonly, expert: review, excluded: review}
delegation:
  allowed-workers: [novice, expert]
  capability-routing:
    - contract-id: security-audit
      required-capability: security-review
capability-registry:
  expert:
    - capability: security-review
      confidence: 0.7
  excluded:
    - capability: security-review
      confidence: 1.0
`
	writeLintTestFile(t, filepath.Join(dir, "team.yaml"), manifest)
	spec, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	session := spec.RuntimeSession()
	c := newTestCoordinatorForDryRun(t, session.Agents, nil, session.Config)
	candidates, err := c.ResolveCapabilityCandidates(t.Context(), CapabilityQuery{Required: []string{"security-review"}})
	if err != nil || len(candidates) != 2 || candidates[0].AgentID != "expert" || candidates[0].Score <= 0 {
		t.Fatalf("inline routing=%+v err=%v", candidates, err)
	}
	if err := c.validateCapabilityRouting([]TaskDef{{Agent: "novice", ContractID: "security-audit"}}); err == nil {
		t.Fatal("unqualified inline worker accepted")
	}
	if err := c.validateCapabilityRouting([]TaskDef{{Agent: "expert", ContractID: "security-audit"}}); err != nil {
		t.Fatalf("qualified inline worker rejected: %v", err)
	}
}

func TestTeamInlineMigration(t *testing.T) {
	for _, agents := range []string{
		"agents: {}\n",
		"agents:\n  shorthand: '{@ .selected @}'\n  worker:\n    preset: readonly\n    instructions: '{@ .message @}'\n    no-net: false\n    max-retries: 0\n    tools: []\n    skills: []\n    allowed-paths: []\n",
		"agents:\n  first: &worker {preset: readonly, instructions: '{@ .message @}'}\n  second: {<<: *worker, model: ''}\n",
	} {
		dir := t.TempDir()
		manifest := "name: inline-team\nvars: {selected: readonly, message: inspect}\n" + agents
		writeLintTestFile(t, filepath.Join(dir, "team.yaml"), manifest)
		before, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
		if err != nil {
			t.Fatal(err)
		}
		out, _, err := MigrateTeamManifestToV1Alpha1(dir)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(filepath.Join(dir, "team.yaml"))
		if string(raw) != manifest || strings.Contains(string(out), "*worker") {
			t.Fatalf("migration wrote input or left dangling alias: %s", out)
		}
		if !strings.Contains(string(out), "agents:") || (strings.Contains(agents, "{@ .message @}") && !strings.Contains(string(out), "{@ .message @}")) {
			t.Fatalf("migration dropped authored data: %s", out)
		}
		if strings.Contains(agents, "max-retries") {
			for _, literal := range []string{"no-net: false", "max-retries: 0", "tools: []", "skills: []", "allowed-paths: []", "shorthand: '{@ .selected @}'"} {
				if !strings.Contains(string(out), literal) {
					t.Fatalf("lost %s: %s", literal, out)
				}
			}
		}
		writeLintTestFile(t, filepath.Join(dir, "team.yaml"), string(out))
		after, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
		if err != nil || !reflect.DeepEqual(before.RuntimeSession().Agents, after.RuntimeSession().Agents) {
			t.Fatalf("round trip changed workers: %v", err)
		}
		var document yaml.Node
		if err := yaml.Unmarshal(out, &document); err != nil {
			t.Fatal(err)
		}
	}
}
