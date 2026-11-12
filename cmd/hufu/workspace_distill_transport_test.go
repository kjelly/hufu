package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/scripted"
	"github.com/kjelly/hufu/internal/taskdistill"
	"github.com/kjelly/hufu/internal/team"
	"github.com/spf13/cobra"
)

func TestDistillationTransportRecoveryAndWireBudget(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		closed                      bool
		failures, budget, wantCalls int
		want                        error
		longBody                    bool
	}{
		{"closed then complete", true, 1, 2, 2, nil, false},
		{"body closes after downstream headers", true, 1, 2, 2, nil, true},
		{"persistent closure bounded", true, 10, 6, 2, taskdistill.ErrTransport, false},
		{"closed without remaining budget", true, 1, 1, 1, taskdistill.ErrCallBudget, false},
		{"unavailable without remaining budget", false, 1, 1, 1, taskdistill.ErrCallBudget, false},
		{"unavailable then complete", false, 1, 2, 2, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := opts
			t.Cleanup(func() { opts = old })
			opts = runOptions{}
			t.Setenv("HOME", t.TempDir())
			project := t.TempDir()
			t.Chdir(project)
			var calls atomic.Int32
			provider := scripted.NewProvider("maintenance-model", []scripted.Step{{Content: `{"decisions":[{"source_id":"source","action":"discard","reason":"transient"}],"knowledge":[]}`}})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" && int(calls.Add(1)) <= tc.failures {
					if tc.closed {
						prefix := "{"
						if tc.longBody {
							prefix = strings.Repeat(" ", 9000) + prefix
						}
						w.Header().Set("Content-Length", fmt.Sprint(len(prefix)+1024))
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						_, _ = fmt.Fprint(w, prefix)
						w.(http.Flusher).Flush()
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = connection.Close()
					} else {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = fmt.Fprint(w, `{"error":{"message":"fixture unavailable","type":"server_error"}}`)
					}
					return
				}
				provider.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			configuration := fmt.Sprintf("backends:\n  fixture:\n    kind: llm\n    type: openai-compatible\n    base-url: %s/v1\n", server.URL)
			if err := os.WriteFile(filepath.Join(project, "hufu.yaml"), []byte(configuration), 0o600); err != nil {
				t.Fatal(err)
			}
			dir := helperSetupTeam(t, filepath.Join(project, ".agent-teams"), "maintenance-fixture")
			if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: maintenance-fixture\ncontext-window: 131072\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			team.RegisterConfiguredContextWindow([]string{"fixture/maintenance-model", "maintenance-model"}, 131072)
			ctx := agent.WithProviderCallBudget(t.Context(), tc.budget)
			generator, _, closeGenerator, err := taskDistillationGeneratorFactory(ctx, dir, filepath.Join(project, "audit"), "fixture/maintenance-model")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(closeGenerator)
			draft, err := taskdistill.GenerateBatched(ctx, distillationProgressGenerator{generator, &cobra.Command{}, false}, []taskdistill.Source{{ID: "source", Content: "public evidence"}}, "", tc.budget)
			if int(calls.Load()) != tc.wantCalls || tc.want != nil && (!errors.Is(err, tc.want) || len(draft.Decisions) != 0) || tc.want == nil && (err != nil || len(draft.Decisions) != 1) {
				t.Fatalf("calls=%d want=%d expected_error=%v err=%v", calls.Load(), tc.wantCalls, tc.want, err)
			}
		})
	}
}
