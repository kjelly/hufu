package main

import (
	"bytes"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/scripted"
	"github.com/kjelly/hufu/internal/team"
)

func TestMaintenanceGeneratorDoesNotRequireWorkerMCP(t *testing.T) {
	for _, scenario := range []string{"worker_policy", "action_provider", "action_provider_with_input", "invalid_input", "invalid_policy"} {
		t.Run(scenario, func(t *testing.T) {
			originalOpts := opts
			t.Cleanup(func() { opts = originalOpts })
			opts = runOptions{}
			t.Setenv("HOME", t.TempDir())
			project := t.TempDir()
			t.Chdir(project)
			var calls, toolRequests atomic.Int32
			provider := scripted.NewProvider("maintenance-model", []scripted.Step{{Content: "review complete"}})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" {
					calls.Add(1)
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					var request struct {
						Tools []any `json:"tools"`
					}
					if err := json.Unmarshal(raw, &request); err != nil || len(request.Tools) != 0 {
						toolRequests.Add(1)
					}
					r.Body = io.NopCloser(bytes.NewReader(raw))
				}
				provider.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			configuration := fmt.Sprintf("backends:\n  fixture:\n    kind: llm\n    type: openai-compatible\n    base-url: %s/v1\n", server.URL)
			if err := os.WriteFile(filepath.Join(project, "hufu.yaml"), []byte(configuration), 0o600); err != nil {
				t.Fatal(err)
			}
			dir := helperSetupTeam(t, filepath.Join(project, ".agent-teams"), "maintenance-fixture")
			manifest := `name: maintenance-fixture
coordinator-model: fixture/maintenance-model
worker-model: fixture/maintenance-model
context-window: 131072
mcp-servers:
  documents:
    type: local
    command: [this-fixture-mcp-must-never-start]
    allowedTools: [read_document]
`
			if scenario == "action_provider" || scenario == "action_provider_with_input" || scenario == "invalid_input" {
				manifest += `action-providers:
  documents:
    runtime: mcp
    server: documents
    tool: read_document
`
			} else {
				manifest += `    toolPolicies:
      read_document:
        sideEffect: none
        filesystem: none
        inputSchema: {type: object, additionalProperties: false, properties: {}}
`
				if scenario == "invalid_policy" {
					manifest = strings.Replace(manifest, "sideEffect: none", "sideEffect: invalid", 1)
				}
			}
			if scenario == "action_provider_with_input" || scenario == "invalid_input" {
				manifest += `  resolve-scope:
    runtime: golang
    source: ./resolver
    mode: trusted-static
inputs:
  scope:
    type: object
    properties: {}
    additional-properties: false
    resolver:
      id: fixture-scope-v1
      capability: resolve-scope
      type: resolve_scope
      source: invocation_prompt
      side_effect: none
      timeout: 5
`
				if err := os.MkdirAll(filepath.Join(dir, "resolver"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "resolver", "main.go"), []byte("package main\nimport (\"context\"; \"io\"; \"errors\")\nfunc Run(context.Context, io.Reader, io.Writer) error { return errors.New(\"resolver must not execute\") }\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if scenario == "invalid_input" {
					manifest = strings.Replace(manifest, "side_effect: none", "side_effect: invalid", 1)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			team.RegisterConfiguredContextWindow([]string{"fixture/maintenance-model", "maintenance-model"}, 131072)
			generator, _, release, err := taskDistillationGeneratorFactory(t.Context(), dir, filepath.Join(project, "maintenance-audit"), "fixture/maintenance-model")
			if scenario == "invalid_policy" || scenario == "invalid_input" {
				if release != nil {
					release()
				}
				if err == nil || calls.Load() != 0 {
					t.Fatalf("invalid configuration admitted: calls=%d err=%v", calls.Load(), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(release)
			text, err := generator.GenerateText(t.Context(), "Review the provided evidence without tools.")
			if err != nil || text != "review complete" || calls.Load() != 1 || toolRequests.Load() != 0 {
				t.Fatalf("text=%q calls=%d tool_requests=%d err=%v", text, calls.Load(), toolRequests.Load(), err)
			}
			checkpoint, exists, err := team.LoadSessionReadOnly(filepath.Join(project, "maintenance-audit"))
			if err != nil || !exists || checkpoint.ExecutionPolicySnapshot == nil {
				t.Fatalf("missing maintenance policy: exists=%v err=%v", exists, err)
			}
			policy := checkpoint.ExecutionPolicySnapshot
			if len(policy.MCPWorkerPolicies) != 0 || len(policy.MCPActionProviders) != 0 || policy.ActionCatalogHash != "" || policy.RunInputSchemaHash != "" || policy.RunInputPolicyHash != "" {
				t.Fatal("maintenance policy advertises unreachable worker capabilities")
			}
			// Loading and starting a regular team still requires authentic live
			// tools. Maintenance must not mutate authored configuration or relax
			// the ordinary coordinator's startup gate.
			session, err := team.LoadTeam(dir, nil, nil, team.DefaultProviderRegistry)
			if err != nil {
				t.Fatal(err)
			}
			session.Workspace = t.TempDir()
			coordinator, err := team.NewCoordinator(session, server.URL+"/v1", "", nil, nil, team.RoleModels{}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
			if coordinator != nil {
				_ = coordinator.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "MCP") {
				t.Fatalf("regular coordinator accepted missing MCP tools: %v", err)
			}
		})
	}
}
