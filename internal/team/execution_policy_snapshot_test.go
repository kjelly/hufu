package team

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/execution"
)

func newExecutionPolicySnapshotCoordinator(t *testing.T, workspace string, maxConcurrent int, remoteLimit int) *Coordinator {
	return newExecutionPolicySnapshotCoordinatorWithBackendOptions(t, workspace, maxConcurrent, remoteLimit, "", "", defaultExecutionPolicyCodexConfig())
}

type executionPolicySnapshotCoordinatorOptions struct {
	defaultProviderURL    string
	defaultProviderAPIKey string
	codexConfig           agent.SubagentProviderConfig
	modelList             []config.ModelEntry
	noNet                 bool
	workerNoNet           bool
	workerExtraModels     []string
	codexWorkerNoNet      bool
}

func defaultExecutionPolicyCodexConfig() agent.SubagentProviderConfig {
	return agent.SubagentProviderConfig{
		Type:           codexAppServerProviderType,
		Command:        []string{"codex", "app-server"},
		Protocol:       "app-server-v2",
		ExecutionWorld: localExecutionWorldName,
		InheritEnv:     []string{"HOME", "CODEX_HOME"},
		MaxConcurrent:  7,
	}
}

func newExecutionPolicySnapshotCoordinatorWithBackendOptions(t *testing.T, workspace string, maxConcurrent int, remoteLimit int, defaultProviderURL, defaultProviderAPIKey string, codexConfig agent.SubagentProviderConfig) *Coordinator {
	return newExecutionPolicySnapshotCoordinatorWithOptions(t, workspace, maxConcurrent, remoteLimit, executionPolicySnapshotCoordinatorOptions{
		defaultProviderURL: defaultProviderURL, defaultProviderAPIKey: defaultProviderAPIKey, codexConfig: codexConfig,
	})
}

func newExecutionPolicySnapshotCoordinatorWithOptions(t *testing.T, workspace string, maxConcurrent int, remoteLimit int, options executionPolicySnapshotCoordinatorOptions) *Coordinator {
	t.Helper()
	session := &TeamSession{
		Dir:       workspace,
		Workspace: workspace,
		Config: agent.TeamConfig{
			Name:              "execution-policy",
			DefaultLLMBackend: "ollama",
			Generation:        agent.GenerationParams{Model: "remote/team-model"},
			WorkerModel:       "remote/worker-model",
			CoordinatorModel:  "remote/coordinator-model",
			GuardModel:        "remote/config-guard-model",
			Providers: map[string]config.ProviderConfig{
				"ollama": {MaxConcurrent: 2},
				"remote": {MaxConcurrent: remoteLimit},
			},
			SubagentProviders: map[string]agent.SubagentProviderConfig{
				codexSubagentProviderName: options.codexConfig,
			},
		},
		Agents: map[string]*agent.AgentDef{
			"worker": {
				Name:        "worker",
				Role:        "worker",
				Generation:  agent.GenerationParams{Model: "remote/agent-model"},
				ExtraModels: append([]string{"ollama/extra-model"}, options.workerExtraModels...),
				NoNet:       options.workerNoNet,
			},
			"codex-worker": {
				Name:             "codex-worker",
				Role:             "worker",
				Generation:       agent.GenerationParams{Model: "codex-bare-model"},
				SubagentProvider: codexSubagentProviderName,
				NoNet:            options.codexWorkerNoNet,
			},
		},
	}
	c, err := NewCoordinator(session, options.defaultProviderURL, options.defaultProviderAPIKey, nil, nil, options.modelList, RoleModels{
		Sidecar: "remote/sidecar-model", Guard: "remote/guard-model", Judge: "ollama/judge-model", PlanReviewer: "remote/plan-model",
	}, maxConcurrent, false, false, false, nil, nil, nil, false, "", options.noNet, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c.eventStore != nil {
			_ = c.eventStore.Close()
		}
		c.CloseContextPreflight()
	})
	return c
}

func newExecutionPolicySnapshotCoordinatorAtProjectRoot(t *testing.T, projectRoot, workspace string, maxConcurrent int, remoteLimit int, options executionPolicySnapshotCoordinatorOptions) *Coordinator {
	t.Helper()
	originalRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("get current project root: %v", err)
	}
	if err := os.Chdir(projectRoot); err != nil {
		t.Fatalf("enter project root %q: %v", projectRoot, err)
	}
	defer func() {
		if err := os.Chdir(originalRoot); err != nil {
			t.Errorf("restore project root %q: %v", originalRoot, err)
		}
	}()
	return newExecutionPolicySnapshotCoordinatorWithOptions(t, workspace, maxConcurrent, remoteLimit, options)
}

func snapshotBackend(t *testing.T, snapshot *ExecutionPolicySnapshot, name string) ExecutionBackendPolicySnapshot {
	t.Helper()
	for _, policy := range snapshot.Backends {
		if policy.Backend == name {
			return policy
		}
	}
	t.Fatalf("snapshot has no backend %q: %#v", name, snapshot.Backends)
	return ExecutionBackendPolicySnapshot{}
}

func snapshotRoute(t *testing.T, snapshot *ExecutionPolicySnapshot, model string) ExecutionModelRouteSnapshot {
	t.Helper()
	for _, route := range snapshot.ModelRoutes {
		if route.Model == model {
			return route
		}
	}
	t.Fatalf("snapshot has no route for %q: %#v", model, snapshot.ModelRoutes)
	return ExecutionModelRouteSnapshot{}
}

