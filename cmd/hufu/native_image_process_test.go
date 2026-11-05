package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"image"
	"image/png"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

type nativeCLIRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string         `json:"role"`
		Content jsontext.Value `json:"content"`
	} `json:"messages"`
	Tools []jsontext.Value `json:"tools"`
}

type nativeCLIProcessFixture struct {
	binary, home, project, state, search, workspace string
	images                                          [][]byte
	paths                                           []string
}

func newNativeCLIProcessFixture(t *testing.T, binary, endpoint, acceptance string) nativeCLIProcessFixture {
	t.Helper()
	root := t.TempDir()
	home, project, search := filepath.Join(root, "home"), filepath.Join(root, "project"), filepath.Join(root, "teams")
	teamDir := filepath.Join(search, "native-cli")
	for _, dir := range []string{filepath.Join(home, ".config", "hufu"), project, teamDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configuration := "workspace-versioning:\n  mode: off\nmedia:\n  enabled: true\n  allowed-targets:\n    - backend: ollama\n      provider-key: local\n      model: vision\n      image-token-upper-bound: 4096\n"
	manifest := fmt.Sprintf("name: native-cli\nworker-model: ollama/vision\ncoordinator-model: ollama/text\nprovider-url: %s/v1\ncontext-window: 32768\nmax-tokens: 1024\nresult-transport: tool\ngoal-mode: exploratory\nmax-retries: 0\nmax-rounds: 2\ntimeout: 20\n", endpoint)
	if acceptance != "" {
		manifest += fmt.Sprintf("acceptance: %q\n", acceptance)
	}
	files := map[string]string{
		filepath.Join(home, ".config", "hufu", "hufu.yaml"): configuration,
		filepath.Join(teamDir, "team.yaml"):                 manifest,
		filepath.Join(teamDir, "worker.md"):                 "---\nname: worker\nrole: worker\nmodel: ollama/vision\ntools: submit_result\nresult-transport: tool\nmemory: {mode: off}\n---\nCompare the supplied images and submit the observations.\n",
		filepath.Join(teamDir, "coordinator.md"):            "---\nname: coordinator\nrole: coordinator\nmodel: ollama/text\n---\nPresent accepted observations.\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var imageBytes [][]byte
	var paths []string
	for index, size := range []image.Point{{X: 2, Y: 3}, {X: 3, Y: 2}} {
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, image.NewRGBA(image.Rectangle{Max: size})); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, fmt.Sprintf("image,%d.png", index))
		if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		imageBytes, paths = append(imageBytes, encoded.Bytes()), append(paths, path)
	}
	return nativeCLIProcessFixture{binary: binary, home: home, project: project, state: filepath.Join(root, "state"), search: search, workspace: filepath.Join(root, "workspace"), images: imageBytes, paths: paths}
}

func (f nativeCLIProcessFixture) args(facade, goal string, extra ...string) []string {
	var args []string
	if facade == "legacy" {
		args = []string{"--agent-team", "native-cli"}
	} else {
		args = []string{"run", "--team", "native-cli"}
	}
	args = append(args, "--agent-team-search-path", f.search, "--workspace", f.workspace, "--context-window", "32768", "--output", "json", "--no-spinner", "--no-color", "--image", f.paths[0], "--image", f.paths[1], "--image", f.paths[0])
	args = append(args, extra...)
	return append(args, "--", goal)
}

func (f nativeCLIProcessFixture) run(t *testing.T, args []string) ([]byte, error, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, f.binary, args...)
	command.Dir = f.project
	command.Env = []string{"HOME=" + f.home, "XDG_CONFIG_HOME=" + filepath.Join(f.home, ".config"), "HUFU_STATE_HOME=" + f.state, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + os.TempDir(), "GOMAXPROCS=2", "NO_COLOR=1", "TERM=dumb"}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if ctx.Err() != nil {
		t.Fatalf("native CLI timed out: %s", stderr.String())
	}
	return stdout.Bytes(), err, stderr.String()
}

