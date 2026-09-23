package memoryconflict

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
)

func tokenList(content string) []string {
	var out []string
	for token := range Tokens(content) {
		out = append(out, token)
	}
	sort.Strings(out)
	return out
}

func TestTokens(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{name: "stopwords and short words dropped", content: "Use the DB for storage, not files", want: []string{"files", "storage"}},
		{name: "underscore and digits kept", content: "run go_test in v2 of phase123", want: []string{"go_test", "phase123", "run"}},
		{name: "chinese bigrams", content: "使用資料庫", want: []string{"使用", "料庫", "用資", "資料"}},
		{name: "single han rune", content: "改 SQLite", want: []string{"sqlite", "改"}},
		{name: "mixed script splits runs", content: "部署到production環境", want: []string{"production", "環境", "署到", "部署"}},
		{name: "japanese kana bigrams", content: "テスト", want: []string{"スト", "テス"}},
		{name: "other scripts need three runes", content: "да база", want: []string{"база"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sort.Strings(tc.want)
			if got := tokenList(tc.content); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Tokens(%q) = %q, want %q", tc.content, got, tc.want)
			}
		})
	}
}

func conflictItem(id, content string, agent string, paths ...string) contextstore.ContextItem {
	item := contextstore.ContextItem{ID: id, Kind: contextstore.ContextDecision, Content: content, ContentHash: "hash-" + content, Scope: contextstore.Scope{ProjectID: "p", TeamID: "t", AgentID: agent}}
	for _, path := range paths {
		item.Evidence = append(item.Evidence, contextstore.EvidenceRef{Type: "file_path", Ref: path})
	}
	return item
}

func pairIDs(set CandidateSet) []string {
	out := make([]string, 0, len(set.Pairs))
	for _, pair := range set.Pairs {
		out = append(out, pair.A.ID+"|"+pair.B.ID)
	}
	return out
}

func TestCandidatePairs(t *testing.T) {
	storage := conflictItem("a", "Use SQLite database storage for session state", "")
	storage2 := conflictItem("b", "Use PostgreSQL database storage for session state", "")
	unrelated := conflictItem("c", "Indent Go code with tabs", "")
	privateStorage := conflictItem("d", "Use MySQL database storage for session state", "worker")
	duplicate := storage2
	duplicate.ID = "e"
	cases := []struct {
		name  string
		items []contextstore.ContextItem
		opts  PairOptions
		want  []string
	}{
		{name: "overlapping topics pair", items: []contextstore.ContextItem{unrelated, storage2, storage}, want: []string{"a|b"}},
		{name: "scopes never mix", items: []contextstore.ContextItem{storage, privateStorage}, want: []string{}},
		{name: "identical content is not a candidate", items: []contextstore.ContextItem{storage2, duplicate}, want: []string{}},
		{name: "shared file path pairs unrelated text", items: []contextstore.ContextItem{conflictItem("x", "Build with make", "", "Makefile"), conflictItem("y", "Never commit binaries", "", "Makefile")}, want: []string{"x|y"}},
		{name: "common file path ignored", items: []contextstore.ContextItem{conflictItem("x", "Build with make", "", "go.mod"), conflictItem("y", "Never commit binaries", "", "go.mod"), conflictItem("z", "Pin tool versions", "", "go.mod")}, opts: PairOptions{MaxFilePathFanout: 2}, want: []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set, err := CandidatePairs(context.Background(), tc.items, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if got := pairIDs(set); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("pairs = %v, want %v", got, tc.want)
			}
			again, err := CandidatePairs(context.Background(), tc.items, tc.opts)
			if err != nil || !reflect.DeepEqual(pairIDs(again), pairIDs(set)) {
				t.Fatalf("CandidatePairs is not deterministic: %v vs %v (%v)", pairIDs(again), pairIDs(set), err)
			}
		})
	}
}