func TestExecutionPolicySnapshotFreezesPolicyBeforeTaskAdmission(t *testing.T) {
	workspace := t.TempDir()
	firstHome := t.TempDir()
	firstCodexHome := t.TempDir()
	t.Setenv("HOME", firstHome)
	t.Setenv("CODEX_HOME", firstCodexHome)

	c := newExecutionPolicySnapshotCoordinator(t, workspace, 4, 3)
	c.initEventStore()
	if err := c.checkRunAdmission(); err != nil {
		t.Fatalf("freeze execution policy: %v", err)
	}

	snapshot := c.ExecutionPolicySnapshot()
	if snapshot == nil || snapshot.ConfigurationHash == "" {
		t.Fatalf("admitted snapshot = %#v, want fingerprinted policy", snapshot)
	}
	rebuilt, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatalf("rebuild execution policy snapshot: %v", err)
	}
	if rebuilt.snapshot.ConfigurationHash != snapshot.ConfigurationHash {
		t.Fatalf("rebuilt snapshot fingerprint = %q, want deterministic %q", rebuilt.snapshot.ConfigurationHash, snapshot.ConfigurationHash)
	}
	if snapshot.TeamMaxConcurrent != 4 || snapshot.DefaultLLMBackend != execution.OllamaBackendName {
		t.Fatalf("team snapshot = %#v, want max=4 and default=%q", snapshot, execution.OllamaBackendName)
	}
	if remote := snapshotBackend(t, snapshot, "remote"); remote.MaxConcurrent != 3 || remote.ProviderKey == "" {
		t.Fatalf("remote backend snapshot = %#v, want max=3 and provider identity", remote)
	}
	if codex := snapshotBackend(t, snapshot, codexSubagentProviderName); codex.MaxConcurrent != codexDefaultMaxConcurrent {
		t.Fatalf("codex backend snapshot = %#v, want hard cap %d", codex, codexDefaultMaxConcurrent)
	}
	if route := snapshotRoute(t, snapshot, "remote/guard-model"); route.Backend != "remote" || route.ProviderKey == "" {
		t.Fatalf("guard route = %#v, want remote provider route", route)
	}
	if route := snapshotRoute(t, snapshot, "remote/config-guard-model"); route.Backend != "remote" || route.ProviderKey == "" {
		t.Fatalf("configured guard route = %#v, want remote provider route", route)
	}
	if route := snapshotRoute(t, snapshot, "codex-bare-model"); route.Backend != codexSubagentProviderName || route.LegacyProvider != codexSubagentProviderName {
		t.Fatalf("Codex worker route = %#v, want bare model bound to Codex", route)
	}

	var codexWorld *ExecutionWorldPolicySnapshot
	for i := range snapshot.ExecutionWorlds {
		if snapshot.ExecutionWorlds[i].Backend == codexSubagentProviderName {
			codexWorld = &snapshot.ExecutionWorlds[i]
			break
		}
	}
	if codexWorld == nil || codexWorld.ExecutionWorld != localExecutionWorldName || codexWorld.ProjectRootHash == "" || len(codexWorld.InheritEnv) != 2 {
		t.Fatalf("codex execution world = %#v, want configured world, hashed root, and HOME/CODEX_HOME", codexWorld)
	}
	if len(codexWorld.AgentNetworkPolicies) != 1 || codexWorld.AgentNetworkPolicies[0] != (ExecutionAgentNetworkPolicySnapshot{Agent: "codex-worker", NetworkDisabled: false}) {
		t.Fatalf("codex agent network policies = %#v, want frozen effective policy for codex-worker", codexWorld.AgentNetworkPolicies)
	}
	for _, variable := range codexWorld.InheritEnv {
		if !variable.Present || variable.ValueHash == "" {
			t.Fatalf("codex environment variable = %#v, want redacted present value", variable)
		}
	}

	persisted := LoadSession(workspace)
	if persisted == nil || persisted.ExecutionPolicySnapshot == nil || persisted.ExecutionPolicySnapshot.ConfigurationHash != snapshot.ConfigurationHash {
		t.Fatalf("persisted execution policy snapshot = %#v, want admitted fingerprint", persisted)
	}
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	policyEvents := 0
	for _, event := range events {
		if event.Type != string(EventExecutionPolicySnapshot) {
			continue
		}
		policyEvents++
		var eventSnapshot ExecutionPolicySnapshot
		if err := json.Unmarshal(event.Payload, &eventSnapshot); err != nil {
			t.Fatalf("decode policy event: %v", err)
		}
		if eventSnapshot.ConfigurationHash != snapshot.ConfigurationHash {
			t.Fatalf("event fingerprint = %q, want %q", eventSnapshot.ConfigurationHash, snapshot.ConfigurationHash)
		}
		if strings.Contains(string(event.Payload), firstHome) || strings.Contains(string(event.Payload), firstCodexHome) || strings.Contains(string(event.Payload), c.projectDir) {
			t.Fatalf("policy event leaked inherited environment or project root: %s", event.Payload)
		}
	}
	if policyEvents != 1 {
		t.Fatalf("execution policy event count = %d, want 1", policyEvents)
	}
	if replayed := ReduceToSessionData(events); replayed.ExecutionPolicySnapshot == nil || replayed.ExecutionPolicySnapshot.ConfigurationHash != snapshot.ConfigurationHash {
		t.Fatalf("event replay policy snapshot = %#v, want admitted snapshot", replayed.ExecutionPolicySnapshot)
	}

	// The accessor cannot expose mutable policy state to callers.
	snapshot.TeamMaxConcurrent = 99
	if got := c.ExecutionPolicySnapshot().TeamMaxConcurrent; got != 4 {
		t.Fatalf("mutated getter changed coordinator policy to %d", got)
	}
	c.session.Agents["codex-worker"].SubagentProvider = ""
	if changed, err := newExecutionPolicyState(c); err != nil || changed.snapshot.ConfigurationHash == snapshot.ConfigurationHash {
		t.Fatalf("agent provider binding change policy = %#v, err=%v; want changed fingerprint", changed, err)
	}
	c.session.Agents["codex-worker"].SubagentProvider = codexSubagentProviderName

	// Mutate all live inputs after the snapshot. New semaphore construction and
	// Codex child startup must still consume only the captured values.
	c.maxConcurrent = 1
	c.session.Config.Providers["remote"] = config.ProviderConfig{MaxConcurrent: 1}
	c.session.Config.DefaultLLMBackend = "remote"
	secondHome := t.TempDir()
	secondCodexHome := t.TempDir()
	t.Setenv("HOME", secondHome)
	t.Setenv("CODEX_HOME", secondCodexHome)

	scheduler := newDAGScheduler(c, []TaskDef{{Agent: "worker", Goal: "test"}}, nil, nil)
	if scheduler.sem == nil || cap(scheduler.sem) != 4 {
		t.Fatalf("team semaphore cap = %d, want frozen 4", cap(scheduler.sem))
	}
	if providerSemaphore := c.providerSemaphore("remote/later-model"); providerSemaphore == nil || cap(providerSemaphore) != 3 {
		t.Fatalf("provider semaphore cap = %d, want frozen 3", cap(providerSemaphore))
	}
	backendSemaphore, err := c.executionBackendSemaphore(execution.ExecutionTarget{Backend: "remote", Model: "later-model"})
	if err != nil || backendSemaphore == nil || cap(backendSemaphore) != 3 {
		t.Fatalf("backend semaphore cap = %d, err=%v, want frozen 3", cap(backendSemaphore), err)
	}
	bareTarget, err := c.resolveCanonicalTaskTarget("later-bare-model", "")
	if err != nil || bareTarget.Backend != execution.OllamaBackendName {
		t.Fatalf("bare model route = %#v, err=%v, want frozen %q backend", bareTarget, err, execution.OllamaBackendName)
	}
	codex := &CodexSubagentProvider{coordinator: c, name: codexSubagentProviderName, config: c.session.Config.SubagentProviders[codexSubagentProviderName]}
	childEnvironment := strings.Join(codex.childEnvironment(), "\n")
	if !strings.Contains(childEnvironment, "HOME="+firstHome) || !strings.Contains(childEnvironment, "CODEX_HOME="+firstCodexHome) || strings.Contains(childEnvironment, secondHome) || strings.Contains(childEnvironment, secondCodexHome) {
		t.Fatalf("Codex child environment = %q, want frozen HOME/CODEX_HOME", childEnvironment)
	}
	projectRoot := c.projectDir
	c.projectDir = t.TempDir()
	c.noNet = true
	c.session.Agents["codex-worker"].NoNet = true
	frozenRoot, frozenNetworkDisabled, err := c.codexExecutionWorld(codexSubagentProviderName, c.session.Agents["codex-worker"])
	if err != nil || frozenRoot != projectRoot || frozenNetworkDisabled {
		t.Fatalf("frozen Codex execution world = root=%q no-net=%t err=%v, want root=%q no-net=false", frozenRoot, frozenNetworkDisabled, err, projectRoot)
	}
	if err := c.checkRunAdmission(); err == nil || !strings.Contains(err.Error(), "execution policy changed after coordinator startup") {
		t.Fatalf("post-freeze policy drift error = %v, want admission failure", err)
	}
}