func TestNativeImageCLIUsesOrderedImagesWithoutReplanning(t *testing.T) {
	binary := buildTestBinary(t)
	for _, facade := range []string{"canonical", "legacy", "failed acceptance", "accepted"} {
		t.Run(facade, func(t *testing.T) {
			requests := make(chan nativeCLIRequest, 8)
			server := nativeCLIProvider(t, requests)
			acceptance := ""
			switch facade {
			case "failed acceptance":
				acceptance = "false"
			case "accepted":
				acceptance = "true"
			}
			f := newNativeCLIProcessFixture(t, binary, server.URL, acceptance)
			assertNativeCLIRejectsUnsupportedScopes(t, f, facade, requests)
			stdout, runErr, diagnostic := f.run(t, f.args(facade, "@worker Compare the images."))
			var output jsonRunOutput
			accepted := facade == "accepted"
			if err := json.Unmarshal(stdout, &output); err != nil || (runErr == nil) != accepted || output.GoalSatisfied != accepted || !strings.Contains(output.Result, "CLI formatted comparison.") || len(requests) != 2 {
				t.Fatalf("native CLI outcome: decode=%v run=%v calls=%d output=%s\n%s", err, runErr, len(requests), stdout, diagnostic)
			}
			if facade == "failed acceptance" {
				if output.Acceptance == nil || output.Acceptance.EffectiveState() != team.AcceptanceFailed || team.IsRunOutcomeSuccess(team.RunOutcome(output.Outcome)) {
					t.Fatal("native presentation erased failed acceptance", output)
				}
			} else if accepted {
				if output.Acceptance == nil || output.Acceptance.EffectiveState() != team.AcceptancePassed || !team.IsRunOutcomeSuccess(team.RunOutcome(output.Outcome)) {
					t.Fatal("native presentation lost verified acceptance", output)
				}
			} else if output.Outcome != string(team.RunOutcomeUnverified) {
				t.Fatal("native presentation invented acceptance", output)
			}
			worker, formatter := <-requests, <-requests
			assertNativeCLIImages(t, worker, [][]byte{f.images[0], f.images[1], f.images[0]})
			formatted, err := json.Marshal(formatter.Messages)
			if err != nil || formatter.Model != "text" || len(formatter.Tools) != 0 || bytes.Contains(formatted, []byte("image_url")) || bytes.Contains(formatted, []byte("base64,")) {
				t.Fatal("CLI formatter received image data or tools", err)
			}
			assertNativeCLIDurableEvidence(t, f.workspace, f.paths)
		})
	}
}

func assertNativeCLIRejectsUnsupportedScopes(t *testing.T, f nativeCLIProcessFixture, facade string, requests chan nativeCLIRequest) {
	t.Helper()
	for _, goal := range []string{"@worker Compare the images. @worker Repeat the comparison."} {
		_, err, diagnostic := f.run(t, f.args(facade, goal))
		if err == nil || len(requests) != 0 {
			t.Fatalf("unsupported image scope entered a model: %v calls=%d\n%s", err, len(requests), diagnostic)
		}
	}
	_, err, diagnostic := f.run(t, f.args(facade, "@worker Compare the images.", "--auto-team"))
	if err == nil || len(requests) != 0 {
		t.Fatalf("automatic image team selection entered a model: %v calls=%d\n%s", err, len(requests), diagnostic)
	}
}

