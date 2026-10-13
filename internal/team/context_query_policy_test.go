package team

import (
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestContextQueryPolicyStrictManifests(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		for _, mode := range []string{"", "legacy", "active", "shadow", "ACTIVE", "typo"} {
			t.Run(fmt.Sprintf("%t/%s", versioned, mode), func(t *testing.T) {
				config := "name: query-policy\nmodel: test\n"
				if mode != "" {
					config += "context-routing:\n  query-mode: " + mode + "\n"
				}
				if versioned {
					config = "apiVersion: hufu.io/v1alpha1\nkind: AgentTeam\nmetadata:\n  name: query-policy\nspec:\n" + "  " + strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(config, "name: query-policy\n"), "\n"), "\n", "\n  ") + "\n"
				}
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(config), 0o600); err != nil {
					t.Fatal(err)
				}
				cfg, err := parseTeamYML(dir, nil)
				valid := mode == "" || mode == "legacy" || mode == "active"
				if (err == nil) != valid {
					t.Fatalf("mode %q: %v", mode, err)
				}
				if valid && cfg.ContextRouting.QueryMode != mode {
					t.Fatal("mode lost")
				}
			})
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: query\nmodel: test\ncontext-routing:\n  query-mod: active\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseTeamYML(dir, nil); err == nil {
		t.Fatal("unknown query field accepted")
	}
}

func TestContextQueryPolicyFreezesModeAndPreservesLegacyHash(t *testing.T) {
	c := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 3)
	legacy := c.ExecutionPolicySnapshot()
	if legacy.ContextQuery != nil {
		t.Fatal("default writes query policy")
	}
	c.session.Config.ContextRouting.QueryMode = "legacy"
	current, err := newExecutionPolicyState(c)
	if err != nil || current.snapshot.ConfigurationHash != legacy.ConfigurationHash {
		t.Fatal("explicit legacy changes historical hash")
	}
	c.session.Config.ContextRouting.QueryMode = "active"
	current, err = newExecutionPolicyState(c)
	if err != nil || current.snapshot.ContextQuery == nil || current.snapshot.ConfigurationHash == legacy.ConfigurationHash {
		t.Fatalf("active not frozen: %v", err)
	}
	if err := c.AdmitExecutionPolicy(); err == nil {
		t.Fatal("live mode drift accepted")
	}
	active := current.snapshot
	for _, version := range []int{executionPolicyLegacySnapshotVersion, executionPolicyPreviousSnapshotVersion} {
		historical := cloneExecutionPolicySnapshot(active)
		historical.Version = version
		if err := validateExecutionPolicySnapshot(historical); err == nil {
			t.Fatal("old snapshot accepted active query policy")
		}
	}
	copy := cloneExecutionPolicySnapshot(active)
	copy.ContextQuery.Limits.SourceRows++
	if err := validateExecutionPolicySnapshot(copy); err == nil {
		t.Fatal("changed source budget accepted")
	}
	if active.ContextQuery.Limits.SourceRows != 64 {
		t.Fatal("snapshot clone aliases original")
	}
	data, err := json.Marshal(legacy)
	if err != nil || strings.Contains(string(data), "context_query") {
		t.Fatal("legacy wire changed")
	}
}

func TestContextQueryPolicyResumeRequiresIdenticalMode(t *testing.T) {
	for _, mode := range []string{"legacy", "active"} {
		t.Run(mode, func(t *testing.T) {
			workspace := t.TempDir()
			options := executionPolicySnapshotCoordinatorOptions{codexConfig: defaultExecutionPolicyCodexConfig(), contextRouting: agent.ContextRoutingPolicy{QueryMode: mode}}
			first := newExecutionPolicySnapshotCoordinatorWithOptions(t, workspace, 2, 3, options)
			if err := first.AdmitExecutionPolicy(); err != nil {
				t.Fatal(err)
			}
			checkpoint := LoadSession(workspace)
			if checkpoint == nil {
				t.Fatal("missing durable snapshot")
			}
			if first.eventStore != nil {
				if err := first.eventStore.Close(); err != nil {
					t.Fatal(err)
				}
				first.eventStore = nil
			}
			resumed := newExecutionPolicySnapshotCoordinatorWithOptions(t, workspace, 2, 3, options)
			resumed.SetSessionData(checkpoint)
			if err := resumed.AdmitExecutionPolicy(); err != nil {
				t.Fatalf("same mode rejected: %v", err)
			}
			if resumed.eventStore != nil {
				if err := resumed.eventStore.Close(); err != nil {
					t.Fatal(err)
				}
				resumed.eventStore = nil
			}
			if mode == "active" {
				options.contextRouting.QueryMode = "legacy"
			} else {
				options.contextRouting.QueryMode = "active"
			}
			changed := newExecutionPolicySnapshotCoordinatorWithOptions(t, workspace, 2, 3, options)
			changed.SetSessionData(checkpoint)
			if err := changed.AdmitExecutionPolicy(); err == nil {
				t.Fatal("resume changed query mode")
			}
		})
	}
}