func TestExecutionPolicySnapshotBlocksResumeOnPersistedConfigurationDrift(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	first := newExecutionPolicySnapshotCoordinator(t, workspace, 2, 3)
	first.initEventStore()
	if err := first.checkRunAdmission(); err != nil {
		t.Fatalf("first admission: %v", err)
	}
	if err := first.eventStore.Close(); err != nil {
		t.Fatal(err)
	}
	first.eventStore = nil

	checkpoint := LoadSession(workspace)
	if checkpoint == nil || checkpoint.ExecutionPolicySnapshot == nil {
		t.Fatal("first admission did not persist an execution policy snapshot")
	}
	second := newExecutionPolicySnapshotCoordinator(t, workspace, 2, 1)
	second.SetSessionData(checkpoint)
	second.initEventStore()
	if err := second.checkRunAdmission(); err == nil || !strings.Contains(err.Error(), "snapshot drift detected") {
		t.Fatalf("resume drift admission error = %v, want persisted policy drift", err)
	}

	// Ensure the failed admission did not create an alternate policy event.
	events, err := second.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	policyEvents := 0
	for _, event := range events {
		if event.Type == string(EventExecutionPolicySnapshot) {
			policyEvents++
		}
	}
	if policyEvents != 1 {
		t.Fatalf("policy events after rejected resume = %d, want 1", policyEvents)
	}
}

