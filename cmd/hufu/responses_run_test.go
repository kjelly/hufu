package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/scripted"
	"github.com/kjelly/hufu/internal/team"
)

func TestResponsesRunBinaryAndProtocolDrift(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	f := newVersionedCLIFixture(t)
	provider := scripted.NewProvider("test", []scripted.Step{{ToolCall: &scripted.ToolCall{Name: "finish", Arguments: `{"response":"Responses CLI verified"}`}, Usage: &scripted.Usage{InputTokens: 10, OutputTokens: 2}}})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" {
			t.Error("CLI silently changed protocol")
			http.Error(w, "wrong protocol", 400)
			return
		}
		if r.URL.Path == "/v1/responses" {
			calls.Add(1)
		}
		provider.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	configPath := filepath.Join(os.Getenv("HOME"), ".config", "hufu", "hufu.yaml")
	if err := os.WriteFile(configPath, []byte("workspace-versioning:\n  mode: off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	search := filepath.Join(f.subject, ".agent-teams")
	teamDir := filepath.Join(search, "dev")
	if err := os.MkdirAll(teamDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf("name: dev\nworker-model: ollama/test\ncoordinator-model: ollama/test\ncontext-window: 131072\ntimeout: 15\nmax-rounds: 3\nmax-retries: 0\nproviders:\n  local:\n    provider-url: %s/v1\n    api-protocol: responses\n", server.URL)
	teamPath := filepath.Join(teamDir, "team.yaml")
	if err := os.WriteFile(teamPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "worker.md"), []byte("---\nname: reviewer\nrole: worker\nmodel: ollama/test\ntools: view\n---\nReview supplied text.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := buildTestBinary(t)
	childTemp, err := os.MkdirTemp(os.TempDir(), "resp-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(childTemp); err != nil {
			t.Error(err)
		}
	})
	run := func() (jsonRunOutput, error, string) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, "run", "--team", "dev", "--agent-team-search-path", search, "--output", "json", "--unattended", "--", "Return the fixed result; task prose mentions chat-completions but cannot change transport.")
		command.Dir = f.subject
		command.Env = []string{"HOME=" + os.Getenv("HOME"), "HUFU_STATE_HOME=" + f.state, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + childTemp, "NO_COLOR=1", "TERM=dumb"}
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		var output jsonRunOutput
		if stdout.Len() > 0 {
			if decodeErr := json.Unmarshal(stdout.Bytes(), &output); decodeErr != nil {
				t.Fatalf("invalid CLI JSON: %v\n%s", decodeErr, stdout.String())
			}
		}
		return output, err, stderr.String()
	}
	output, err, diagnostic := run()
	exit, exited := errors.AsType[*exec.ExitError](err)
	// A finish response does not prove acceptance: this fixture deliberately
	// declares no verifier and must retain the ordinary unverified outcome.
	if !exited || exit.ExitCode() != 7 || output.Outcome != "unverified" || output.StopReason != string(team.StopReasonAcceptanceNotSet) || output.GoalSatisfied || calls.Load() != 1 {
		t.Fatalf("Responses CLI: %v calls=%d outcome=%s result=%s\n%s", err, calls.Load(), output.Outcome, output.Result, diagnostic)
	}
	if err := os.WriteFile(teamPath, []byte(strings.Replace(manifest, "api-protocol: responses", "api-protocol: chat-completions", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err, diagnostic = run()
	if err == nil || calls.Load() != 1 || !strings.Contains(diagnostic, "execution policy") {
		t.Fatalf("protocol drift reached generation: err=%v calls=%d\n%s", err, calls.Load(), diagnostic)
	}
}
