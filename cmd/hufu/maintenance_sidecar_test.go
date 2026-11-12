package main

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/scripted"
	"github.com/kjelly/hufu/internal/taskdistill"
	"github.com/kjelly/hufu/internal/team"
	"github.com/spf13/cobra"
)

func TestMaintenanceGeneratorAdmitsConfiguredRoutesAndDistillationBatches(t *testing.T) {
	for _, tc := range []struct {
		name, routes, errorCode string
		truncated               bool
		repair                  string
	}{
		{"configured", "execution-routes:\n  maintenance-route:\n    candidates: [fixture/worker]\n", "", false, ""},
		{"truncated", "execution-routes:\n  maintenance-route:\n    candidates: [fixture/worker]\n", "", true, ""},
		{"truncated then wrong source", "execution-routes:\n  maintenance-route:\n    candidates: [fixture/worker]\n", "", true, "references"},
		{"truncated then blank rationale", "execution-routes:\n  maintenance-route:\n    candidates: [fixture/worker]\n", "", true, "rationale"},
		{"duplicate knowledge", "execution-routes:\n  maintenance-route:\n    candidates: [fixture/worker]\n", "", true, "duplicates"},
		{"schema violation", "execution-routes:\n  maintenance-route:\n    candidates: [fixture/worker]\n", "", true, "schema"},
		{"overlong knowledge", "execution-routes:\n  maintenance-route:\n    candidates: [fixture/worker]\n", "", true, "length"},
		{"sensitive knowledge", "execution-routes:\n  maintenance-route:\n    candidates: [fixture/worker]\n", "", true, "sensitive"},
		{"retained citation", "execution-routes:\n  maintenance-route:\n    candidates: [fixture/worker]\n", "", true, "citation"},
		{"missing", "", "execution route", false, ""},
		{"invalid", "execution-routes:\n  maintenance-route:\n    candidates: [fixture/worker, fixture/worker]\n    fallback-on: [rate_limited]\n", "execution_route_invalid", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalOpts := opts
			t.Cleanup(func() { opts = originalOpts })
			opts = runOptions{}
			t.Setenv("HOME", t.TempDir())
			project := t.TempDir()
			t.Chdir(project)
			var calls, wrongModel, truncatedBatch atomic.Int32
			steps := maintenanceDistillationReplies(tc.truncated, tc.repair)
			provider := scripted.NewProvider("maintenance-model", steps)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" {
					calls.Add(1)
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						http.Error(w, "request body unavailable", http.StatusBadRequest)
						return
					}
					var request struct {
						Model           string `json:"model"`
						ReasoningEffort string `json:"reasoning_effort"`
					}
					if json.Unmarshal(raw, &request) != nil || request.Model != "maintenance-model" || request.ReasoningEffort != "none" {
						wrongModel.Add(1)
					}
					minimum := taskdistill.MaxInputBytes / 2
					if bytes.Contains(raw, []byte("fragment_0_0/")) {
						minimum /= 2
					}
					if !bytes.Contains(raw, bytes.Repeat([]byte("e"), minimum)) {
						truncatedBatch.Add(1)
					}
					r.Body = io.NopCloser(bytes.NewReader(raw))
					if tc.truncated && calls.Load() == 1 {
						recorder := httptest.NewRecorder()
						provider.ServeHTTP(recorder, r)
						for key, values := range recorder.Header() {
							w.Header()[key] = values
						}
						w.WriteHeader(recorder.Code)
						_, _ = w.Write(bytes.ReplaceAll(recorder.Body.Bytes(), []byte(`"finish_reason":"stop"`), []byte(`"finish_reason":"length"`)))
						return
					}
				}
				provider.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			configuration := fmt.Sprintf("backends:\n  fixture:\n    kind: llm\n    type: openai-compatible\n    base-url: %s/v1\n", server.URL) + tc.routes
			if err := os.WriteFile(filepath.Join(project, "hufu.yaml"), []byte(configuration), 0o600); err != nil {
				t.Fatal(err)
			}
			dir := helperSetupTeam(t, filepath.Join(project, ".agent-teams"), "maintenance-fixture")
			manifest := "name: maintenance-fixture\ncoordinator-model: fixture/coordinator\nexecution-route: maintenance-route\ncontext-window: 131072\n"
			if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			team.RegisterConfiguredContextWindow([]string{"fixture/maintenance-model", "maintenance-model"}, 131072)
			workspace := filepath.Join(project, "maintenance-audit")
			firstRoot := runtimeSubjectRoot()
			generator, model, release, err := taskDistillationGeneratorFactory(t.Context(), dir, workspace, "fixture/maintenance-model")
			if tc.errorCode != "" {
				if release != nil {
					release()
				}
				if err == nil || !strings.Contains(err.Error(), tc.errorCode) || calls.Load() != 0 {
					t.Fatalf("invalid route admitted: calls=%d err=%v", calls.Load(), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(release)
			second, _, closeSecond, err := taskDistillationGeneratorFactory(t.Context(), dir, filepath.Join(project, "another-audit"), "fixture/maintenance-model")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(closeSecond)
			firstIdentity := generator.(interface{ CheckpointIdentity() string }).CheckpointIdentity()
			if firstIdentity == "" || firstIdentity != second.(interface{ CheckpointIdentity() string }).CheckpointIdentity() {
				firstState, _, firstErr := team.LoadSessionReadOnly(workspace)
				secondState, _, secondErr := team.LoadSessionReadOnly(filepath.Join(project, "another-audit"))
				if firstErr == nil && secondErr == nil && firstState != nil && secondState != nil {
					firstPolicy, _ := json.Marshal(firstState.ExecutionPolicySnapshot)
					secondPolicy, _ := json.Marshal(secondState.ExecutionPolicySnapshot)
					t.Logf("first fixture policy: %s\nsecond fixture policy: %s", firstPolicy, secondPolicy)
				}
				t.Fatalf("admitted checkpoint identity changed with the audit workspace: first root=%q current root=%q expected hash=%s", firstRoot, runtimeSubjectRoot(), team.ExecutionPolicyProjectRootHash(firstRoot))
			}
			if model != "fixture/maintenance-model" {
				t.Fatalf("override resolved to %q", model)
			}
			// Exercise the actual sidecar profile and provider boundary with a
			// batch larger than the generic compactor's 32,000-rune envelope.
			sources := []taskdistill.Source{{ID: "evidence", Content: strings.Repeat("e", taskdistill.MaxInputBytes)}}
			draft, err := taskdistill.GenerateBatched(t.Context(), distillationProgressGenerator{generator, &cobra.Command{}, false}, sources, "discard transient reports", taskdistill.DefaultMaxModelCalls)
			wantCalls := int32(2)
			if tc.truncated {
				wantCalls = 4
			}
			if tc.repair != "" && tc.repair != "duplicates" {
				wantCalls++
			}
			wantAction, wantKnowledge := "discard", 0
			if tc.repair == "duplicates" || tc.repair == "length" {
				wantAction, wantKnowledge = "distill", 1
			}
			if tc.repair == "citation" {
				wantAction = "retain"
			}
			if err != nil || len(draft.Decisions) != 1 || draft.Decisions[0].Action != wantAction || len(draft.Knowledge) != wantKnowledge || calls.Load() != wantCalls || wrongModel.Load() != 0 || truncatedBatch.Load() != 0 {
				t.Fatalf("draft=%+v calls=%d wrong_model=%d truncated=%d err=%v", draft, calls.Load(), wrongModel.Load(), truncatedBatch.Load(), err)
			}
			checkpoint, exists, err := team.LoadSessionReadOnly(workspace)
			if err != nil || !exists || checkpoint.ExecutionPolicySnapshot == nil {
				t.Fatalf("maintenance admission was not persisted: exists=%v err=%v", exists, err)
			}
		})
	}
}

