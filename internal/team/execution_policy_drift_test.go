package team

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestExecutionPolicyDifferences(t *testing.T) {
	frozen := &ExecutionPolicySnapshot{
		Version: executionPolicySnapshotVersion, TeamMaxConcurrent: 2,
		Backends:          []ExecutionBackendPolicySnapshot{{Backend: "remote", MaxConcurrent: 3, IdentityHash: "old-provider-identity"}},
		ModelRoutes:       []ExecutionModelRouteSnapshot{{Model: "old-model", Backend: "remote", ProviderKey: "remote"}},
		ResultContracts:   []ExecutionResultContractPolicySnapshot{{Owner: "agent:verifier", ID: "schemas/audit.json", SchemaSHA256: "old-schema"}},
		MCPWorkerPolicies: []ExecutionMCPWorkerPolicySnapshot{{Server: "browser", Hash: "old-tool-policy"}},
		ExecutionWorlds:   []ExecutionWorldPolicySnapshot{{Backend: "remote", InheritEnv: []ExecutionEnvironmentVariableSnapshot{{Name: "HOME", Present: true, ValueHash: "old-home"}}}},
		ConfigurationHash: "old-overall-hash",
	}
	tests := []struct {
		name   string
		change func(*ExecutionPolicySnapshot)
		want   []string
		absent []string
	}{
		{
			name:   "numeric values",
			change: func(s *ExecutionPolicySnapshot) { s.TeamMaxConcurrent = 4 },
			want:   []string{"team_max_concurrent: 2 -> 4"},
		},
		{
			name:   "provider identity stays opaque",
			change: func(s *ExecutionPolicySnapshot) { s.Backends[0].IdentityHash = "new-provider-identity" },
			want:   []string{`backends["backend=remote"].identity_hash`, "underlying values are not retained"},
			absent: []string{"old-provider-identity", "new-provider-identity"},
		},
		{
			name:   "model replacement",
			change: func(s *ExecutionPolicySnapshot) { s.ModelRoutes[0].Model = "new-model" },
			want:   []string{`model_routes["model=new-model,backend=remote,provider_key=remote"]: added`, `model_routes["model=old-model,backend=remote,provider_key=remote"]: removed`},
		},
		{
			name: "model insertion does not shift existing entries",
			change: func(s *ExecutionPolicySnapshot) {
				s.ModelRoutes = append([]ExecutionModelRouteSnapshot{{Model: "a-model", Backend: "remote"}}, s.ModelRoutes...)
			},
			want:   []string{`model_routes["model=a-model,backend=remote,provider_key="]: added`},
			absent: []string{"old-model"},
		},
		{
			name: "same model on distinct backends",
			change: func(s *ExecutionPolicySnapshot) {
				s.ModelRoutes = append(s.ModelRoutes, ExecutionModelRouteSnapshot{Model: "old-model", Backend: "local"})
			},
			want:   []string{`model_routes["model=old-model,backend=local,provider_key="]: added`},
			absent: []string{"backend=remote"},
		},
		{
			name:   "contract schema",
			change: func(s *ExecutionPolicySnapshot) { s.ResultContracts[0].SchemaSHA256 = "new-schema" },
			want:   []string{`result_contracts["owner=agent:verifier"].schema_sha256`, "fingerprint changed"},
			absent: []string{"old-schema", "new-schema"},
		},
		{
			name:   "MCP worker policy",
			change: func(s *ExecutionPolicySnapshot) { s.MCPWorkerPolicies[0].Hash = "new-tool-policy" },
			want:   []string{`mcp_worker_policies["server=browser"].hash`, "fingerprint changed"},
			absent: []string{"old-tool-policy", "new-tool-policy"},
		},
		{
			name:   "environment digest",
			change: func(s *ExecutionPolicySnapshot) { s.ExecutionWorlds[0].InheritEnv[0].ValueHash = "new-home" },
			want:   []string{`execution_worlds["backend=remote"].inherit_env["name=HOME"].value_hash`},
			absent: []string{"old-home", "new-home"},
		},
		{
			name: "optional policy addition",
			change: func(s *ExecutionPolicySnapshot) {
				s.ContextArtifacts = &ExecutionContextArtifactPolicySnapshot{MinBytes: 1024}
			},
			want: []string{"context_artifacts: added"},
		},
		{
			name:   "optional policy removal",
			change: func(s *ExecutionPolicySnapshot) { s.ResultContracts = nil },
			want:   []string{`result_contracts["owner=agent:verifier"]: removed`},
		},
		{
			name: "backend insertion",
			change: func(s *ExecutionPolicySnapshot) {
				s.Backends = append(s.Backends, ExecutionBackendPolicySnapshot{Backend: "local"})
			},
			want: []string{`backends["backend=local"]: added`},
		},
		{
			name: "entry identity terminal safety",
			change: func(s *ExecutionPolicySnapshot) {
				s.MCPWorkerPolicies = append(s.MCPWorkerPolicies, ExecutionMCPWorkerPolicySnapshot{Server: "browser\n\x1b[31m"})
			},
			want:   []string{`server=browser\n\x1b[31m`},
			absent: []string{"\x1b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := cloneExecutionPolicySnapshot(frozen)
			tt.change(current)
			current.ConfigurationHash = "new-overall-hash"
			before, after := cloneExecutionPolicySnapshot(frozen), cloneExecutionPolicySnapshot(current)
			got := executionPolicyDifferences(frozen, current)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("diagnostic missing %q: %s", want, got)
				}
			}
			for _, absent := range append(tt.absent, "configuration_hash", "old-overall-hash", "new-overall-hash") {
				if strings.Contains(got, absent) {
					t.Errorf("diagnostic contains %q: %s", absent, got)
				}
			}
			if !reflect.DeepEqual(frozen, before) || !reflect.DeepEqual(current, after) {
				t.Fatal("diagnostic mutated a snapshot")
			}
			if got != executionPolicyDifferences(frozen, current) {
				t.Fatal("diagnostic order is unstable")
			}
		})
	}
}

