package team

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team/preset"
)

func TestTeamCompositionPresetParity(t *testing.T) {
	for _, name := range preset.TeamNames() {
		for _, versioned := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/flat", true: "/versioned"}[versioned], func(t *testing.T) {
				compact, expanded := t.TempDir(), t.TempDir()
				manifest := "name: composition\nmodel: ollama/composition-model\npreset: " + strings.ToUpper(name) + "\n"
				expandedManifest := "name: composition\nmodel: ollama/composition-model\n"
				if versioned {
					manifest = "apiVersion: hufu.io/v1alpha1\nkind: AgentTeam\nmetadata:\n  name: composition\nspec:\n  model: ollama/composition-model\n  preset: " + name + "\n"
				}
				writeLintTestFile(t, filepath.Join(compact, "team.yaml"), manifest)
				writeLintTestFile(t, filepath.Join(expanded, "team.yaml"), expandedManifest)
				declaration, _ := preset.LookupTeam(name)
				for file, body := range declaration.Render() {
					writeLintTestFile(t, filepath.Join(expanded, file), body)
				}
				before, err := CompileTeam(expanded, nil, nil, DefaultProviderRegistry)
				if err != nil {
					t.Fatal(err)
				}
				after, err := CompileTeam(compact, nil, nil, DefaultProviderRegistry)
				if err != nil {
					t.Fatal(err)
				}
				// Compare every AgentDef field, not only the partial compatibility snapshot.
				if !reflect.DeepEqual(before.RuntimeSession().Agents, after.RuntimeSession().Agents) {
					t.Fatalf("AgentDefs changed: before=%#v after=%#v", before.RuntimeSession().Agents, after.RuntimeSession().Agents)
				}
				a, err := BuildCompatSnapshot(before.RuntimeSession())
				if err != nil {
					t.Fatal(err)
				}
				b, err := BuildCompatSnapshot(after.RuntimeSession())
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(a, b) {
					t.Fatal("compatibility snapshot changed")
				}
				if !reflect.DeepEqual(before.RuntimeSession().Config.Vars, after.RuntimeSession().Config.Vars) {
					t.Fatal("roster variables changed")
				}
				aCoord := (&Coordinator{session: before.RuntimeSession()}).GetOrchestratorDef()
				bCoord := (&Coordinator{session: after.RuntimeSession()}).GetOrchestratorDef()
				if !reflect.DeepEqual(aCoord, bCoord) {
					t.Fatal("coordinator selection changed")
				}
				oldLint, err := LintTeam(expanded, nil, nil, DefaultProviderRegistry)
				if err != nil {
					t.Fatal(err)
				}
				inspection, err := InspectTeam(compact, nil, nil, DefaultProviderRegistry, TeamCompileLint)
				if err != nil || !inspection.Complete {
					t.Fatalf("inspection=%#v err=%v", inspection, err)
				}
				newLint, err := LintTeam(compact, nil, nil, DefaultProviderRegistry)
				if err != nil {
					t.Fatal(err)
				}
				oldCodes, newCodes := make([]string, 0), make([]string, 0)
				for _, f := range oldLint.Findings {
					oldCodes = append(oldCodes, f.Code+":"+f.Severity)
				}
				for _, f := range newLint.Findings {
					newCodes = append(newCodes, f.Code+":"+f.Severity)
				}
				slices.Sort(oldCodes)
				slices.Sort(newCodes)
				if !slices.Equal(oldCodes, newCodes) {
					t.Fatalf("lint changed: %v vs %v", oldCodes, newCodes)
				}
				for _, entry := range declaration.Agents {
					alias := agentFilenameBase(entry.FileName)
					if after.Agents[alias].Name.Source != SourcePreset || !strings.Contains(after.Agents[alias].Tools.Detail, "team-preset:"+name) {
						t.Fatalf("missing provenance: %#v", after.Agents[alias])
					}
					if source := inspection.Sources.Agents[alias]; source.File != "team.yaml" || source.Fields["tools"].Line == 0 {
						t.Fatalf("source=%#v", source)
					}
				}
				entries, err := os.ReadDir(compact)
				if err != nil || len(entries) != 1 {
					t.Fatalf("inspection wrote files: %v %v", entries, err)
				}
			})
		}
	}
}

func TestTeamCompositionInvalidPreset(t *testing.T) {
	for _, value := range []string{"null", "42", "false", "[]", "{}", "''", "'  '", "coding", "unknown-preset"} {
		t.Run(value, func(t *testing.T) {
			dir := t.TempDir()
			writeLintTestFile(t, filepath.Join(dir, "team.yaml"), "preset: "+value+"\n")
			if _, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry); err == nil {
				t.Fatal("invalid preset accepted")
			}
			result, err := LintTeam(dir, nil, nil, DefaultProviderRegistry)
			if value == "coding" || value == "unknown-preset" {
				if err != nil || result.Complete || len(result.Findings) != 1 || result.Findings[0].Code != FindingTeamPresetUnknown || result.Findings[0].File != "team.yaml" || result.Findings[0].Line != 1 {
					t.Fatalf("result=%#v err=%v", result, err)
				}
			} else if err == nil {
				t.Fatal("invalid type did not return load error")
			}
		})
	}
}