func TestExecutionPolicySnapshotProvidesCodexNetworkPolicyForEverySelectableAgentRoute(t *testing.T) {
	tests := []struct {
		name    string
		options executionPolicySnapshotCoordinatorOptions
		model   string
	}{
		{
			name: "extra model",
			options: executionPolicySnapshotCoordinatorOptions{
				workerNoNet:       true,
				workerExtraModels: []string{"codex/extra-model"},
				codexConfig:       defaultExecutionPolicyCodexConfig(),
			},
			model: "codex/extra-model",
		},
		{
			name: "model list",
			options: executionPolicySnapshotCoordinatorOptions{
				workerNoNet: true,
				modelList:   []config.ModelEntry{{ID: "codex/model-list-model"}},
				codexConfig: defaultExecutionPolicyCodexConfig(),
			},
			model: "codex/model-list-model",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newExecutionPolicySnapshotCoordinatorWithOptions(t, t.TempDir(), 2, 3, test.options)
			target, err := c.resolveCanonicalTaskTarget(test.model, "")
			if err != nil || target.Backend != codexSubagentProviderName {
				t.Fatalf("selectable Codex target = %#v, err=%v", target, err)
			}
			root, networkDisabled, err := c.codexExecutionWorld(target.Backend, c.session.Agents["worker"])
			if err != nil || root != c.projectDir || !networkDisabled {
				t.Fatalf("Codex execution world for selectable %s = root=%q no-net=%t err=%v, want root=%q no-net=true", test.name, root, networkDisabled, err, c.projectDir)
			}
			policy := c.ExecutionPolicySnapshot()
			var world *ExecutionWorldPolicySnapshot
			for i := range policy.ExecutionWorlds {
				if policy.ExecutionWorlds[i].Backend == codexSubagentProviderName {
					world = &policy.ExecutionWorlds[i]
					break
				}
			}
			if world == nil {
				t.Fatal("Codex execution world is missing from the snapshot")
			}
			var workerPolicy *ExecutionAgentNetworkPolicySnapshot
			for i := range world.AgentNetworkPolicies {
				if world.AgentNetworkPolicies[i].Agent == "worker" {
					workerPolicy = &world.AgentNetworkPolicies[i]
					break
				}
			}
			if workerPolicy == nil || !workerPolicy.NetworkDisabled {
				t.Fatalf("Codex snapshot agent network policies = %#v, want worker's effective no-net policy", world.AgentNetworkPolicies)
			}
		})
	}
}

func TestExecutionPolicySnapshotBlocksResumedLLMBackendIdentityDriftBeforeProviderCall(t *testing.T) {
	var providerCalls atomic.Int32
	provider := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			providerCalls.Add(1)
		}))
	}
	firstProvider := provider()
	defer firstProvider.Close()
	secondProvider := provider()
	defer secondProvider.Close()

	workspace := t.TempDir()
	first := newExecutionPolicySnapshotCoordinatorWithBackendOptions(t, workspace, 2, 3, firstProvider.URL, "first-provider-secret", defaultExecutionPolicyCodexConfig())
	if err := first.AdmitExecutionPolicy(); err != nil {
		t.Fatalf("first admission: %v", err)
	}
	snapshot := first.ExecutionPolicySnapshot()
	if identity := snapshotBackend(t, snapshot, execution.OllamaBackendName).IdentityHash; identity == "" {
		t.Fatal("LLM backend identity hash is missing")
	}
	persisted, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), firstProvider.URL) || strings.Contains(string(persisted), "first-provider-secret") {
		t.Fatalf("snapshot leaked provider transport material: %s", persisted)
	}
	checkpoint := LoadSession(workspace)
	if checkpoint == nil {
		t.Fatal("first admission did not persist its checkpoint")
	}
	if err := first.eventStore.Close(); err != nil {
		t.Fatal(err)
	}
	first.eventStore = nil

	providerCalls.Store(0)
	second := newExecutionPolicySnapshotCoordinatorWithBackendOptions(t, workspace, 2, 3, secondProvider.URL, "first-provider-secret", defaultExecutionPolicyCodexConfig())
	second.SetSessionData(checkpoint)
	if _, err := second.ExecuteTasks(context.Background(), []TaskDef{{Agent: "worker", Goal: "must not contact changed provider"}}); err == nil || !strings.Contains(err.Error(), "snapshot drift detected") {
		t.Fatalf("resumed provider identity drift error = %v, want pre-provider admission failure", err)
	}
	if got := providerCalls.Load(); got != 0 {
		t.Fatalf("provider calls after rejected identity drift = %d, want 0", got)
	}
}

func TestExecutionPolicySnapshotBlocksResumedCodexIdentityDriftBeforeChildProcess(t *testing.T) {
	workspace := t.TempDir()
	first := newExecutionPolicySnapshotCoordinatorWithBackendOptions(t, workspace, 2, 3, "", "", defaultExecutionPolicyCodexConfig())
	if err := first.AdmitExecutionPolicy(); err != nil {
		t.Fatalf("first admission: %v", err)
	}
	checkpoint := LoadSession(workspace)
	if checkpoint == nil {
		t.Fatal("first admission did not persist its checkpoint")
	}
	if err := first.eventStore.Close(); err != nil {
		t.Fatal(err)
	}
	first.eventStore = nil

	marker := filepath.Join(t.TempDir(), "codex-child-started")
	launcher := filepath.Join(t.TempDir(), "codex-launcher")
	launcherSource := "#!/bin/sh\nprintf child-started > " + strconv.Quote(marker) + "\n"
	if err := os.WriteFile(launcher, []byte(launcherSource), 0o700); err != nil {
		t.Fatal(err)
	}
	changedCodexConfig := defaultExecutionPolicyCodexConfig()
	changedCodexConfig.Command = []string{launcher}
	changedCodexConfig.Protocol = "app-server-v3"
	second := newExecutionPolicySnapshotCoordinatorWithBackendOptions(t, workspace, 2, 3, "", "", changedCodexConfig)
	second.SetSessionData(checkpoint)
	if _, err := second.ExecuteTasks(context.Background(), []TaskDef{{Agent: "codex-worker", Goal: "must not launch changed child"}}); err == nil || !strings.Contains(err.Error(), "snapshot drift detected") {
		t.Fatalf("resumed Codex identity drift error = %v, want pre-process admission failure", err)
	}
	if _, err := os.Stat(marker); err == nil || !os.IsNotExist(err) {
		t.Fatalf("Codex child started after rejected identity drift: stat err=%v", err)
	}
}

