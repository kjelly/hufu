package main

import (
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/promotion"
)

func TestWritePromotionDiagnosticsSorted(t *testing.T) {
	var out strings.Builder
	err := writePromotionDiagnostics(&out, []promotion.Diagnostic{
		{SourceID: "b", Reason: "secret_like_content"},
		{SourceID: "a", Reason: "unresolved_conflict"},
		{SourceID: "a", Reason: "secret_like_content"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "diagnostic\ta\tsecret_like_content\ndiagnostic\ta\tunresolved_conflict\ndiagnostic\tb\tsecret_like_content\n"
	if out.String() != want {
		t.Fatalf("diagnostics output = %q, want %q", out.String(), want)
	}
}

func TestPromotionViewDraftEditedLabel(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name   string
		edited *bool
		want   string
	}{
		{name: "unknown", edited: nil, want: "unknown"},
		{name: "edited", edited: &yes, want: "true"},
		{name: "unedited", edited: &no, want: "false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (promotionProposalView{DraftEdited: tc.edited}).draftEditedLabel(); got != tc.want {
				t.Fatalf("label = %q, want %q", got, tc.want)
			}
		})
	}
}