func TestTeamCompositionCollisionAndMixedSources(t *testing.T) {
	for _, test := range []struct {
		file, body string
		valid      bool
	}{
		{"reviewer.md", "---\npreset: review\n---\nReview.", false},
		{"REVIEWER.md", "---\npreset: review\n---\nReview.", false},
		{"custom.md", "---\nname: developer\npreset: review\n---\nReview.", false},
		{"security.md", "---\npreset: review\n---\nReview security.", true},
		{"coordinator.md", "Coordinate the team.", true},
	} {
		t.Run(test.file, func(t *testing.T) {
			dir := t.TempDir()
			writeLintTestFile(t, filepath.Join(dir, "team.yaml"), "preset: coding-reviewed\n")
			writeLintTestFile(t, filepath.Join(dir, test.file), test.body)
			spec, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
			if (err == nil) != test.valid {
				t.Fatalf("compile valid=%v err=%v", test.valid, err)
			}
			lint, lintErr := LintTeam(dir, nil, nil, DefaultProviderRegistry)
			if lintErr != nil {
				t.Fatal(lintErr)
			}
			if !test.valid {
				if lint.Complete || !slices.ContainsFunc(lint.Findings, func(f TeamLintFinding) bool {
					return f.Code == FindingDuplicateAgent && strings.Contains(f.Message, "team.yaml")
				}) {
					t.Fatalf("lint=%#v", lint)
				}
			} else if spec.RuntimeSession().Agents[agentFilenameBase(test.file)] == nil {
				t.Fatal("mixed member missing")
			}
		})
	}
}

func TestTeamCompositionDiscoveryMigrationAndBindings(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "dev")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "preset: '{@ .selected @}'\nvars:\n  selected: coding-reviewed\ndelegation:\n  allowed-workers: [developer, reviewer]\ncontracts:\n  - id: compose\n    agent: developer\n    goal: inspect\n"
	writeLintTestFile(t, filepath.Join(dir, "team.yml"), manifest)
	registry := NewTeamRegistry([]string{root})
	if err := registry.Discover(); err != nil || !registry.HasTeam("dev") {
		t.Fatalf("discovery: %v", err)
	}
	spec, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	session := spec.RuntimeSession()
	segments, err := SplitSegmentByAgents(PromptSegment{Type: SegmentSwitchTeam, Name: "dev", Content: "@developer inspect @reviewer review"}, registry, []*agent.AgentDef{session.Agents["developer"], session.Agents["reviewer"]})
	if err != nil || len(segments) != 3 || segments[1].Type != SegmentInvokeAgent || segments[1].Name != "developer" {
		t.Fatalf("segments=%#v err=%v", segments, err)
	}
	migrated, _, err := MigrateTeamManifestToV1Alpha1(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(migrated), "{@ .selected @}") {
		t.Fatal("migration baked in template")
	}
	if err := os.Remove(filepath.Join(dir, "team.yml")); err != nil {
		t.Fatal(err)
	}
	writeLintTestFile(t, filepath.Join(dir, "team.yaml"), string(migrated))
	roundTrip, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(session.Agents, roundTrip.RuntimeSession().Agents) {
		t.Fatal("migration changed AgentDefs")
	}
	if err := verifyContractMigration(session.ContractTasks, roundTrip.RuntimeSession().ContractTasks); err != nil {
		t.Fatal(err)
	}

}

func TestTeamCompositionProviderExecutionAndPolicyParity(t *testing.T) {
	c, _, capture := newWorkerProviderSurfaceCoordinator(t, "composition-provider", "direct")
	root := t.TempDir()
	compact, expanded := filepath.Join(root, "compact"), filepath.Join(root, "expanded")
	for _, dir := range []string{compact, expanded} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	manifest := "name: composition\nmodel: composition-provider\nmax-tokens: '128'\nmax-retries: 0\ntimeout: 10\n"
	writeLintTestFile(t, filepath.Join(compact, "team.yaml"), manifest+"preset: coding-single\n")
	writeLintTestFile(t, filepath.Join(expanded, "team.yaml"), manifest)
	declaration, _ := preset.LookupTeam("coding-single")
	for file, body := range declaration.Render() {
		writeLintTestFile(t, filepath.Join(expanded, file), body)
	}
	var fingerprint string
	runtimeWorkspace := c.session.Workspace
	for _, dir := range []string{expanded, compact} {
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
		if fingerprint != "" && fingerprint != state.snapshot.ConfigurationHash {
			t.Fatal("format changed policy hash")
		}
		fingerprint = state.snapshot.ConfigurationHash
	}
	if _, err := c.RunDirectAgent(t.Context(), "worker", "complete composition fixture"); err != nil {
		t.Fatal(err)
	}
	assertWorkerProviderRequiredContext(t, capture, "complete composition fixture")
	if !capture.submitted {
		t.Fatal("worker did not submit a typed result")
	}
}
