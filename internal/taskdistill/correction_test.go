package taskdistill

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"strings"
	"testing"
)

func TestCorrectionReceivesRejectedCandidateWithoutOverridingRetention(t *testing.T) {
	for _, action := range []string{"retain", "discard"} {
		t.Run(action, func(t *testing.T) {
			sources := []Source{{ID: "source", Content: "frozen evidence"}}
			candidate := Draft{Decisions: []Decision{{"source", action, "preserve this judgment"}}, Knowledge: []Knowledge{{"candidate fact", []string{"source"}}}}
			raw, _ := json.Marshal(candidate)
			calls := 0
			g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}}
			g.generatorFunc = func(_ context.Context, prompt string) (string, error) {
				calls++
				if calls == 1 {
					return string(raw), nil
				}
				_, context, found := strings.Cut(prompt, "not a patch:\n")
				context, _, _ = strings.Cut(context, "\nEnd rejected candidate.")
				var previous Draft
				if !found || json.Unmarshal([]byte(context), &previous) != nil || Digest(previous) != Digest(candidate) || !strings.Contains(prompt, "decision action="+action) {
					t.Fatal("correction lost candidate, action diagnostic, or replacement contract")
				}
				if got := requestedSources(t, prompt); len(got) != 1 || got[0] != sources[0] {
					t.Fatal("correction changed frozen evidence")
				}
				candidate.Knowledge = []Knowledge{}
				replacement, _ := json.Marshal(candidate)
				return string(replacement), nil
			}
			draft, err := GenerateBatched(t.Context(), g, sources, "", 2)
			if err != nil || calls != 2 || draft.Decisions[0].Action != action || len(draft.Knowledge) != 0 || len(g.checkpoints) != 1 {
				t.Fatalf("retention changed or repair failed: %+v calls=%d err=%v", draft, calls, err)
			}
		})
	}
}

func TestCorrectionCandidateIsRedactedBoundedAndNotPublished(t *testing.T) {
	secret := "AKIA" + strings.Repeat("A", 16)
	for _, raw := range []string{
		`{"knowledge":[{"text":"` + secret + `"}]}`,
		`{"knowledge":[{"text":"\u0041KIA` + strings.Repeat("A", 16) + `"}]}`,
		`{"knowledge":[{"text":"` + secret,
	} {
		sources := []Source{{ID: "source", Content: "frozen evidence"}}
		canonical, _ := draftPrompt(sources, "")
		issue := &OutputViolation{Code: "schema_violation", Path: "$"}
		err := withRejectedCandidate(issue, raw)
		prompt, promptErr := correctionPrompt(canonical, sources, err, false)
		if promptErr != nil || strings.Contains(prompt, secret) || strings.Contains(prompt, `\u0041KIA`) {
			t.Fatalf("candidate leaked a secret: %v", promptErr)
		}
		published, _ := json.Marshal(issue)
		if strings.Contains(string(published), "knowledge") || strings.Contains(issue.Error(), "knowledge") || !errors.Is(issue, errModelContract) {
			t.Fatal("candidate became a public diagnostic")
		}
		if !jsontext.Value(raw).IsValid() && issue.candidate != "" {
			t.Fatal("malformed candidate replayed")
		}
		base, _ := requestPrompt(canonical, sources, err.Error(), false)
		large := strings.Repeat("x", MaxPromptBytes-len(base)-1) + canonical
		bounded, err := correctionPrompt(large, sources, issue, false)
		if err != nil || len(bounded) > MaxPromptBytes || strings.Contains(bounded, "Rejected candidate JSON") {
			t.Fatalf("oversize candidate was truncated or exceeded envelope: %d %v", len(bounded), err)
		}
	}
}
