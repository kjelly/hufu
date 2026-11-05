package main

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestNativeImageDryRunDoesNotCreateRuntimeStateOrContactProvider(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "dry-run must remain offline", http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	homeYAML := fmt.Sprintf(`provider-url: %s/v1
media:
  enabled: true
  allowed-targets:
    - backend: ollama
      provider-key: local
      model: vision
      image-token-upper-bound: 4096
`, server.URL)
	teamYAML := `apiVersion: hufu.io/v1alpha1
kind: AgentTeam
metadata:
  name: image-preview
spec:
  worker-model: ollama/vision
  coordinator-model: ollama/vision
`
	tc, stateRoot := loadDryRunFixtureTeam(t, "image-preview", teamYAML, homeYAML, "")
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "a,b.png")
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	opts.imagePaths, opts.agentTeamName = []string{path, path}, "image-preview"
	for _, segments := range [][]team.PromptSegment{
		{{Type: team.SegmentSwitchTeam, Name: "image-preview"}, {Type: team.SegmentInvokeAgent, Name: "analyst", Content: "Describe these images."}},
		{{Type: team.SegmentSwitchTeam, Name: "image-preview", Content: "Describe these images."}},
		{{Type: team.SegmentSwitchTeam, Name: "image-preview"}, {Type: team.SegmentText, Content: "Describe these images."}},
	} {
		if err := executeNativeImageDryRun(t.Context(), segments, map[string]*teamContext{"image-preview": tc}); err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 0 || len(tc.coordinator.TaskTracker().TodoList().Items()) != 0 {
		t.Fatal("image preview entered a provider or task execution")
	}
	if _, err := os.Stat(stateRoot); !os.IsNotExist(err) {
		t.Fatalf("image preview created managed runtime state: %v", err)
	}
}