func TestExecutionPolicySnapshotBlocksResumedCodexExecutionWorldDriftBeforeChildProcess(t *testing.T) {
	launcherConfig := func(t *testing.T) (agent.SubagentProviderConfig, string) {
		t.Helper()
		marker := filepath.Join(t.TempDir(), "codex-child-started")
		launcher := filepath.Join(t.TempDir(), "codex-launcher")
		launcherSource := "#!/bin/sh\nprintf child-started > " + strconv.Quote(marker) + "\n"
		if err := os.WriteFile(launcher, []byte(launcherSource), 0o700); err != nil {
			t.Fatal(err)
		}
		config := defaultExecutionPolicyCodexConfig()
		config.Command = []string{launcher}
		return config, marker
	}

	tests := []struct {
		name          string
		firstOptions  executionPolicySnapshotCoordinatorOptions
		secondOptions executionPolicySnapshotCoordinatorOptions
		firstRoot     string
		secondRoot    string
	}{
		{
			name:          "coordinator no-net",
			firstOptions:  executionPolicySnapshotCoordinatorOptions{},
			secondOptions: executionPolicySnapshotCoordinatorOptions{noNet: true},
		},
		{
			name:          "agent no-net",
			firstOptions:  executionPolicySnapshotCoordinatorOptions{},
			secondOptions: executionPolicySnapshotCoordinatorOptions{codexWorkerNoNet: true},
		},
		{
			name:          "project root",
			firstOptions:  executionPolicySnapshotCoordinatorOptions{},
			secondOptions: executionPolicySnapshotCoordinatorOptions{},
			firstRoot:     t.TempDir(),
			secondRoot:    t.TempDir(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			codexConfig, marker := launcherConfig(t)
			test.firstOptions.codexConfig = codexConfig
			test.secondOptions.codexConfig = codexConfig
			if test.firstRoot == "" {
				test.firstRoot = mustExecutionPolicyProjectRoot(t)
			}
			if test.secondRoot == "" {
				test.secondRoot = test.firstRoot
			}

			first := newExecutionPolicySnapshotCoordinatorAtProjectRoot(t, test.firstRoot, workspace, 2, 3, test.firstOptions)
			if err := first.AdmitExecutionPolicy(); err != nil {
				t.Fatalf("first admission: %v", err)
			}
			checkpoint := LoadSession(workspace)
			if checkpoint == nil {
				t.Fatal("first admission did not persist its checkpoint")
			}
			if err := first.eventStore.Close(); err != nil {
				t.Fatal(err)
			}
			first.eventStore = nil

			second := newExecutionPolicySnapshotCoordinatorAtProjectRoot(t, test.secondRoot, workspace, 2, 3, test.secondOptions)
			second.SetSessionData(checkpoint)
			if _, err := second.ExecuteTasks(context.Background(), []TaskDef{{Agent: "codex-worker", Goal: "must not launch after execution-world drift"}}); err == nil || !strings.Contains(err.Error(), "snapshot drift detected") {
				t.Fatalf("resumed Codex execution-world drift error = %v, want pre-process admission failure", err)
			}
			if _, err := os.Stat(marker); err == nil || !os.IsNotExist(err) {
				t.Fatalf("Codex child started after rejected execution-world drift: stat err=%v", err)
			}
		})
	}
}

