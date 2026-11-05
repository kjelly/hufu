package team

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/providerproxy"
)

func TestNativeImagePreviewHasNoDurableOrProviderSideEffects(t *testing.T) {
	for _, scenario := range []string{"repeated images", "invalid image", "missing image", "too many", "byte limit", "denied target", "coordinator", "unknown worker"} {
		t.Run(scenario, func(t *testing.T) {
			var requests, boundaries atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				http.Error(w, "unexpected preview request", http.StatusBadRequest)
			}))
			t.Cleanup(server.Close)
			factory := func(context.Context, string, providerproxy.Config) (providerproxy.Boundary, error) {
				boundaries.Add(1)
				return nil, fmt.Errorf("preview cannot start a provider boundary")
			}
			limits := media.Limits{}
			if scenario == "byte limit" {
				limits.MaxBytesPerImage = 16
			}
			c := newMediaPolicyCoordinatorWithTransport(t, server.URL+"/v1", factory, providerintrospection.CapabilityUnknown, func(session *TeamSession) {
				for name, role := range map[string]string{"denied": "worker", "coordinator": "coordinator"} {
					def := cloneAgentDef(session.Agents["worker"])
					def.Name, def.Role = name, role
					if name == "denied" {
						def.Generation.Model = "fixture/text"
					}
					session.Agents[name] = def
				}
			}, limits)
			path := filepath.Join(c.projectDir, "input.png")
			data := mediaStorePNG(t)
			if scenario == "invalid image" {
				data = []byte("not an image")
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			input := MediaTurnRequest{Images: []string{path, path}}
			worker := "worker"
			switch scenario {
			case "missing image":
				input.Images = []string{filepath.Join(c.projectDir, "missing.png")}
			case "too many":
				input.Images = make([]string, media.MaxCount+1)
			case "denied target":
				worker = "denied"
			case "coordinator":
				worker = "coordinator"
			case "unknown worker":
				worker = "unknown"
			}
			before := mediaPreviewFiles(t, c.session.Dir)
			runID, invocationID := c.executionRunID, c.currentInvocationID()
			preview, err := c.PreviewNativeImageTask(t.Context(), worker, "Compare the images.", input)
			if scenario == "repeated images" {
				if err != nil || preview.ImageCount != 2 || preview.TotalBytes != 2*int64(len(data)) || preview.Target.Model != "vision" || preview.ModelImageCapability != providerintrospection.CapabilityUnknown {
					t.Fatalf("preview lost local input or claimed capability: %+v err=%v", preview, err)
				}
			} else if err == nil {
				t.Fatal("invalid preview was accepted")
			}
			if requests.Load() != 0 || boundaries.Load() != 0 || c.executionRunID != runID || c.currentInvocationID() != invocationID || len(c.TaskTracker().TodoList().Items()) != 0 || len(c.conversationHistory) != 0 {
				t.Fatal("preview entered execution or changed conversation state")
			}
			if after := mediaPreviewFiles(t, c.session.Dir); !reflect.DeepEqual(before, after) {
				t.Fatal("preview wrote CAS, events or other workspace files")
			}
		})
	}
}

func mediaPreviewFiles(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	files := make(map[string][32]byte)
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[path] = sha256.Sum256(data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files
}
