package consolidation

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type replyGenerator struct {
	reply string
	err   error
}

func (g replyGenerator) GenerateText(context.Context, string) (string, error) { return g.reply, g.err }

var testSources = []DraftSource{
	{ID: "a", Kind: "pattern", Content: "Run go test before committing."},
	{ID: "b", Kind: "pattern", Content: "Run go vet before committing."},
}

func TestValidateDraft(t *testing.T) {
	cases := []struct {
		name    string
		result  DraftResult
		wantErr string
	}{
		{name: "valid", result: DraftResult{Text: "Run go test and go vet before committing.", CoveredSourceIDs: []string{"b", "a"}}},
		{name: "empty", result: DraftResult{Text: "  ", CoveredSourceIDs: []string{"a", "b"}}, wantErr: "empty text"},
		{name: "too long", result: DraftResult{Text: strings.Repeat("x", MaxDraftRunes+1), CoveredSourceIDs: []string{"a", "b"}}, wantErr: "runes"},
		{name: "secret", result: DraftResult{Text: "Use api_key=sk-live-abcdefghijklmnopqrstu before committing.", CoveredSourceIDs: []string{"a", "b"}}, wantErr: "secret"},
		{name: "verbatim copy", result: DraftResult{Text: "Run go test before committing.", CoveredSourceIDs: []string{"a", "b"}}, wantErr: "verbatim"},
		{name: "missing source", result: DraftResult{Text: "Run go test and go vet.", CoveredSourceIDs: []string{"a"}}, wantErr: "covered_source_ids"},
		{name: "extra source", result: DraftResult{Text: "Run go test and go vet.", CoveredSourceIDs: []string{"a", "b", "c"}}, wantErr: "covered_source_ids"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDraft(tc.result, testSources)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !errors.Is(err, ErrInvalidDraft) {
				t.Fatalf("err = %v, want ErrInvalidDraft containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestJSONDrafterStrictDecode(t *testing.T) {
	cases := []struct {
		name    string
		reply   string
		genErr  error
		wantErr bool
	}{
		{name: "valid", reply: `{"text":"Merged.","covered_source_ids":["a","b"]}`},
		{name: "fenced", reply: "```json\n{\"text\":\"Merged.\",\"covered_source_ids\":[]}\n```", wantErr: true},
		{name: "unknown field", reply: `{"text":"Merged.","covered_source_ids":[],"confidence":1}`, wantErr: true},
		{name: "trailing", reply: `{"text":"Merged.","covered_source_ids":[]}{}`, wantErr: true},
		{name: "generator error", genErr: errors.New("timeout"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := JSONDrafter{Generator: replyGenerator{reply: tc.reply, err: tc.genErr}}.Draft(context.Background(), testSources)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
	if _, err := (JSONDrafter{}).Draft(context.Background(), testSources); err == nil {
		t.Fatal("drafter without generator succeeded")
	}
}

func TestPromptAndSourceRunes(t *testing.T) {
	prompt := JSONDrafter{}.Prompt(testSources)
	for _, want := range []string{`"id":"a"`, "Run go vet before committing.", "text, covered_source_ids"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
	if got := SourceRunes([]DraftSource{{Content: "ab"}, {Content: "中文"}}); got != 4 {
		t.Fatalf("SourceRunes = %d, want 4", got)
	}
}