func TestCodexRunAttemptAdmitsExecutionPolicyBeforeChildProcess(t *testing.T) {
	markerConfig := func(t *testing.T) (agent.SubagentProviderConfig, string) {
		t.Helper()
		marker := filepath.Join(t.TempDir(), "codex-child-started")
		launcher := filepath.Join(t.TempDir(), "codex-launcher")
		launcherSource := "#!/bin/sh\nprintf child-started > " + strconv.Quote(marker) + "\n"
		if err := os.WriteFile(launcher, []byte(launcherSource), 0o700); err != nil {
			t.Fatal(err)
		}
		config := defaultExecutionPolicyCodexConfig()
		config.Command = []string{launcher}
		config.InheritEnv = nil
		return config, marker
	}
	assertBlocked := func(t *testing.T, c *Coordinator, config agent.SubagentProviderConfig, marker, want string) {
		t.Helper()
		provider := NewCodexSubagentProvider(c, codexSubagentProviderName, config)
		if _, err := provider.RunAttempt(t.Context(), AttemptRequest{}); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("direct Codex admission error = %v, want %q before request/process activity", err, want)
		}
		if _, err := os.Stat(marker); err == nil || !os.IsNotExist(err) {
			t.Fatalf("Codex child started after rejected direct admission: stat err=%v", err)
		}
	}

	t.Run("resumed no-net drift", func(t *testing.T) {
		workspace := t.TempDir()
		config, marker := markerConfig(t)
		first := newExecutionPolicySnapshotCoordinatorWithOptions(t, workspace, 2, 3, executionPolicySnapshotCoordinatorOptions{codexConfig: config})
		if err := first.AdmitExecutionPolicy(); err != nil {
			t.Fatalf("first admission: %v", err)
		}
		checkpoint := LoadSession(workspace)
		if err := first.eventStore.Close(); err != nil {
			t.Fatal(err)
		}
		first.eventStore = nil
		second := newExecutionPolicySnapshotCoordinatorWithOptions(t, workspace, 2, 3, executionPolicySnapshotCoordinatorOptions{codexConfig: config, noNet: true})
		second.SetSessionData(checkpoint)
		assertBlocked(t, second, config, marker, "snapshot drift detected")
	})

	t.Run("resumed project-root drift", func(t *testing.T) {
		workspace := t.TempDir()
		config, marker := markerConfig(t)
		first := newExecutionPolicySnapshotCoordinatorAtProjectRoot(t, t.TempDir(), workspace, 2, 3, executionPolicySnapshotCoordinatorOptions{codexConfig: config})
		if err := first.AdmitExecutionPolicy(); err != nil {
			t.Fatalf("first admission: %v", err)
		}
		checkpoint := LoadSession(workspace)
		if err := first.eventStore.Close(); err != nil {
			t.Fatal(err)
		}
		first.eventStore = nil
		second := newExecutionPolicySnapshotCoordinatorAtProjectRoot(t, t.TempDir(), workspace, 2, 3, executionPolicySnapshotCoordinatorOptions{codexConfig: config})
		second.SetSessionData(checkpoint)
		assertBlocked(t, second, config, marker, "snapshot drift detected")
	})

	t.Run("legacy interrupted state", func(t *testing.T) {
		workspace := t.TempDir()
		config, marker := markerConfig(t)
		c := newExecutionPolicySnapshotCoordinatorWithOptions(t, workspace, 2, 3, executionPolicySnapshotCoordinatorOptions{codexConfig: config})
		c.initEventStore()
		item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "codex-worker", Desc: "legacy interrupted task"}})[0]
		if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskInProgress, "", ""); err != nil {
			t.Fatal(err)
		}
		c.SetSessionData(&SessionData{})
		assertBlocked(t, c, config, marker, "legacy session has interrupted tasks")
	})
}

func TestHufuLocalRunAttemptAdmitsExecutionPolicyBeforeProviderCall(t *testing.T) {
	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		providerCalls.Add(1)
	}))
	defer provider.Close()

	assertBlocked := func(t *testing.T, c *Coordinator, want string) {
		t.Helper()
		if _, err := NewHufuLocalSubagentProvider(c).RunAttempt(t.Context(), AttemptRequest{}); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("direct hufu-local admission error = %v, want %q before contract/provider work", err, want)
		}
		if got := providerCalls.Load(); got != 0 {
			t.Fatalf("provider calls after rejected hufu-local admission = %d, want 0", got)
		}
	}

	t.Run("resumed policy drift", func(t *testing.T) {
		workspace := t.TempDir()
		first := newExecutionPolicySnapshotCoordinatorWithBackendOptions(t, workspace, 2, 3, provider.URL, "", defaultExecutionPolicyCodexConfig())
		if err := first.AdmitExecutionPolicy(); err != nil {
			t.Fatalf("first admission: %v", err)
		}
		checkpoint := LoadSession(workspace)
		if err := first.eventStore.Close(); err != nil {
			t.Fatal(err)
		}
		first.eventStore = nil

		providerCalls.Store(0)
		second := newExecutionPolicySnapshotCoordinatorWithBackendOptions(t, workspace, 2, 1, provider.URL, "", defaultExecutionPolicyCodexConfig())
		second.SetSessionData(checkpoint)
		assertBlocked(t, second, "snapshot drift detected")
	})

	t.Run("legacy interrupted state", func(t *testing.T) {
		workspace := t.TempDir()
		c := newExecutionPolicySnapshotCoordinatorWithBackendOptions(t, workspace, 2, 3, provider.URL, "", defaultExecutionPolicyCodexConfig())
		c.initEventStore()
		item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "legacy interrupted task"}})[0]
		if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskInProgress, "", ""); err != nil {
			t.Fatal(err)
		}
		c.SetSessionData(&SessionData{})
		providerCalls.Store(0)
		assertBlocked(t, c, "legacy session has interrupted tasks")
	})
}

