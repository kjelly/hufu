package context

import (
	"context"
	"path/filepath"
	"testing"
)

// TestSearchLexicalToleratesQueriesWithoutFTSTerms guards the worker context
// routing preflight: a task goal written entirely in CJK text, or one whose
// only ASCII words are FTS5 operators, must not turn lexical retrieval into
// an FTS syntax error that fails the task before its worker starts.
func TestSearchLexicalToleratesQueriesWithoutFTSTerms(t *testing.T) {
	repo, _ := scopeMatrixFixture(t)
	scope := Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess", AgentID: "agent-a"}
	cases := []struct {
		name    string
		query   string
		wantHit string
	}{
		{name: "CJK-only goal", query: "清理舊環境並建立驗證所需基礎設施"},
		{name: "punctuation only", query: "--- ;;; //"},
		{name: "operator keywords only", query: "AND OR NOT NEAR"},
		{name: "operator keyword between terms", query: "private NOT finding", wantHit: "agent-a-finding"},
		{name: "trailing operator keyword", query: "private finding OR", wantHit: "agent-a-finding"},
		{name: "mixed CJK and paths", query: "使用 --storage /tmp/kvmforge-router 避開 AppArmor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := repo.SearchLexical(context.Background(), SearchRequest{Query: tc.query, Scope: scope})
			if err != nil {
				t.Fatalf("SearchLexical(%q) error = %v", tc.query, err)
			}
			if tc.wantHit != "" && !containsID(idsOfResults(got), tc.wantHit) {
				t.Fatalf("SearchLexical(%q) = %v, want %s", tc.query, idsOfResults(got), tc.wantHit)
			}
		})
	}
}

func TestFTSQueryQuotesTermsAndDropsOperators(t *testing.T) {
	// Terms are alternatives: a record that shares any of them can match.
	cases := []struct {
		query string
		want  string
	}{
		{query: "", want: ""},
		{query: "清理舊環境", want: ""},
		{query: "private finding", want: `"private" OR "finding"`},
		{query: "a NOT b", want: `"a" OR "b"`},
		{query: "AND OR NOT NEAR", want: ""},
		{query: "not near or and", want: `"not" OR "near" OR "or" OR "and"`},
		{query: "--storage /tmp/kvmforge-router", want: `"storage" OR "tmp" OR "kvmforge" OR "router"`},
		{query: "Redact the token; redact THE Token", want: `"Redact" OR "the" OR "token"`},
	}
	for _, tc := range cases {
		if got := ftsQuery(tc.query); got != tc.want {
			t.Errorf("ftsQuery(%q) = %q, want %q", tc.query, got, tc.want)
		}
	}
}

// TestSearchLexicalMatchesLongGoalsBySharedTerms pins lexical retrieval for
// the queries it actually receives: a task goal of a paragraph or more. When
// every term was required, no record contained all of them and persistent
// memory never reached the worker. A record that shares the goal's rare terms
// must be found and ranked above one that shares fewer of them, and one
// that shares no term must not be found.
func TestSearchLexicalMatchesLongGoalsBySharedTerms(t *testing.T) {
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	scope := Scope{ProjectID: "proj", TeamID: "team"}
	if err = repo.Append(context.Background(),
		ContextItem{ID: "redaction", Kind: ContextPattern, Content: "RedactSecrets runs the private key block pass before the key/value passes.", Scope: scope, Lifecycle: LifecycleConfirmed},
		ContextItem{ID: "testing", Kind: ContextPattern, Content: "Add table-driven tests for every package.", Scope: scope, Lifecycle: LifecycleConfirmed},
		ContextItem{ID: "reviewprep", Kind: ContextPattern, Content: "reviewprep resolves commit ranges via git rev-list.", Scope: scope, Lifecycle: LifecycleConfirmed},
	); err != nil {
		t.Fatal(err)
	}
	goal := "Modify internal/utils/redact.go so that RedactSecrets also redacts bare provider tokens such as sk- and ghp_ " +
		"that appear without a credential-named key. Keep the private key block pass first, keep redaction idempotent, " +
		"and add table-driven tests that use only obvious fake values."
	got, err := repo.SearchLexical(context.Background(), SearchRequest{Query: goal, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	ids := idsOfResults(got)
	if len(ids) != 2 || ids[0] != "redaction" || ids[1] != "testing" {
		t.Fatalf("SearchLexical(goal) = %v, want [redaction testing]", ids)
	}
}
