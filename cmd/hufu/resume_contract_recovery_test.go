package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/team"
	"github.com/spf13/cobra"
)

func TestResumeContractRecoveryFlags(t *testing.T) {
	savedOpts, upgrade, branch, hash, task, model, metadata, inputPolicy := opts, resumeUpgradeContracts, resumeRecoveryBranch, resumeRecoveryPlan, resumeRepairResult, resumeRepairModel, resumeMetadataOnly, resumeUpgradeInputPolicy
	t.Cleanup(func() {
		opts, resumeUpgradeContracts, resumeRecoveryBranch, resumeRecoveryPlan, resumeRepairResult, resumeRepairModel, resumeMetadataOnly = savedOpts, upgrade, branch, hash, task, model, metadata
		resumeUpgradeInputPolicy = inputPolicy
	})
	for _, tc := range []struct {
		name                   string
		dry, upgrade, metadata bool
		branch, hash, repair   string
		valid                  bool
		inputPolicy            bool
	}{
		{name: "preview", dry: true, upgrade: true, valid: true},
		{name: "ordinary preflight", dry: true, valid: true},
		{name: "metadata preview", dry: true, upgrade: true, metadata: true, valid: true},
		{name: "metadata requires upgrade", metadata: true},
		{name: "input policy preview", dry: true, upgrade: true, inputPolicy: true, valid: true},
		{name: "input policy requires upgrade", inputPolicy: true},
		{name: "input policy requires plan", upgrade: true, inputPolicy: true},
		{name: "apply", upgrade: true, branch: "recovery", hash: strings.Repeat("a", 64), valid: true},
		{name: "no plan", upgrade: true, branch: "recovery"},
		{name: "no branch", upgrade: true, hash: strings.Repeat("a", 64)},
		{name: "preview cannot apply", dry: true, upgrade: true, branch: "recovery", hash: strings.Repeat("a", 64)},
		{name: "repair separate", upgrade: true, dry: true, repair: "7"},
		{name: "plan needs upgrade", branch: "recovery", hash: strings.Repeat("a", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts = runOptions{dryRun: tc.dry}
			resumeMetadataOnly = tc.metadata
			resumeUpgradeInputPolicy = tc.inputPolicy
			resumeUpgradeContracts, resumeRecoveryBranch, resumeRecoveryPlan, resumeRepairResult, resumeRepairModel = tc.upgrade, tc.branch, tc.hash, tc.repair, ""
			if err := validateResumeRepairFlags(); (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestResumeContractRecoveryPublishesChildWithoutCallingModels(t *testing.T) {
	for _, test := range []struct {
		name        string
		metadata    bool
		inputPolicy bool
		output      string
	}{
		{name: "versioned", output: "json"},
		{name: "metadata_only", metadata: true, output: "json"},
		{name: "metadata_only_text", metadata: true, output: "text"},
		{name: "input_policy", metadata: true, inputPolicy: true, output: "json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			metadata := test.metadata
			savedOpts := opts
			oldTeam, oldSearch, oldBranch, oldRun, oldTask, oldModel := resumeTeamName, resumeSearch, resumeBranch, resumeRun, resumeRepairResult, resumeRepairModel
			oldUpgrade, oldRecovery, oldPlan, oldMetadata := resumeUpgradeContracts, resumeRecoveryBranch, resumeRecoveryPlan, resumeMetadataOnly
			oldInputPolicy := resumeUpgradeInputPolicy
			t.Cleanup(func() {
				opts = savedOpts
				resumeTeamName, resumeSearch, resumeBranch, resumeRun, resumeRepairResult, resumeRepairModel = oldTeam, oldSearch, oldBranch, oldRun, oldTask, oldModel
				resumeUpgradeContracts, resumeRecoveryBranch, resumeRecoveryPlan, resumeMetadataOnly = oldUpgrade, oldRecovery, oldPlan, oldMetadata
				resumeUpgradeInputPolicy = oldInputPolicy
			})
			resumeUpgradeInputPolicy = false
			f := newVersionedCLIFixture(t)
			if metadata {
				if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".config", "hufu", "hufu.yaml"), []byte("workspace-versioning:\n  mode: off\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			f.write(t, "unchanged.txt", "retained project bytes\n")
			t.Chdir(f.subject)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				http.Error(w, "unexpected model call", 500)
			}))
			t.Cleanup(server.Close)
			teamDir := filepath.Join(f.subject, ".agent-teams", "dev")
			if err := os.MkdirAll(teamDir, 0o700); err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{
				"team.yaml":   "name: dev\nworker-model: ollama/fixture\ncoordinator-model: ollama/fixture\ndelegation:\n  bind-task-goal-contracts: true\ncontracts:\n  - id: review\n    agent: worker\n    side_effect: none\n    recovery: retry\n    result-contract: {schema: result.json, require-structured: true}\n    execution: {requires-result: true}\n",
				"worker.md":   "---\nname: worker\nrole: worker\ntools: view\nside_effect: none\n---\nInspect assigned evidence.\n",
				"result.json": `{"type":"object","required":["value"],"properties":{"value":{"type":"string"}},"additionalProperties":false}`,
			} {
				if err := os.WriteFile(filepath.Join(teamDir, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			resolverSource := filepath.Join(teamDir, "resolver", "main.go")
			if test.inputPolicy {
				config := "inputs:\n  scope:\n    type: string\n    required: true\n    default: original\n    resolver:\n      id: scope-v1\n      capability: resolve-scope\n      type: scope\n      source: invocation_prompt\n      side_effect: none\n      timeout: 5\naction-providers:\n  resolve-scope:\n    runtime: golang\n    source: ./resolver\n    mode: trusted-static\n"
				file, err := os.OpenFile(filepath.Join(teamDir, "team.yaml"), os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := file.WriteString(config)
				if err := file.Close(); err != nil || writeErr != nil {
					t.Fatal(err, writeErr)
				}
				if err := os.MkdirAll(filepath.Dir(resolverSource), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(resolverSource, []byte("package main\nimport (\"context\"; \"io\"; \"errors\")\nfunc Run(context.Context, io.Reader, io.Writer) error { return errors.New(\"resolver must not execute\") }\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			opts = runOptions{}
			if _, err := runCLI(t, "session", "fork", "--name", "source", "--workspace", f.control, "--json"); err != nil {
				t.Fatal(err)
			}
			opts = runOptions{workspace: f.control, workspaceMode: "exact", dryRun: true, canonicalRun: true, outputFormat: "json", displayMode: "plain", eventFormat: "text", executionProfile: "default", providerURL: server.URL + "/v1"}
			resumeTeamName, resumeSearch, resumeBranch, resumeRun, resumeRepairResult, resumeRepairModel = "dev", filepath.Dir(teamDir), "source", "", "", ""
			registry := team.NewTeamRegistry([]string{filepath.Dir(teamDir)})
			if err := registry.Discover(); err != nil {
				t.Fatal(err)
			}
			binding, _, err := resolveExistingExecutionWorkspace(t.Context(), "dev", true)
			if err != nil {
				t.Fatal(err)
			}
			binding, _, err = bindManagedResumeWorkspace(t.Context(), binding, "dev", true)
			if err != nil {
				t.Fatal(err)
			}
			tc, err := loadTeamByNameWithWorkspaceBinding(context.WithValue(t.Context(), contractRecoverySetupKey{}, true), "dev", binding, registry, opts.providerURL, "", newPathConsent(), nil, nil, false, false)
			if err != nil {
				t.Fatal(err)
			}
			policy := tc.coordinator.ExecutionPolicySnapshot()
			inputDefinitions := tc.session.RunInputDefinitions
			compiled, _, err := team.CompileTaskGoalContracts(tc.session, []team.TaskDef{{Agent: "worker", ContractID: "review", Goal: "Inspect original evidence"}})
			if err != nil {
				t.Fatal(err)
			}
			if err = tc.Close(); err != nil {
				t.Fatal(err)
			}
			snapshot, err := team.ResolveRunInputSnapshot(inputDefinitions, nil, "source-run", "source-inv", "dev")
			if err != nil {
				t.Fatal(err)
			}
			task := &team.TodoItem{ID: "7", Agent: "worker", Desc: compiled[0].Goal, Goal: compiled[0].Goal, Status: team.TaskBlocked, ContractID: compiled[0].ContractID, ContractHash: compiled[0].ContractHash, ContractRevision: compiled[0].ContractRevision, SideEffect: team.SideEffectNone, Recovery: team.RecoveryRetry, Execution: compiled[0].Execution, ResultContract: compiled[0].ResultContract, RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash}
			events, err := team.OpenEventStore(f.control)
			if err != nil {
				t.Fatal(err)
			}
			events.SetBranchID("source")
			for _, entry := range []struct {
				kind  string
				value any
				task  string
			}{
				{string(team.EventRunStarted), map[string]any{"team": "dev"}, ""},
				{string(team.EventExecutionPolicySnapshot), policy, ""},
				{string(team.EventRunInputsResolved), snapshot, ""},
				{string(team.EventTaskCreated), task, "7"},
				{string(team.EventWorkflowStateChanged), map[string]any{"state": "FAILED"}, ""},
			} {
				payload, err := json.Marshal(entry.value)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = events.AppendPersistedContext(t.Context(), team.RunEvent{Type: entry.kind, RunID: "source-run", Actor: "runtime", TaskID: entry.task, Payload: payload}); err != nil {
					t.Fatal(err)
				}
			}
			lineage, err := events.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			if err = team.SaveSession(f.control, team.ReduceToSessionData(lineage)); err != nil {
				t.Fatal(err)
			}
			if err = events.Close(); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(teamDir, "result.json"), []byte(`{"type":"object","description":"updated contract","required":["value"],"properties":{"value":{"type":"string"}},"additionalProperties":false}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if test.inputPolicy {
				if err := os.WriteFile(resolverSource, []byte("package main\nimport (\"context\"; \"io\"; \"errors\")\nfunc Run(context.Context, io.Reader, io.Writer) error { return errors.New(\"updated resolver must not execute\") }\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := runtimeStoreFiles(t, f.control)
			resumeUpgradeContracts, resumeMetadataOnly = true, metadata
			resumeUpgradeInputPolicy = test.inputPolicy
			resumeRecoveryBranch, resumeRecoveryPlan = "", ""
			opts.outputFormat = "json"
			cmd := &cobra.Command{Use: "resume"}
			cmd.SetContext(t.Context())
			var output bytes.Buffer
			cmd.SetOut(&output)
			if err = runResumeCommand(cmd, nil); err != nil {
				t.Fatal(err)
			}
			var plan team.ContractRecoveryPlan
			if err = json.Unmarshal(output.Bytes(), &plan); err != nil {
				t.Fatal(err, output.String())
			}
			if plan.Retried != 1 || plan.Retained != 0 || len(plan.SHA256) != 64 || plan.MetadataOnly != metadata || (plan.InputPolicyUpgrade != nil) != test.inputPolicy {
				t.Fatalf("unexpected plan: %+v", plan)
			}
			if !reflect.DeepEqual(before, runtimeStoreFiles(t, f.control)) {
				t.Fatal("preview changed durable workspace")
			}
			opts.dryRun = false
			opts.outputFormat = test.output
			resumeRecoveryBranch, resumeRecoveryPlan = "recovered", plan.SHA256
			output.Reset()
			if err = runResumeCommand(cmd, nil); err != nil {
				t.Fatal(err)
			}
			var result struct {
				Branch       string `json:"branch"`
				SourceBranch string `json:"source_branch"`
				PlanHash     string `json:"plan_hash"`
				MetadataOnly *bool  `json:"metadata_only"`
			}
			if test.output == "text" {
				if !strings.Contains(output.String(), "project files remain shared") || !strings.Contains(output.String(), `--execution-profile "default"`) || strings.Contains(output.String(), "--metadata-only") || strings.Contains(output.String(), "--upgrade-contracts") {
					t.Fatalf("continuation command lost the resume profile or retained recovery flags: %s", output.String())
				}
				result.Branch = "recovered"
			} else {
				if err = json.Unmarshal(output.Bytes(), &result); err != nil {
					t.Fatal(err, output.String())
				}
				if result.MetadataOnly == nil || *result.MetadataOnly != metadata || result.SourceBranch != plan.SourceBranch || result.PlanHash != plan.SHA256 {
					t.Fatalf("apply output lost recovery identity or shared-file semantics: %s", output.String())
				}
			}
			child, err := team.ReadResumeSource(f.control, result.Branch, "")
			if err != nil {
				t.Fatal(err)
			}
			if child.Session.Tasks[0].Status != team.TaskPending {
				t.Fatal("child did not reopen failed task")
			}
			parent, err := team.ReadResumeSource(f.control, "source", "")
			if err != nil {
				t.Fatal(err)
			}
			if parent.Session.Tasks[0].Status != team.TaskBlocked || parent.Session.ExecutionPolicySnapshot.ConfigurationHash != policy.ConfigurationHash {
				t.Fatal("apply rewrote source")
			}
			if calls.Load() != 0 {
				t.Fatal("contract recovery called a provider")
			}
			// Ordinary resume preflight must stay read-only and must not
			// require either upgrade flag after the child was published.
			resumeUpgradeContracts, resumeUpgradeInputPolicy, resumeMetadataOnly = false, false, false
			resumeRecoveryBranch, resumeRecoveryPlan, resumeBranch = "", "", result.Branch
			opts.dryRun, opts.outputFormat = true, "json"
			before = runtimeStoreFiles(t, f.control)
			output.Reset()
			if err := runResumeCommand(cmd, nil); err != nil {
				t.Fatal(err)
			}
			var readiness team.ResumePreflightAssessment
			if err := json.Unmarshal(output.Bytes(), &readiness); err != nil || readiness.Resumable != 1 || readiness.Accepted != 0 || readiness.TerminalUnresolved != 0 {
				t.Fatalf("ordinary preflight: %+v %v", readiness, err)
			}
			if !reflect.DeepEqual(before, runtimeStoreFiles(t, f.control)) || calls.Load() != 0 {
				t.Fatal("ordinary preflight changed durable state or invoked a provider")
			}
			if f.read(t, "unchanged.txt") != "retained project bytes\n" {
				t.Fatal("recovery changed project files")
			}
		})
	}
}
