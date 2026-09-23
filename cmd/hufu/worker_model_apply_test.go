package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	hulog "github.com/kjelly/hufu/internal/log"
	"github.com/kjelly/hufu/internal/team"
)

// newWorkerModelTestSession builds a loaded-team fixture keyed like the team
// loader: one coordinator plus workers whose frontmatter model is md-<name>.
func newWorkerModelTestSession(workers ...string) *team.TeamSession {
	session := &team.TeamSession{
		Config: agent.TeamConfig{Name: "hufu-coding"},
		Agents: map[string]*agent.AgentDef{
			"coordinator": {Name: "coordinator", FileAlias: "coordinator", Role: "coordinator", Generation: agent.GenerationParams{Model: "coord-own"}},
		},
	}
	for _, name := range workers {
		session.Agents[name] = &agent.AgentDef{
			Name:       name,
			FileAlias:  name,
			Role:       "worker",
			System:     "prompt for " + name,
			Tools:      "view,grep",
			Generation: agent.GenerationParams{Model: "md-" + name},
		}
	}
	return session
}

func agentModels(session *team.TeamSession) map[string]string {
	models := make(map[string]string, len(session.Agents))
	for key, def := range session.Agents {
		models[key] = def.Generation.Model
	}
	return models
}

func assertAgentModels(t *testing.T, session *team.TeamSession, want map[string]string) {
	t.Helper()
	got := agentModels(session)
	for key, model := range want {
		if got[key] != model {
			t.Errorf("agent %s model = %q, want %q (all: %v)", key, got[key], model, got)
		}
	}
}

func TestApplyWorkerModelOverridesPrecedence(t *testing.T) {
	tests := []struct {
		name      string
		overrides ModelCLIOverrides
		want      map[string]string
	}{
		{
			name:      "no override keeps frontmatter models",
			overrides: ModelCLIOverrides{},
			want:      map[string]string{"coder": "md-coder", "reviewer": "md-reviewer", "coordinator": "coord-own"},
		},
		{
			name:      "global model sets every worker",
			overrides: ModelCLIOverrides{Model: "A"},
			want:      map[string]string{"coder": "A", "reviewer": "A", "coordinator": "coord-own"},
		},
		{
			name:      "worker model sets only that worker",
			overrides: ModelCLIOverrides{WorkerModels: []WorkerModelOverride{{Agent: "coder", Target: "B"}}},
			want:      map[string]string{"coder": "B", "reviewer": "md-reviewer", "coordinator": "coord-own"},
		},
		{
			name:      "worker model beats global model",
			overrides: ModelCLIOverrides{Model: "A", WorkerModels: []WorkerModelOverride{{Agent: "coder", Target: "B"}}},
			want:      map[string]string{"coder": "B", "reviewer": "A", "coordinator": "coord-own"},
		},
		{
			name:      "qualified selector is stored verbatim for canonical parsing",
			overrides: ModelCLIOverrides{WorkerModels: []WorkerModelOverride{{Agent: "coder", Target: "codex/gpt-6-sol"}}},
			want:      map[string]string{"coder": "codex/gpt-6-sol", "reviewer": "md-reviewer"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := newWorkerModelTestSession("coder", "reviewer")
			if err := applyCLIGenerationOverridesToAgents(session, tt.overrides); err != nil {
				t.Fatal(err)
			}
			assertAgentModels(t, session, tt.want)
			for key, def := range session.Agents {
				if key == "coordinator" {
					continue
				}
				if def.System != "prompt for "+key || def.Role != "worker" || def.Tools != "view,grep" {
					t.Errorf("agent %s prompt/role/tools changed: %+v", key, def)
				}
			}
		})
	}
}

