package team

import (
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func launchSettingsCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	c := newTestCoordinator(t)
	manager, err := agent.NewProviderManager("http://127.0.0.1:1/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.providerManager = manager
	c.maxConcurrent = 2
	c.session.LaunchOptions = &LaunchOptions{ExecutionProfile: "default", NoNet: true, ForcedSkills: []string{"fixture"}}
	c.session.Config.Generation = agent.GenerationParams{Temperature: "0.2", MaxTokens: "4096", ContextWindow: 131072}
	c.session.Config.WorkerModel, c.session.Config.CoordinatorModel = "ollama/test", "ollama/test"
	c.session.Agents["worker"].Generation = agent.GenerationParams{Model: "ollama/test", MaxTokens: "2048"}
	return c
}

func launchPolicy(t *testing.T, c *Coordinator) *ExecutionPolicySnapshot {
	t.Helper()
	state, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	c.executionPolicy = state
	return c.ExecutionPolicySnapshot()
}

func TestRestoreLaunchSettings(t *testing.T) {
	for _, name := range []string{"restored", "legacy", "tampered", "missing_agent", "added_agent"} {
		t.Run(name, func(t *testing.T) {
			original := launchSettingsCoordinator(t)
			original.session.Config.ProviderAPIKey = "private-fixture-provider-key"
			policy := launchPolicy(t, original)
			body, err := json.Marshal(policy)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "private-fixture") || strings.Contains(string(body), "127.0.0.1") {
				t.Fatal("launch snapshot persisted provider credentials or endpoint")
			}
			current := launchSettingsCoordinator(t)
			current.session.LaunchOptions = nil
			current.session.Config.Generation = agent.GenerationParams{Temperature: "0.9", MaxTokens: "8192"}
			current.session.Config.Timeout = 999
			current.session.Agents["worker"].Generation.Model = "ollama/changed"
			before := current.session.Config
			switch name {
			case "legacy":
				policy.Launch = nil
				policy.ConfigurationHash, err = executionPolicyConfigurationHash(policy)
				if err != nil {
					t.Fatal(err)
				}
			case "tampered":
				policy.Launch.Timeout++
			case "missing_agent":
				delete(current.session.Agents, "worker")
			case "added_agent":
				current.session.Agents["other"] = &agent.AgentDef{Name: "other"}
			}
			err = RestoreLaunchSettings(current.session, policy)
			switch name {
			case "tampered", "missing_agent", "added_agent":
				if err == nil || !reflect.DeepEqual(before, current.session.Config) {
					t.Fatalf("invalid restoration mutated config: %v", err)
				}
			case "legacy":
				if err != nil || !reflect.DeepEqual(before, current.session.Config) {
					t.Fatalf("legacy values were guessed: %v", err)
				}
			case "restored":
				if err != nil {
					t.Fatal(err)
				}
				after := launchPolicy(t, current)
				if after.ConfigurationHash != policy.ConfigurationHash || !reflect.DeepEqual(after.Launch, policy.Launch) {
					t.Fatalf("restoration did not reproduce admitted settings: %s", executionPolicyDifferences(policy, after))
				}
				current.session.Agents["worker"].Generation.MaxTokens = "1024"
				if matches, err := current.executionPolicySnapshotMatchesCurrent(policy); err != nil || matches {
					t.Fatalf("explicit override bypassed drift checks: %v", err)
				}
			}
		})
	}
}

