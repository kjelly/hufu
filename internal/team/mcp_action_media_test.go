package team

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/providerintrospection"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func TestMCPActionPublishesImagesAfterOutputsValidation(t *testing.T) {
	for _, scenario := range []string{"structured images", "image only", "schema rejection", "invalid image"} {
		t.Run(scenario, func(t *testing.T) {
			image := base64.StdEncoding.EncodeToString(mediaStorePNG(t))
			fake := newMCPActionFake(collectDebugTool("Capture images."))
			fake.setHandler(func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				data := image
				if scenario == "invalid image" {
					data = "invalid"
				}
				result := &mcpgo.CallToolResult{Content: []mcpgo.Content{mcpgo.NewTextContent("before"), mcpgo.NewImageContent(data, "image/png"), mcpgo.NewTextContent("after"), mcpgo.NewImageContent(data, "image/png")},
					StructuredContent: map[string]any{"summary": "created", "count": 2}}
				if scenario == "image only" || scenario == "schema rejection" {
					result.Content, result.StructuredContent = []mcpgo.Content{result.Content[1], result.Content[3]}, nil
				}
				return result, nil
			})
			manager := newMCPActionManager(t, fake)
			teamDir := writeMCPActionCatalogTeam(t, mcpActionTestManifest+actionCatalogTestEntry)
			loaded, err := LoadTeam(teamDir, nil, nil, DefaultProviderRegistry)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "schema rejection" {
				loaded.ActionCatalog.Entries[0].OutputSchema = nil
				if err := loaded.ActionCatalog.rehash(); err != nil {
					t.Fatal(err)
				}
			}
			c := newMediaPolicyCoordinatorWithMCP(t, manager, "http://fixture.invalid/v1", nil, providerintrospection.CapabilityUnknown, func(session *TeamSession) {
				session.ActionCatalog, session.ProviderRegistry = loaded.ActionCatalog, loaded.ProviderRegistry
				session.Config.ActionProviders, session.MCPServers = loaded.Config.ActionProviders, loaded.MCPServers
				for name, def := range loaded.Agents {
					session.Agents[name] = def
				}
			})
			entry := c.session.ActionCatalog.Entries[0]
			const payload = `{"service":"api"}`
			task := TaskDef{Agent: entry.Agent, Goal: "Capture images.", SideEffect: entry.SideEffect, Recovery: entry.Recovery,
				Action: &Action{Capability: entry.Capability, Type: entry.Type, Payload: payload},
				CatalogAction: &CatalogActionBinding{ActionID: entry.ID, EntryHash: entry.Hash, CatalogHash: c.session.ActionCatalog.Hash,
					ArgumentsHash: runInputHash([]byte(payload)), InvocationID: "tai-media-fixture"}}
			_, runErr := c.RunTask(t.Context(), task)
			items := c.TaskTracker().TodoList().Items()
			if len(items) != 1 || fake.calls.Load() != 1 {
				t.Fatalf("native image action did not execute exactly once: items=%d RPC=%d err=%v", len(items), fake.calls.Load(), runErr)
			}
			item := items[0]
			if scenario == "schema rejection" || scenario == "invalid image" {
				if item.Status == TaskDone || item.TypedResult != nil && item.TypedResult.RuntimeMedia != nil {
					t.Fatal("rejected native action advertised canonical image results")
				}
				events, err := readValidatedEventJournal(t.Context(), c.session.Workspace)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range events {
					if event.Type == string(EventToolMediaPublished) {
						t.Fatal("invalid native result published an image grant")
					}
				}
				if scenario == "schema rejection" {
					store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.projectDir)
					if err == nil {
						if refs, _ := store.ListByTask(t.Context(), item.ID); len(refs) != 0 {
							t.Fatal("output schema rejection ingested image artifacts")
						}
					}
				}
				return
			}
			if item.Status != TaskDone || item.TypedResult == nil || item.TypedResult.RuntimeMedia == nil {
				t.Fatalf("native image action lost its result: status=%s err=%v", item.Status, runErr)
			}
			result := item.TypedResult
			if err := result.RuntimeMedia.Validate(); err != nil {
				t.Fatal(err)
			}
			if item.ExecutionReceipt == nil || item.ExecutionReceipt.RuntimeMediaHash != result.RuntimeMedia.Hash {
				t.Fatal("native image result lost its execution receipt binding")
			}
			if receipt := readRuntimeActionReceipt(t, c.session.Workspace); receipt.RuntimeMediaHash != result.RuntimeMedia.Hash {
				t.Fatal("native action receipt lost its image result hash")
			}
			parts := 4
			if scenario == "image only" {
				parts = 2
			}
			if len(result.Artifacts) != 1 || len(result.RuntimeMedia.Sources) != 1 || len(result.RuntimeMedia.Sources[0].Parts) != parts {
				t.Fatal("native result lost CAS deduplication or ordered media blocks")
			}
			if scenario == "image only" && len(result.RuntimeOutputs) != 0 || scenario == "structured images" && result.RuntimeOutputs["summary"] != "created" {
				t.Fatal("media publication rewrote native Outputs")
			}
			events, err := readValidatedEventJournal(t.Context(), c.session.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
				t.Fatal(err)
			}
			clone := cloneTaskResult(result)
			clone.RuntimeMedia.Sources[0].Parts[0].Type = "invalid"
			if reflect.DeepEqual(clone.RuntimeMedia, result.RuntimeMedia) || result.RuntimeMedia.Validate() != nil {
				t.Fatal("result clone shares mutable runtime media state")
			}
			if filepath.IsAbs(result.Artifacts[0].Path) {
				t.Fatal("native media result exposed its private CAS filesystem path")
			}
			store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.projectDir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Resolve(t.Context(), result.Artifacts[0]); err != nil {
				t.Fatal("runtime media artifact is incompatible with the canonical resolver", err)
			}
			testRuntimeMediaResultInputs(t, c, item)
		})
	}
}