func TestExecutionPolicyDifferencesSliceRepresentation(t *testing.T) {
	tests := []struct {
		name    string
		before  *ExecutionPolicySnapshot
		after   *ExecutionPolicySnapshot
		changed bool
	}{
		{
			name: "required null versus empty", changed: true,
			before: &ExecutionPolicySnapshot{}, after: &ExecutionPolicySnapshot{Backends: []ExecutionBackendPolicySnapshot{}},
		},
		{
			name:   "omitted null versus empty",
			before: &ExecutionPolicySnapshot{}, after: &ExecutionPolicySnapshot{ResultContracts: []ExecutionResultContractPolicySnapshot{}},
		},
		{
			name: "reordered entries", changed: true,
			before: &ExecutionPolicySnapshot{Backends: []ExecutionBackendPolicySnapshot{{Backend: "a"}, {Backend: "b"}}},
			after:  &ExecutionPolicySnapshot{Backends: []ExecutionBackendPolicySnapshot{{Backend: "b"}, {Backend: "a"}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			beforeHash, err := executionPolicyConfigurationHash(tt.before)
			if err != nil {
				t.Fatal(err)
			}
			afterHash, err := executionPolicyConfigurationHash(tt.after)
			if err != nil {
				t.Fatal(err)
			}
			got := executionPolicyDifferences(tt.before, tt.after)
			if changed := strings.Contains(got, "\n  - "); changed != tt.changed || changed != (beforeHash != afterHash) {
				t.Fatalf("diagnostic disagrees with canonical hash: %s", got)
			}
		})
	}
}

func TestExecutionPolicyDifferencesRedactsEntryIdentity(t *testing.T) {
	current := &ExecutionPolicySnapshot{MCPWorkerPolicies: []ExecutionMCPWorkerPolicySnapshot{{Server: "https://user:private-password@example.test"}}}
	got := executionPolicyDifferences(&ExecutionPolicySnapshot{}, current)
	if strings.Contains(got, "private-password") || !strings.Contains(got, "mcp_worker_policies") {
		t.Fatalf("unsafe entry label: %s", got)
	}
}

func TestExecutionPolicyDifferencesBoundsOutput(t *testing.T) {
	frozen := &ExecutionPolicySnapshot{}
	current := &ExecutionPolicySnapshot{}
	for i := range maxExecutionPolicyDifferences + 3 {
		current.MCPWorkerPolicies = append(current.MCPWorkerPolicies, ExecutionMCPWorkerPolicySnapshot{Server: fmt.Sprintf("%03d-%s", i, strings.Repeat("x", 1000))})
	}
	got := executionPolicyDifferences(frozen, current)
	if strings.Count(got, "\n  - ") != maxExecutionPolicyDifferences || !strings.Contains(got, "3 additional changes omitted") {
		t.Fatalf("unbounded change count: %s", got)
	}
	if len(got) > 5000 {
		t.Fatalf("diagnostic length = %d, want bounded labels", len(got))
	}
}

func TestExecutionPolicySnapshotDriftDiagnosticPreservesAdmission(t *testing.T) {
	workspace := t.TempDir()
	first := newExecutionPolicySnapshotCoordinator(t, workspace, 2, 3)
	if err := first.FreezeExecutionPolicyAtStartup(); err != nil {
		t.Fatal(err)
	}
	frozen := first.ExecutionPolicySnapshot()
	checkpoint := LoadSession(workspace)
	second := newExecutionPolicySnapshotCoordinator(t, workspace, 2, 1)
	second.SetSessionData(checkpoint)
	err := second.FreezeExecutionPolicyAtStartup()
	if err == nil {
		t.Fatal("changed policy was admitted")
	}
	for _, want := range []string{"snapshot drift detected", `backends["backend=remote"].max_concurrent: 3 -> 1`, "--new"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("startup error missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "result_collection") {
		t.Errorf("unchanged result collection reported as drift: %v", err)
	}
	if got := LoadSession(workspace); !reflect.DeepEqual(got.ExecutionPolicySnapshot, frozen) {
		t.Fatal("rejected admission rewrote the stored policy")
	}
	second.initEventStore()
	events, err := second.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Type == string(EventExecutionPolicySnapshot) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("policy events = %d, want original event only", count)
	}
}

func TestExecutionPolicySnapshotDriftDiagnosticUsesCompatibilityRules(t *testing.T) {
	workspace := t.TempDir()
	first := newExecutionPolicySnapshotCoordinator(t, workspace, 2, 3)
	delete(first.session.Agents, "codex-worker")
	state, err := newExecutionPolicyState(first)
	if err != nil {
		t.Fatal(err)
	}
	frozen := state.snapshot
	// Historical tool transport has no collection block. Environment changes
	// of the unreachable Codex backend are independently compatible.
	frozen.ResultCollection = nil
	for i := range frozen.ExecutionWorlds {
		if frozen.ExecutionWorlds[i].Backend == "codex" {
			frozen.ExecutionWorlds[i].InheritEnv[0].ValueHash = "old-unused-environment"
		}
	}
	frozen.ConfigurationHash, err = executionPolicyConfigurationHash(frozen)
	if err != nil {
		t.Fatal(err)
	}
	second := newExecutionPolicySnapshotCoordinator(t, workspace, 2, 1)
	delete(second.session.Agents, "codex-worker")
	message := second.executionPolicySnapshotDriftError(frozen).Error()
	if !strings.Contains(message, `backends["backend=remote"].max_concurrent`) {
		t.Fatalf("missing incompatible backend setting: %s", message)
	}
	for _, compatible := range []string{"result_collection", "inherit_env", "old-unused-environment"} {
		if strings.Contains(message, compatible) {
			t.Errorf("compatible setting reported as drift (%s): %s", compatible, message)
		}
	}
}

func TestExecutionPolicySnapshotLiveDriftDiagnostic(t *testing.T) {
	c := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 3)
	frozen := c.ExecutionPolicySnapshot()
	c.maxConcurrent = 4
	err := c.ensureExecutionPolicySnapshot()
	if err == nil || !strings.Contains(err.Error(), "execution policy changed after coordinator startup") || !strings.Contains(err.Error(), "team_max_concurrent: 2 -> 4") {
		t.Fatalf("live drift diagnostic = %v", err)
	}
	if !reflect.DeepEqual(c.ExecutionPolicySnapshot(), frozen) {
		t.Fatal("live drift diagnostic rewrote admitted policy")
	}
}