func TestLaunchSettingsSQLiteRestore(t *testing.T) {
	const workspaceEnv = "HUFU_TEST_LAUNCH_WORKSPACE"
	if workspace := os.Getenv(workspaceEnv); workspace != "" {
		store, err := OpenEventStoreReadOnly(workspace)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = store.Close() }()
		events, err := store.ReadEvents()
		if err != nil {
			t.Fatal(err)
		}
		policy, err := executionPolicySnapshotFromEvents(events)
		if err != nil || policy == nil || policy.Launch == nil {
			t.Fatalf("missing durable startup values: %v", err)
		}
		c := launchSettingsCoordinator(t)
		c.session.Config.Generation.MaxTokens = "9999"
		if err := RestoreLaunchSettings(c.session, policy); err != nil {
			t.Fatal(err)
		}
		if launchPolicy(t, c).ConfigurationHash != policy.ConfigurationHash {
			t.Fatal("new process could not reconstruct policy without original flags")
		}
		return
	}
	c := launchSettingsCoordinator(t)
	policy := launchPolicy(t, c)
	if err := c.FreezeExecutionPolicyAtStartup(); err != nil {
		t.Fatal(err)
	}
	if c.eventStore != nil {
		t.Fatal("startup did not release the setup journal")
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLaunchSettingsSQLiteRestore$")
	cmd.Env = append(os.Environ(), workspaceEnv+"="+c.session.Workspace)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fresh process restoration failed: %v\n%s", err, output)
	}
	// Defensive copies cannot rewrite the stored startup or nested agent map.
	policy.Launch.Options.ForcedSkills[0] = "mutated"
	a := policy.Launch.Agents["worker"]
	a.Timeout++
	policy.Launch.Agents["worker"] = a
	if reflect.DeepEqual(policy.Launch, c.ExecutionPolicySnapshot().Launch) {
		t.Fatal("snapshot copy aliases mutable launch settings")
	}
}

func TestLaunchSettingsLegacyPolicyComparison(t *testing.T) {
	c := launchSettingsCoordinator(t)
	c.session.LaunchOptions = nil
	legacy := launchPolicy(t, c)
	c.session.LaunchOptions = &LaunchOptions{ExecutionProfile: "default"}
	current := launchPolicy(t, c)
	if ok, err := c.executionPolicySnapshotMatchesCurrent(legacy); err != nil || !ok {
		t.Fatalf("optional launch capture invalidated an unchanged old policy: %v", err)
	}
	if ok, err := compatibleUnusedExecutionEnvironment(legacy, current); err != nil || !ok {
		t.Fatalf("legacy result collection no longer compatible: %v", err)
	}
	c.session.Config.Timeout++
	if ok, err := c.executionPolicySnapshotMatchesCurrent(legacy); err != nil || ok {
		t.Fatalf("legacy generation guard was bypassed: %v", err)
	}
}

func TestLaunchSettingsSelectsBranchWithoutCheckpointFallback(t *testing.T) {
	workspace, _, store := branchSwitchFixture(t)
	c := launchSettingsCoordinator(t)
	for branch, limit := range map[string]string{"main": "2048", "sibling": "8192"} {
		c.session.Config.Generation.MaxTokens = limit
		policy := launchPolicy(t, c)
		body, err := json.Marshal(policy)
		if err != nil {
			t.Fatal(err)
		}
		store.SetBranchID(branch)
		if _, err := store.AppendPersisted(RunEvent{Type: string(EventExecutionPolicySnapshot), Actor: "coordinator", Payload: body}); err != nil {
			t.Fatal(err)
		}
		item := &TodoItem{ID: "1", Agent: "worker", Status: TaskDone, Desc: "accepted"}
		body, err = json.Marshal(taskTransitionPayload(item))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Append(RunEvent{Type: string(EventTaskCompleted), TaskID: item.ID, Payload: body}); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshotRuntimeState(t, workspace)
	for branch, limit := range map[string]string{"main": "2048", "sibling": "8192"} {
		source, err := ReadResumeSource(workspace, branch, "")
		if err != nil {
			t.Fatal(err)
		}
		current := launchSettingsCoordinator(t)
		if err := RestoreLaunchSettings(current.session, source.Session.ExecutionPolicySnapshot); err != nil {
			t.Fatal(err)
		}
		if current.session.Config.Generation.MaxTokens != limit {
			t.Fatalf("branch %s restored output limit %q, want %q; policy=%+v", branch, current.session.Config.Generation.MaxTokens, limit, source.Session.ExecutionPolicySnapshot)
		}
		if !before.equal(snapshotRuntimeState(t, workspace)) {
			t.Fatal("launch restoration mutated runtime state")
		}
	}
}

func TestLaunchSettingsRejectInvalidGenerationBeforeStorage(t *testing.T) {
	c := launchSettingsCoordinator(t)
	c.session.Config.Generation.MaxTokens = "private-invalid-output-limit"
	if _, err := newExecutionPolicyState(c); err == nil || strings.Contains(err.Error(), "private-invalid") {
		t.Fatalf("invalid generation was admitted or disclosed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.session.Workspace, "runtime.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("invalid startup settings reached durable storage: %v", err)
	}
}
