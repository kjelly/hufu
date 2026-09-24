package context

import (
	"context"
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
	cases := []struct {
		query string
		want  string
	}{
		{query: "", want: ""},
		{query: "清理舊環境", want: ""},
		{query: "private finding", want: `"private" "finding"`},
		{query: "a NOT b", want: `"a" "b"`},
		{query: "AND OR NOT NEAR", want: ""},
		{query: "not near or and", want: `"not" "near" "or" "and"`},
		{query: "--storage /tmp/kvmforge-router", want: `"storage" "tmp" "kvmforge" "router"`},
	}
	for _, tc := range cases {
		if got := ftsQuery(tc.query); got != tc.want {
			t.Errorf("ftsQuery(%q) = %q, want %q", tc.query, got, tc.want)
		}
	}
}