func TestCandidatePairsFanoutDiagnosticAndLimits(t *testing.T) {
	items := []contextstore.ContextItem{conflictItem("x", "Build with make", "", "go.mod"), conflictItem("y", "Never commit binaries", "", "go.mod"), conflictItem("z", "Pin tool versions", "", "go.mod")}
	set, err := CandidatePairs(context.Background(), items, PairOptions{MaxFilePathFanout: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Diagnostics) != 1 || set.Diagnostics[0].Reason != ReasonFilePathTooCommon || set.Diagnostics[0].Detail != "go.mod" {
		t.Fatalf("diagnostics = %+v", set.Diagnostics)
	}
	if _, err = CandidatePairs(context.Background(), items, PairOptions{MaxItemsPerScope: 2}); err == nil || !strings.Contains(err.Error(), "--max-items") {
		t.Fatalf("over-limit scope err = %v", err)
	}
}

func TestTopScoredOrdersByScoreThenID(t *testing.T) {
	got := topScored([]scored{{id: "d", score: 0.5}, {id: "c", score: 0.9}, {id: "b", score: 0.5}, {id: "a", score: 0.1}}, 3)
	want := []scored{{id: "c", score: 0.9}, {id: "b", score: 0.5}, {id: "d", score: 0.5}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("topScored = %+v, want %+v", got, want)
	}
}

type fakeSimilar struct {
	neighbors map[string][]contextstore.SearchResult
	failAt    int
	calls     int
}

func (f *fakeSimilar) SearchSimilarTo(_ context.Context, itemID string, _ contextstore.SearchRequest) ([]contextstore.SearchResult, error) {
	f.calls++
	if f.failAt > 0 && f.calls == f.failAt {
		return nil, errors.New("ollama unavailable")
	}
	return f.neighbors[itemID], nil
}

func result(item contextstore.ContextItem, score float64) contextstore.SearchResult {
	return contextstore.SearchResult{Item: item, Score: score}
}

func TestCandidatePairsVectorFindsVocabularyMismatch(t *testing.T) {
	sqlite := conflictItem("sqlite", "Use SQLite for storage", "")
	postgres := conflictItem("postgres", "Architecture migrated to PostgreSQL", "")
	items := []contextstore.ContextItem{sqlite, postgres}
	lexical, err := CandidatePairs(context.Background(), items, PairOptions{})
	if err != nil || len(lexical.Pairs) != 0 {
		t.Fatalf("lexical pairs = %v err=%v, want none", pairIDs(lexical), err)
	}
	vector := &fakeSimilar{neighbors: map[string][]contextstore.SearchResult{"sqlite": {result(postgres, 0.8)}}}
	set, err := CandidatePairs(context.Background(), items, PairOptions{Vector: vector})
	if err != nil {
		t.Fatal(err)
	}
	if !set.VectorUsed || set.VectorPairs != 1 || !reflect.DeepEqual(pairIDs(set), []string{"postgres|sqlite"}) {
		t.Fatalf("vector set = %+v", set)
	}
}

func TestCandidatePairsVectorFiltersEligibilityScopeAndThreshold(t *testing.T) {
	a := conflictItem("a", "Use SQLite for storage", "")
	b := conflictItem("b", "Architecture migrated to PostgreSQL", "")
	outsider := conflictItem("z", "Not eligible", "")
	private := conflictItem("p", "Private note", "worker")
	vector := &fakeSimilar{neighbors: map[string][]contextstore.SearchResult{
		"a": {result(a, 1), result(outsider, 0.99), result(private, 0.95), result(b, 0.4)},
	}}
	set, err := CandidatePairs(context.Background(), []contextstore.ContextItem{a, b}, PairOptions{Vector: vector})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Pairs) != 0 {
		t.Fatalf("pairs = %v, want none (self, non-eligible, other scope, below threshold)", pairIDs(set))
	}
}

