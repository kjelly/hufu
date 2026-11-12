package taskdistill

import (
	"context"
	"errors"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
)

type generatorFunc func(context.Context, string) (string, error)

func (f generatorFunc) GenerateText(ctx context.Context, prompt string) (string, error) {
	return f(ctx, prompt)
}

func TestGenerateValidatesRetentionAndKnowledge(t *testing.T) {
	sources := []Source{{ID: "a", Content: "verified procedure"}, {ID: "b", Content: "same verified procedure"}}
	valid := `{"decisions":[{"source_id":"a","action":"distill","reason":"reusable"},{"source_id":"b","action":"distill","reason":"duplicate"}],"knowledge":[{"text":"Use the verified procedure under its original constraints.","source_ids":["a","b"]}]}`
	for _, tc := range []struct {
		name, reply string
		sources     []Source
		fail        bool
	}{
		{"merge", valid, sources, false},
		{"protected", valid, []Source{{ID: "a", Protected: true}, {ID: "b"}}, true},
		{"unknown source", strings.ReplaceAll(valid, `"b"`, `"c"`), sources, true},
		{"missing coverage", strings.Replace(valid, `["a","b"]`, `["a"]`, 1), sources, true},
		{"duplicate citation", strings.Replace(valid, `["a","b"]`, `["a","a","b"]`, 1), sources, true},
		{"unknown field", strings.Replace(valid, `"decisions":`, `"receipt":"forged","decisions":`, 1), sources, true},
		{"wrong case", strings.Replace(valid, `"distill"`, `"DISTILL"`, 1), sources, true},
		{"trailing", valid + `{}`, sources, true},
		{"fenced", "```json\n" + valid + "\n```", sources, true},
		{"missing required array", `{"decisions":[]}`, sources, true},
		{"missing knowledge", `{"decisions":[{"source_id":"a","action":"discard","reason":"transient"},{"source_id":"b","action":"discard","reason":"transient"}]}`, sources, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Generate(t.Context(), generatorFunc(func(context.Context, string) (string, error) { return tc.reply, nil }), tc.sources, "")
			if tc.fail {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("expected invalid draft, got %+v %v", result, err)
				}
				return
			}
			if err != nil || len(result.Knowledge) != 1 {
				t.Fatalf("draft=%+v err=%v", result, err)
			}
		})
	}
}

func TestGenerateRefusesOversizedSourcesBeforeCallingModel(t *testing.T) {
	calls := 0
	_, err := Generate(t.Context(), generatorFunc(func(context.Context, string) (string, error) { calls++; return "", nil }), []Source{{ID: "a", Content: strings.Repeat("x", MaxInputBytes)}}, "")
	if !errors.Is(err, ErrInvalid) || calls != 0 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestDigestIsStableAcrossMapIterationOrders(t *testing.T) {
	value := map[string]any{"a": map[string]int{"x": 1, "y": 2}, "b": "evidence"}
	want := Digest(value)
	for range 100 {
		if got := Digest(value); got != want {
			t.Fatalf("unstable digest: %s != %s", got, want)
		}
	}
}

func TestSaveKnowledgeIsIdempotentAndRefusesChangedConfirmation(t *testing.T) {
	repo, err := contextstore.OpenSQLiteInMemory("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	sources := []Source{{ID: "one", Content: "source evidence"}}
	draft := Draft{Decisions: []Decision{{"one", "distill", "useful"}}, Knowledge: []Knowledge{{"Keep a reusable validated procedure.", []string{"one"}}}}
	scope := contextstore.Scope{ProjectID: "project", TeamID: "team"}
	for range 2 {
		if err = SaveKnowledge(t.Context(), repo, scope, "plan", sources, draft); err != nil {
			t.Fatal(err)
		}
	}
	items, err := repo.Query(t.Context(), contextstore.RepositoryQuery{Scope: scope})
	if err != nil || len(items) != 1 || len(items[0].Evidence) != 1 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	if items[0].Evidence[0].Ref != "one@sha256:"+Digest(sources[0]) {
		t.Fatal("knowledge provenance does not bind the complete source snapshot")
	}
	if err = repo.RetireConfirmed(t.Context(), []string{items[0].ID}, "obsolete"); err != nil {
		t.Fatal(err)
	}
	if err = SaveKnowledge(t.Context(), repo, scope, "plan", sources, draft); err == nil {
		t.Fatal("retired knowledge authorized cleanup")
	}
}