func assertNativeCLIDurableEvidence(t *testing.T, workspace string, paths []string) {
	t.Helper()
	var stores []string
	if err := filepath.WalkDir(workspace, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && entry.Name() == "runtime.sqlite" {
			stores = append(stores, filepath.Dir(path))
		}
		return err
	}); err != nil || len(stores) != 1 {
		t.Fatal("CLI did not preserve exactly one workspace", stores, err)
	}
	store, err := team.OpenEventStoreReadOnly(stores[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	runs, grants := 0, 0
	for _, event := range events {
		if event.Type == string(team.EventRunStarted) {
			runs++
		}
		if event.Type == string(team.EventMediaProviderAdmitted) {
			grants++
		}
		for _, path := range paths {
			if bytes.Contains(event.Payload, []byte(path)) {
				t.Fatal("CLI image flag paths leaked into durable events")
			}
		}
	}
	if runs != 1 || grants != 1 {
		t.Fatal("CLI repeated image work or created a new acceptance run")
	}
	if err := team.VerifyNativeMediaLineage(t.Context(), stores[0], events); err != nil {
		t.Fatal(err)
	}
}

func nativeCLIProvider(t *testing.T, requests chan nativeCLIRequest) *httptest.Server {
	t.Helper()
	return nativeCLIProviderWithResponder(t, requests, nil)
}

func nativeCLIProviderWithResponder(t *testing.T, requests chan nativeCLIRequest, respond func(http.ResponseWriter, nativeCLIRequest)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/show":
			var query struct {
				Model string `json:"model"`
			}
			_ = json.UnmarshalRead(io.LimitReader(r.Body, 1<<16), &query)
			capabilities := []string{"completion", "tools"}
			if query.Model == "vision" {
				capabilities = append(capabilities, "vision")
			}
			_ = json.MarshalWrite(w, map[string]any{"model_info": map[string]any{"llama.context_length": 32768}, "capabilities": capabilities, "supports_vision": query.Model == "vision"})
			return
		case "/api/ps":
			_, _ = io.WriteString(w, `{"models":[]}`)
			return
		case "/api/tags":
			_, _ = io.WriteString(w, `{"models":[{"name":"vision"},{"name":"text"}]}`)
			return
		case "/v1/models":
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"vision","object":"model"},{"id":"text","object":"model"}]}`)
			return
		case "/v1/chat/completions":
		default:
			http.NotFound(w, r)
			return
		}
		var request nativeCLIRequest
		if err := json.UnmarshalRead(io.LimitReader(r.Body, 1<<20), &request); err != nil {
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		select {
		case requests <- request:
		default:
			http.Error(w, "unexpected extra invocation", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if respond != nil {
			respond(w, request)
			return
		}
		if request.Model == "vision" {
			arguments := `{"status":"success","summary":"CLI worker comparison.","details":"Canonical CLI image observations."}`
			fmt.Fprintf(w, "data: {\"id\":\"cli-worker\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"vision\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"submit-cli\",\"type\":\"function\",\"function\":{\"name\":\"submit_result\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"cli-worker\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"vision\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", arguments)
			return
		}
		_, _ = io.WriteString(w, "data: {\"id\":\"cli-text\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"text\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"CLI formatted comparison.\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"cli-text\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"text\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server
}

func assertNativeCLIImages(t *testing.T, request nativeCLIRequest, expected [][]byte) {
	t.Helper()
	var images [][]byte
	for _, message := range request.Messages {
		if len(message.Content) == 0 || message.Content[0] != '[' {
			continue
		}
		var parts []struct {
			Type     string `json:"type"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}
		if err := json.Unmarshal(message.Content, &parts); err != nil {
			t.Fatal(err)
		}
		for _, part := range parts {
			if part.Type != "image_url" {
				continue
			}
			encoded, found := strings.CutPrefix(part.ImageURL.URL, "data:image/png;base64,")
			data, err := base64.StdEncoding.DecodeString(encoded)
			if message.Role != "user" || !found || err != nil {
				t.Fatal("CLI changed an image role or MIME encoding")
			}
			images = append(images, data)
		}
	}
	if request.Model != "vision" || len(images) != len(expected) {
		t.Fatal("CLI lost image occurrences")
	}
	for index := range expected {
		if !bytes.Equal(images[index], expected[index]) {
			t.Fatal("CLI changed image order, duplicate occurrences or bytes")
		}
	}
}