func TestCandidatePairsVectorTieBreak(t *testing.T) {
	a := conflictItem("a", "alpha", "")
	b := conflictItem("b", "bravo", "")
	c := conflictItem("c", "charlie", "")
	vector := &fakeSimilar{neighbors: map[string][]contextstore.SearchResult{"a": {result(c, 0.7), result(b, 0.7)}}}
	set, err := CandidatePairs(context.Background(), []contextstore.ContextItem{a, b, c}, PairOptions{Vector: vector, VectorTopK: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pairIDs(set), []string{"a|b"}) {
		t.Fatalf("pairs = %v, want the smaller ID on a tie", pairIDs(set))
	}
}

func TestCandidatePairsDropsAllVectorResultsOnError(t *testing.T) {
	a := conflictItem("a", "Use SQLite database storage for session state", "")
	b := conflictItem("b", "Use PostgreSQL database storage for session state", "")
	c := conflictItem("c", "Architecture migrated", "")
	vector := &fakeSimilar{neighbors: map[string][]contextstore.SearchResult{"a": {result(c, 0.9)}}, failAt: 2}
	set, err := CandidatePairs(context.Background(), []contextstore.ContextItem{a, b, c}, PairOptions{Vector: vector})
	if err != nil {
		t.Fatal(err)
	}
	if set.VectorUsed || set.VectorPairs != 0 || !reflect.DeepEqual(pairIDs(set), []string{"a|b"}) {
		t.Fatalf("set = %+v, want lexical pairs only", set)
	}
	if len(set.Diagnostics) != 1 || set.Diagnostics[0].Reason != ReasonVectorUnavailable {
		t.Fatalf("diagnostics = %+v", set.Diagnostics)
	}
}

func TestCandidatePairsUnionCountsPerSource(t *testing.T) {
	a := conflictItem("a", "Use SQLite database storage for session state", "", "db.go")
	b := conflictItem("b", "Use PostgreSQL database storage for session state", "", "db.go")
	vector := &fakeSimilar{neighbors: map[string][]contextstore.SearchResult{"a": {result(b, 0.9)}, "b": {result(a, 0.9)}}}
	set, err := CandidatePairs(context.Background(), []contextstore.ContextItem{a, b}, PairOptions{Vector: vector})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Pairs) != 1 || set.LexicalPairs != 1 || set.FilePathPairs != 1 || set.VectorPairs != 1 {
		t.Fatalf("set = %+v", set)
	}
}

func TestParseJudgmentStrictDecode(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "valid", raw: `{"verdict":"contradicts","rationale":"Different engines."}`},
		{name: "fenced", raw: "```json\n{\"verdict\":\"compatible\",\"rationale\":\"\"}\n```", wantErr: true},
		{name: "unknown field", raw: `{"verdict":"compatible","rationale":"","confidence":1}`, wantErr: true},
		{name: "trailing JSON", raw: `{"verdict":"compatible","rationale":""}{}`, wantErr: true},
		{name: "unknown verdict", raw: `{"verdict":"maybe","rationale":""}`, wantErr: true},
		{name: "undetermined is not a model verdict", raw: `{"verdict":"undetermined","rationale":""}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseJudgment(tc.raw)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidJudgment) {
				t.Fatalf("err = %v, want ErrInvalidJudgment", err)
			}
		})
	}
}

func TestJSONJudgePromptCarriesBothMemories(t *testing.T) {
	a := conflictItem("a", "Use SQLite", "")
	b := conflictItem("b", "Use PostgreSQL", "")
	prompt := JSONJudge{}.Prompt(a, b)
	for _, want := range []string{`"memory_a"`, `"Use SQLite"`, `"memory_b"`, `"Use PostgreSQL"`, "verdict, rationale"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q: %s", want, prompt)
		}
	}
	if _, err := (JSONJudge{}).Judge(context.Background(), a, b); err == nil {
		t.Fatal("judge without generator succeeded")
	}
}