// TestWorkerModelProfileAndCLIEndToEnd drives the real profile merge, the
// runtime option snapshot, and agent application together.
func TestWorkerModelProfileAndCLIEndToEnd(t *testing.T) {
	tests := []struct {
		name    string
		profile string
		args    []string
		want    map[string]string
	}{
		{
			name:    "profile global plus profile worker",
			profile: "model: A\n    worker-model: \"coder=B\"",
			want:    map[string]string{"coder": "B", "reviewer": "A", "sa": "A"},
		},
		{
			name:    "explicit CLI global beats profile worker",
			profile: "worker-model: \"coder=B\"",
			args:    []string{"--model", "A"},
			want:    map[string]string{"coder": "A", "reviewer": "A", "sa": "A"},
		},
		{
			name:    "explicit CLI worker beats profile global",
			profile: "model: A",
			args:    []string{"--worker-model", "coder=B"},
			want:    map[string]string{"coder": "B", "reviewer": "A", "sa": "A"},
		},
		{
			name:    "CLI and profile worker entries merge by agent",
			profile: "worker-model: \"sa=A,coder=A,reviewer=C\"",
			args:    []string{"--worker-model", "coder=B"},
			want:    map[string]string{"sa": "A", "coder": "B", "reviewer": "C"},
		},
		{
			name:    "explicit CLI worker beats explicit CLI global",
			profile: "worker-model: \"sa=P\"",
			args:    []string{"-m", "G", "--worker-model", "coder=B"},
			want:    map[string]string{"sa": "G", "coder": "B", "reviewer": "G"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeHufuYAML(t, dir, "profiles:\n  p:\n    "+tt.profile+"\n")
			defer chdir(t, dir)()
			previous := opts
			t.Cleanup(func() { opts = previous })
			opts = runOptions{profileName: "p"}

			cmd := newWorkerModelProfileTestCmd()
			if err := cmd.ParseFlags(tt.args); err != nil {
				t.Fatal(err)
			}
			if err := applyProfile(cmd); err != nil {
				t.Fatal(err)
			}
			overrides, err := currentModelOverrides()
			if err != nil {
				t.Fatal(err)
			}
			session := newWorkerModelTestSession("sa", "coder", "reviewer")
			applyCLIModelOverrides(&session.Config, overrides)
			if err := applyCLIGenerationOverridesToAgents(session, overrides); err != nil {
				t.Fatal(err)
			}
			assertAgentModels(t, session, tt.want)
			assertAgentModels(t, session, map[string]string{"coordinator": "coord-own"})
		})
	}
}