func maintenanceDistillationReplies(truncated bool, repair string) []scripted.Step {
	last := scripted.Step{Content: `{"decisions":[{"source_id":"fragment_0_1","action":"discard","reason":"transient"}],"knowledge":[]}`}
	if !truncated {
		return []scripted.Step{{Content: `{"decisions":[{"source_id":"fragment_0_0","action":"discard","reason":"transient"}],"knowledge":[]}`}, last}
	}
	steps := []scripted.Step{
		{Content: `{"decisions":[{"source_id":"fragment_0_0","action":"distill","reason":"partial"}],"knowledge":[{"text":"Never accept this truncated fact","source_ids":["fragment_0_0"]}]}`},
		{Content: `{"decisions":[{"source_id":"fragment_0_0/0","action":"discard","reason":"transient"}],"knowledge":[]}`},
		{Content: `{"decisions":[{"source_id":"fragment_0_0/1","action":"discard","reason":"transient"}],"knowledge":[]}`}, last,
	}
	var invalid string
	switch repair {
	case "references":
		invalid = `{"decisions":[{"source_id":"fragment_0_0","action":"discard","reason":"transient"}],"knowledge":[]}`
	case "rationale":
		invalid = `{"decisions":[{"source_id":"fragment_0_0/0","action":"discard","reason":" "}],"knowledge":[]}`
	case "schema":
		invalid = `{"decisions":[{"source_id":123,"action":"discard","reason":"transient"}],"knowledge":[]}`
		steps[1].Match = &scripted.Match{Contains: "schema_violation at decisions[0].source_id (type: expected=string, got=number)"}
	case "length":
		invalid = `{"decisions":[{"source_id":"fragment_0_0/0","action":"distill","reason":"verified"}],"knowledge":[{"text":"` + strings.Repeat("x", 2043) + `","source_ids":["fragment_0_0/0"]}]}`
		steps[1] = scripted.Step{Content: `{"texts":[["Keep the verified applicability constraint."]]}`, Match: &scripted.Match{Contains: "Rewrite overlong knowledge text only."}}
	case "sensitive":
		invalid = `{"decisions":[{"source_id":"fragment_0_0/0","action":"distill","reason":"reusable"}],"knowledge":[{"text":"AKIAAAAAAAAAAAAAAAAA","source_ids":["fragment_0_0/0"]}]}`
	case "duplicates":
		steps[1].Content = `{"decisions":[{"source_id":"fragment_0_0/0","action":"distill","reason":"reusable"}],"knowledge":[{"text":"Keep the verified constraint.","source_ids":["fragment_0_0/0"]},{"text":"Keep the verified constraint.","source_ids":["fragment_0_0/0"]}]}`
	case "citation":
		invalid = `{"decisions":[{"source_id":"fragment_0_0/0","action":"retain","reason":"incomplete evidence"}],"knowledge":[{"text":"Unresolved candidate fact","source_ids":["fragment_0_0/0"]}]}`
		steps[1].Content = `{"decisions":[{"source_id":"fragment_0_0/0","action":"retain","reason":"incomplete evidence"}],"knowledge":[]}`
	}
	if invalid != "" {
		steps = slices.Insert(steps, 1, scripted.Step{Content: invalid})
	}
	return steps
}

func TestMaintenanceInvocationErrorPreservesCancellationCause(t *testing.T) {
	transportErr := errors.New("transport closed")
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, team.ErrInvocationStalled} {
		t.Run(cause.Error(), func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			if got := maintenanceInvocationError(ctx, transportErr); got != transportErr {
				t.Fatalf("live context changed transport error: %v", got)
			}
			cancel(cause)
			got := maintenanceInvocationError(ctx, transportErr)
			if !errors.Is(got, cause) || !errors.Is(got, context.Canceled) {
				t.Fatalf("cancellation lost its cause or classification: %v", got)
			}
			if got := maintenanceInvocationError(ctx, nil); got != nil {
				t.Fatalf("successful response changed to failure: %v", got)
			}
		})
	}
}