func TestRawLLMAccessorsAdmitExecutionPolicyBeforeProviderAccess(t *testing.T) {
	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		providerCalls.Add(1)
	}))
	defer provider.Close()

	assertBlocked := func(t *testing.T, c *Coordinator, want string) {
		t.Helper()
		target := execution.ExecutionTarget{Backend: execution.OllamaBackendName, Model: "raw-accessor-model"}
		backend, err := c.ExecutionRegistry().LanguageModelBackend(target)
		if err != nil {
			t.Fatalf("resolve LLM backend: %v", err)
		}
		catalog, ok := backend.(ModelCatalogBackend)
		if !ok {
			t.Fatalf("LLM backend %T does not expose a model catalog", backend)
		}
		gated, ok := backend.(GatedAgentBackend)
		if !ok {
			t.Fatalf("LLM backend %T does not expose a gated provider", backend)
		}
		accessors := []struct {
			name string
			call func() error
		}{
			{
				name: "LanguageModel",
				call: func() error {
					_, err := backend.LanguageModel(t.Context(), target)
					return err
				},
			},
			{
				name: "ListModelNames",
				call: func() error {
					_, err := catalog.ListModelNames(t.Context(), target)
					return err
				},
			},
			{
				name: "AgentProvider",
				call: func() error {
					_, err := gated.AgentProvider(t.Context(), target)
					return err
				},
			},
			{
				name: "ModelRuntime.ProviderFor",
				call: func() error {
					_, err := c.ModelRuntime().ProviderFor(target.String())
					return err
				},
			},
		}
		for _, accessor := range accessors {
			providerCalls.Store(0)
			if err := accessor.call(); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("raw %s admission error = %v, want %q before provider access", accessor.name, err, want)
			}
			if got := providerCalls.Load(); got != 0 {
				t.Fatalf("provider calls after rejected raw %s access = %d, want 0", accessor.name, got)
			}
		}
	}

	t.Run("resumed policy drift", func(t *testing.T) {
		workspace := t.TempDir()
		first := newExecutionPolicySnapshotCoordinatorWithBackendOptions(t, workspace, 2, 3, provider.URL, "", defaultExecutionPolicyCodexConfig())
		if err := first.AdmitExecutionPolicy(); err != nil {
			t.Fatalf("first admission: %v", err)
		}
		checkpoint := LoadSession(workspace)
		if err := first.eventStore.Close(); err != nil {
			t.Fatal(err)
		}
		first.eventStore = nil

		second := newExecutionPolicySnapshotCoordinatorWithBackendOptions(t, workspace, 2, 1, provider.URL, "", defaultExecutionPolicyCodexConfig())
		second.SetSessionData(checkpoint)
		assertBlocked(t, second, "snapshot drift detected")
	})

	t.Run("legacy interrupted state", func(t *testing.T) {
		workspace := t.TempDir()
		c := newExecutionPolicySnapshotCoordinatorWithBackendOptions(t, workspace, 2, 3, provider.URL, "", defaultExecutionPolicyCodexConfig())
		c.initEventStore()
		item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "legacy interrupted task"}})[0]
		if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskInProgress, "", ""); err != nil {
			t.Fatal(err)
		}
		c.SetSessionData(&SessionData{})
		assertBlocked(t, c, "legacy session has interrupted tasks")
	})
}

func mustExecutionPolicyProjectRoot(t *testing.T) string {
	t.Helper()
	projectRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("get current project root: %v", err)
	}
	return projectRoot
}

func TestExecutionPolicyCodexIdentityCoversCommandAndProtocol(t *testing.T) {
	base := defaultExecutionPolicyCodexConfig()
	baseIdentity := executionPolicyCodexIdentityHash(codexSubagentProviderName, base)
	changedCommand := base
	changedCommand.Command = []string{"other-codex", "app-server"}
	if got := executionPolicyCodexIdentityHash(codexSubagentProviderName, changedCommand); got == baseIdentity {
		t.Fatal("changed Codex command retained the same execution identity")
	}
	changedProtocol := base
	changedProtocol.Protocol = "app-server-v3"
	if got := executionPolicyCodexIdentityHash(codexSubagentProviderName, changedProtocol); got == baseIdentity {
		t.Fatal("changed Codex protocol retained the same execution identity")
	}
}

func TestExecutionPolicySnapshotBlocksLegacyInterruptedTasks(t *testing.T) {
	workspace := t.TempDir()
	c := newExecutionPolicySnapshotCoordinator(t, workspace, 2, 3)
	c.initEventStore()
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "interrupted legacy task"}})[0]
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskInProgress, "", ""); err != nil {
		t.Fatal(err)
	}
	c.SetSessionData(&SessionData{})

	if err := c.checkRunAdmission(); err == nil || !strings.Contains(err.Error(), "legacy session has interrupted tasks") {
		t.Fatalf("legacy interrupted admission error = %v, want fail-closed migration barrier", err)
	}
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == string(EventExecutionPolicySnapshot) {
			t.Fatalf("legacy interrupted admission appended policy event before rejecting: %#v", event)
		}
	}
}

func TestFreezeExecutionPolicyAtStartupClosesSetupJournalAfterDurability(t *testing.T) {
	workspace := t.TempDir()
	c := newExecutionPolicySnapshotCoordinator(t, workspace, 2, 3)
	if err := c.FreezeExecutionPolicyAtStartup(); err != nil {
		t.Fatalf("startup freeze: %v", err)
	}
	if c.eventStore != nil {
		t.Fatal("startup freeze retained a setup-time event-store handle")
	}
	store, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot, err := executionPolicySnapshotFromEvents(events); err != nil || snapshot == nil || snapshot.ConfigurationHash != c.ExecutionPolicySnapshot().ConfigurationHash {
		t.Fatalf("durable startup policy snapshot = %#v, err=%v", snapshot, err)
	}
}