func TestApplyWorkerModelOverridesValidation(t *testing.T) {
	tests := []struct {
		name    string
		session func() *team.TeamSession
		entries []WorkerModelOverride
		want    map[string]string
		wantErr []string
	}{
		{
			name:    "unknown worker fails and lists available workers",
			session: func() *team.TeamSession { return newWorkerModelTestSession("sa", "coder", "reviewer") },
			entries: []WorkerModelOverride{{Agent: "coder", Target: "B"}, {Agent: "codre", Target: "B"}},
			wantErr: []string{`unknown worker "codre" in --worker-model for team "hufu-coding"`, "available workers: coder, reviewer, sa"},
		},
		{
			name:    "coordinator by name fails",
			session: func() *team.TeamSession { return newWorkerModelTestSession("coder") },
			entries: []WorkerModelOverride{{Agent: "coordinator", Target: "B"}},
			wantErr: []string{`targets coordinator "coordinator"`, "use --coordinator-model instead"},
		},
		{
			name: "coordinator by name fails even without a coordinator agent",
			session: func() *team.TeamSession {
				session := newWorkerModelTestSession("coder")
				delete(session.Agents, "coordinator")
				return session
			},
			entries: []WorkerModelOverride{{Agent: "Coordinator", Target: "B"}},
			wantErr: []string{"use --coordinator-model instead"},
		},
		{
			name: "coordinator by role fails",
			session: func() *team.TeamSession {
				session := newWorkerModelTestSession("coder")
				session.Agents["lead"] = &agent.AgentDef{Name: "lead", FileAlias: "lead", Role: "coordinator"}
				return session
			},
			entries: []WorkerModelOverride{{Agent: "lead", Target: "B"}},
			wantErr: []string{`targets coordinator "lead"`, "use --coordinator-model instead"},
		},
		{
			name: "orchestrator role fails",
			session: func() *team.TeamSession {
				session := newWorkerModelTestSession("coder")
				session.Agents["planner"] = &agent.AgentDef{Name: "planner", FileAlias: "planner", Role: "orchestrator"}
				return session
			},
			entries: []WorkerModelOverride{{Agent: "planner", Target: "B"}},
			wantErr: []string{`targets coordinator "planner"`, "use --coordinator-model instead"},
		},
		{
			name: "case-insensitive match targets canonical worker",
			session: func() *team.TeamSession {
				session := newWorkerModelTestSession()
				session.Agents["helper"] = &agent.AgentDef{Name: "Helper", FileAlias: "helper", Role: "worker"}
				return session
			},
			entries: []WorkerModelOverride{{Agent: "HELPER", Target: "A"}},
			want:    map[string]string{"helper": "A"},
		},
		{
			name: "ambiguous case collision fails closed",
			session: func() *team.TeamSession {
				session := newWorkerModelTestSession()
				session.Agents["Helper"] = &agent.AgentDef{Name: "Helper", Role: "worker"}
				session.Agents["helper"] = &agent.AgentDef{Name: "helper", Role: "worker"}
				return session
			},
			entries: []WorkerModelOverride{{Agent: "helper", Target: "A"}},
			wantErr: []string{`worker "helper" in --worker-model is ambiguous`, "matches Helper, helper"},
		},
		{
			name: "file alias identifies the worker",
			session: func() *team.TeamSession {
				session := newWorkerModelTestSession()
				def := &agent.AgentDef{Name: "solution-architect", FileAlias: "sa", Role: "worker"}
				session.Agents["sa"] = def
				session.Agents["solution-architect"] = def
				return session
			},
			entries: []WorkerModelOverride{{Agent: "sa", Target: "A"}},
			want:    map[string]string{"sa": "A", "solution-architect": "A"},
		},
		{
			name: "two identities of one worker conflict",
			session: func() *team.TeamSession {
				session := newWorkerModelTestSession()
				def := &agent.AgentDef{Name: "solution-architect", FileAlias: "sa", Role: "worker"}
				session.Agents["sa"] = def
				session.Agents["solution-architect"] = def
				return session
			},
			entries: []WorkerModelOverride{{Agent: "sa", Target: "A"}, {Agent: "solution-architect", Target: "B"}},
			wantErr: []string{`entries "sa" and "solution-architect" both target worker "solution-architect"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := tt.session()
			before := agentModels(session)
			err := applyCLIGenerationOverridesToAgents(session, ModelCLIOverrides{Model: "G", WorkerModels: tt.entries})
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("applyCLIGenerationOverridesToAgents() succeeded, want error containing %q", tt.wantErr)
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want containing %q", err, want)
					}
				}
				// Validation fails before any agent is modified, including by
				// the global --model value.
				assertAgentModels(t, session, before)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertAgentModels(t, session, tt.want)
		})
	}
}

func TestCurrentModelOverridesRejectsMalformedWorkerModel(t *testing.T) {
	previous := opts
	t.Cleanup(func() { opts = previous })
	opts = runOptions{workerModelOverrides: []string{"coder"}}
	if _, err := currentModelOverrides(); err == nil || !strings.Contains(err.Error(), `invalid --worker-model entry "coder"`) {
		t.Fatalf("currentModelOverrides() error = %v, want malformed entry", err)
	}
	opts = runOptions{workerModelOverrides: []string{"coder=A", "coder=B"}}
	overrides, err := currentModelOverrides()
	if err != nil {
		t.Fatal(err)
	}
	if len(overrides.WorkerModels) != 1 || overrides.WorkerModels[0] != (WorkerModelOverride{Agent: "coder", Target: "B"}) {
		t.Fatalf("WorkerModels = %#v, want last coder entry", overrides.WorkerModels)
	}
}

func TestDisplayTeamHeaderShowsWorkerModelsOnlyWhenPresent(t *testing.T) {
	previous := opts
	opts = runOptions{}
	syncLogState()
	var output bytes.Buffer
	hulog.SetWriter(&output)
	t.Cleanup(func() {
		opts = previous
		hulog.SetWriter(nil)
		syncLogState()
	})

	session := newWorkerModelTestSession("coder", "reviewer")
	displayTeamHeader(session, nil)
	if strings.Contains(output.String(), "Worker models") {
		t.Fatalf("header without overrides printed worker models: %q", output.String())
	}

	output.Reset()
	displayTeamHeader(session, []WorkerModelOverride{{Agent: "CODER", Target: "codex/gpt-6-sol"}, {Agent: "reviewer", Target: "codex/gpt-6-sol"}})
	if !strings.Contains(output.String(), "Worker models:") || !strings.Contains(output.String(), "coder=codex/gpt-6-sol, reviewer=codex/gpt-6-sol") {
		t.Fatalf("header = %q, want canonical worker model overrides", output.String())
	}
}

// writeWorkerModelPreviewTeam writes a two-worker team whose workers inherit
// the team worker-model and returns its registry.
func writeWorkerModelPreviewTeam(t *testing.T) *team.TeamRegistry {
	t.Helper()
	root := t.TempDir()
	teamDir := filepath.Join(root, "preview")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"team.yaml":   "name: preview\nworker-model: ollama/fixture-model\ncoordinator-model: ollama/fixture-model\n",
		"analyst.md":  "---\nname: analyst\nrole: worker\n---\nPreview tasks.\n",
		"coder.md":    "---\nname: coder\nrole: worker\n---\nWrite code.\n",
		"reviewer.md": "---\nname: reviewer\nrole: worker\nmodel: ollama/reviewer-own\n---\nReview code.\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(teamDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	registry := team.NewTeamRegistry([]string{root})
	if err := registry.Discover(); err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestLoadTeamDryRunAppliesWorkerModelOverrides(t *testing.T) {
	originalOpts := opts
	t.Cleanup(func() { opts = originalOpts })
	registry := writeWorkerModelPreviewTeam(t)
	workspace := filepath.Join(t.TempDir(), "must-not-exist")
	opts = runOptions{
		dryRun: true, canonicalRun: true, workspace: workspace, workspaceMode: "exact",
		workerModelOverrides: []string{"Coder=ollama/override-model", "reviewer=codex/gpt-6-sol"},
	}

	tc, err := loadTeamByName(t.Context(), "preview", registry, "", "", nil, nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tc.coordinator.Close() })
	result, err := tc.coordinator.DryRun(t.Context(), "preview")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"analyst":  "ollama/fixture-model",
		"coder":    "ollama/override-model",
		"reviewer": "codex/gpt-6-sol",
	}
	got := map[string]string{}
	for _, info := range result.Agents {
		got[info.Name] = info.ExecutionTarget
	}
	for name, target := range want {
		if got[name] != target {
			t.Errorf("dry-run %s target = %q, want %q (all: %v)", name, got[name], target, got)
		}
	}
	if result.CoordinatorTarget != "ollama/fixture-model" {
		t.Errorf("coordinator target = %q, want unchanged team coordinator-model", result.CoordinatorTarget)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("dry-run created workspace %q: %v", workspace, err)
	}
}

func TestLoadTeamDryRunRejectsInvalidWorkerModelBeforeWorkspace(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		wantErr string
	}{
		{name: "unknown worker", entries: []string{"codre=ollama/x"}, wantErr: `unknown worker "codre" in --worker-model for team "preview"; available workers: analyst, coder, Helper, reviewer`},
		{name: "coordinator", entries: []string{"coordinator=ollama/x"}, wantErr: "use --coordinator-model instead"},
		{name: "malformed", entries: []string{"coder"}, wantErr: `invalid --worker-model entry "coder"`},
		{name: "invalid selector", entries: []string{"coder=nope/x"}, wantErr: `unknown execution backend "nope" for agent coder target "nope/x"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalOpts := opts
			t.Cleanup(func() { opts = originalOpts })
			registry := writeWorkerModelPreviewTeam(t)
			workspace := filepath.Join(t.TempDir(), "must-not-exist")
			opts = runOptions{dryRun: true, canonicalRun: true, workspace: workspace, workspaceMode: "exact", workerModelOverrides: tt.entries}

			tc, err := loadTeamByName(t.Context(), "preview", registry, "", "", nil, nil, nil, false, false)
			if err == nil {
				_ = tc.coordinator.Close()
				t.Fatalf("loadTeamByName() succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("loadTeamByName() error = %q, want containing %q", err, tt.wantErr)
			}
			if _, statErr := os.Stat(workspace); !os.IsNotExist(statErr) {
				t.Fatalf("rejected override created workspace %q: %v", workspace, statErr)
			}
		})
	}
}
