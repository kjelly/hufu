package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
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

	"github.com/kjelly/hufu/internal/resultquery"
)

// This exercises Cobra, the real provider gateway, tool-free sidecar requests,
// all three model stages, and process exit/stdout framing. Replies are fixtures;
// this test does not claim to measure a real model's semantic accuracy.
func TestHistoricalResultsQueryRealBinaryWithLocalProvider(t *testing.T) {
	workspace, ids := resultsTestFixture(t)
	plan := resultquery.Plan{SchemaVersion: 1, Resolution: "resolved", Intent: "summarize", Selectors: resultquery.Selectors{Team: "review"},
		TimeRange: resultquery.TimeRange{Timezone: "UTC"}, Statuses: []string{"done"}, StatusBasis: "at_terminal", SearchTerms: []string{"timeout"}, Order: "relevance", Limit: 20}
	replies := []string{
		string(resultsTestJSON(t, plan)),
		string(resultsTestJSON(t, map[string]any{"result_ids": []string{ids[0]}})),
		string(resultsTestJSON(t, map[string]any{"blocks": []resultquery.AnswerBlock{{Kind: "fact", Text: "已保存 deadline 解法。", Sources: []string{ids[0]}}}})),
	}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_, _ = fmt.Fprint(w, `{"object":"list","data":[{"id":"qwen3:8b","object":"model","max_context_window":32768}]}`)
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Tools    []any `json:"tools"`
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		encoded, _ := json.Marshal(request)
		if len(request.Tools) != 0 || bytes.Contains(encoded, []byte("OTHER_TEAM_SENTINEL")) {
			t.Errorf("provider received unauthorized tools/team content")
			http.Error(w, "scope error", 400)
			return
		}
		index := int(calls.Add(1) - 1)
		if index >= len(replies) {
			t.Errorf("provider generation budget exceeded")
			http.Error(w, "too many calls", 400)
			return
		}
		response := map[string]any{"id": "fixture", "object": "chat.completion", "created": 1, "model": "qwen3:8b",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": replies[index]}}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}
		if err := json.MarshalWrite(w, response); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	binary := buildProcessContractBinary(t)
	home := t.TempDir()
	configuration := "provider-url: " + server.URL + "/v1\nproviders:\n  ollama:\n    provider-url: " + server.URL + "/v1\n"
	if err := os.WriteFile(filepath.Join(home, "hufu.yaml"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "results", "query", "昨天的 timeout 任務結果？", "--workspace", workspace, "--team", "review", "--model", "ollama/qwen3:8b", "--timezone", "UTC", "--output", "json")
	command.Dir = home
	command.Env = []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, "config"), "XDG_CACHE_HOME=" + filepath.Join(home, "cache"), "HUFU_STATE_HOME=" + filepath.Join(home, "state"), "PATH=" + os.Getenv("PATH"), "NO_COLOR=1", "TERM=dumb"}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("query process failed: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	var response resultsEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("stdout is not exactly one JSON document: %v\n%s", err, stdout.String())
	}
	if response.Operation != "succeeded" || response.Query == nil || len(response.Query.Blocks) != 1 || calls.Load() != 3 {
		t.Fatalf("incomplete provider query: %#v calls=%d", response, calls.Load())
	}
	if !response.Usage.Available || response.Usage.TotalTokens != 45 {
		t.Fatalf("query usage lost or mixed with old run: %#v", response.Usage)
	}
	if strings.Contains(stdout.String(), "OTHER_TEAM_SENTINEL") {
		t.Fatal("other team escaped the result scope")
	}
}