func TestExecutionPolicySnapshotEventJournalOwnsProjection(t *testing.T) {
	policyA := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 3).ExecutionPolicySnapshot()
	policyB := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 3, 3).ExecutionPolicySnapshot()
	if policyA == nil || policyB == nil || policyA.ConfigurationHash == policyB.ConfigurationHash {
		t.Fatalf("test snapshots must be valid and distinct: A=%#v B=%#v", policyA, policyB)
	}
	policyPayload, err := json.Marshal(policyA)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("event-only lineage repairs stale session snapshot", func(t *testing.T) {
		workspace := t.TempDir()
		store, err := NewEventStore(workspace, "run-policy", "session-policy")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.AppendPersisted(RunEvent{Type: string(EventExecutionPolicySnapshot), Actor: "coordinator", Payload: policyPayload}); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}

		c := &Coordinator{session: &TeamSession{Workspace: workspace}, sessionData: &SessionData{ExecutionPolicySnapshot: cloneExecutionPolicySnapshot(policyB)}, taskTracker: NewTaskTracker()}
		c.initEventStore()
		defer c.eventStore.Close()
		if c.sessionData.RecoveryRequired {
			t.Fatalf("event-authoritative repair unexpectedly required recovery: %s", c.sessionData.RecoveryReason)
		}
		if got := c.sessionData.ExecutionPolicySnapshot; got == nil || got.ConfigurationHash != policyA.ConfigurationHash {
			t.Fatalf("repaired snapshot = %#v, want event fingerprint %q", got, policyA.ConfigurationHash)
		}
		if saved := LoadSession(workspace); saved == nil || saved.ExecutionPolicySnapshot == nil || saved.ExecutionPolicySnapshot.ConfigurationHash != policyA.ConfigurationHash {
			t.Fatalf("repaired snapshot was not checkpointed: %#v", saved)
		}
	})

	t.Run("task-bearing lineage rejects checkpoint-only snapshot", func(t *testing.T) {
		workspace := t.TempDir()
		item := &TodoItem{ID: "task-policy", Agent: "worker", Desc: "execute", Status: TaskDone}
		payload, err := taskTransitionPayloadJSON(item)
		if err != nil {
			t.Fatal(err)
		}
		store, err := NewEventStore(workspace, "run-policy", "session-policy")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.AppendPersisted(RunEvent{Type: string(EventTaskCompleted), Actor: "worker", TaskID: item.ID, Payload: payload}); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}

		c := &Coordinator{session: &TeamSession{Workspace: workspace}, sessionData: &SessionData{ExecutionPolicySnapshot: cloneExecutionPolicySnapshot(policyA), Tasks: []*TodoItem{item}}, taskTracker: NewTaskTracker()}
		c.initEventStore()
		defer c.eventStore.Close()
		if !c.sessionData.RecoveryRequired || !strings.Contains(c.sessionData.RecoveryReason, "execution policy snapshot") {
			t.Fatalf("checkpoint-only policy was admitted for task lineage: %#v", c.sessionData)
		}
	})
}

func TestExecutionPolicySnapshotReplayIgnoresLegacyEnvelope(t *testing.T) {
	policy := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 3).ExecutionPolicySnapshot()
	payload, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}

	replayed := ReduceToSessionData([]RunEvent{{
		SchemaVersion: eventStoreLegacySchemaVersion,
		Type:          string(EventExecutionPolicySnapshot),
		Payload:       payload,
	}})
	if replayed.ExecutionPolicySnapshot != nil {
		t.Fatalf("legacy event established an execution policy snapshot: %#v", replayed.ExecutionPolicySnapshot)
	}
}

func TestExecutionPolicySnapshotGatesDirectTaskAndPlanEntrypoints(t *testing.T) {
	workspace := t.TempDir()
	c := newExecutionPolicySnapshotCoordinator(t, workspace, 2, 3)
	legacy := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "interrupted legacy task"}})[0]
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(legacy.ID, TaskInProgress, "", ""); err != nil {
		t.Fatal(err)
	}
	c.SetSessionData(&SessionData{})

	if _, err := c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "worker", Goal: "must not schedule"}}); err == nil || !strings.Contains(err.Error(), "legacy session has interrupted tasks") {
		t.Fatalf("direct ExecuteTasks admission error = %v, want legacy snapshot barrier", err)
	}
	if items := c.taskTracker.TodoList().Items(); len(items) != 1 || items[0].ID != legacy.ID {
		t.Fatalf("direct ExecuteTasks created work after failed admission: %#v", items)
	}

	revision := PlanRevision{
		ID:                    "direct-plan",
		Goal:                  "must not execute",
		AcceptanceFingerprint: AcceptanceFingerprint(nil, ""),
		TaskDAG:               []TaskDef{{ID: "task", Agent: "worker", Goal: "must not schedule"}},
		DAGDiff:               PlanDiff{Added: []PlanTaskChange{{ID: "task"}}},
	}
	if _, err := c.ExecutePlanRevision(context.Background(), revision); err == nil || !strings.Contains(err.Error(), "legacy session has interrupted tasks") {
		t.Fatalf("ExecutePlanRevision admission error = %v, want legacy snapshot barrier", err)
	}

	c.planRevisionsMu.Lock()
	c.planRevisions = []PlanRevision{revision}
	c.planRevisionsMu.Unlock()
	if _, err := c.ReviewPlanRevisionWithContext(context.Background(), revision.ID); err == nil || !strings.Contains(err.Error(), "legacy session has interrupted tasks") {
		t.Fatalf("plan reviewer admission error = %v, want legacy snapshot barrier before provider construction", err)
	}
}

func TestExecutionPolicySnapshotGatesDirectTaskExecutionOnLiveDrift(t *testing.T) {
	c := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 3)
	if err := c.AdmitExecutionPolicy(); err != nil {
		t.Fatalf("initial admission: %v", err)
	}
	c.maxConcurrent = 1
	if _, err := c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "worker", Goal: "must not schedule"}}); err == nil || !strings.Contains(err.Error(), "execution policy changed after coordinator startup") {
		t.Fatalf("direct drift admission error = %v, want frozen-policy failure", err)
	}
}
