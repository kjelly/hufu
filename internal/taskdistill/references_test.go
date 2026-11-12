package taskdistill

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestRequestSchemaBindsOuterSourceIdentities(t *testing.T) {
	sources := []Source{{ID: "fragment_0_3/0/0", Content: `{"id":"task-12","source_id":"fragment_0_3"}`}}
	canonical, err := draftPrompt(sources, "preserve verified constraints")
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := requestPrompt(canonical, sources, "", false)
	if err != nil {
		t.Fatal(err)
	}
	_, schemaText, found := strings.Cut(prompt, "Return ONLY JSON matching this schema: ")
	if !found {
		t.Fatal("missing model-facing schema")
	}
	schemaText, _, _ = strings.Cut(schemaText, "\nDATA: ")
	var document any
	if err := json.Unmarshal([]byte(schemaText), &document); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("urn:batch", document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("urn:batch")
	if err != nil {
		t.Fatal(err)
	}
	valid := discardedReply(t, sources)
	for _, id := range []string{sources[0].ID, "task-12", "fragment_0_3"} {
		raw := strings.ReplaceAll(valid, sources[0].ID, id)
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(value); (err == nil) != (id == sources[0].ID) {
			t.Fatalf("schema accepted wrong identity %q: %v", id, err)
		}
	}
	var blank any
	if err := json.Unmarshal([]byte(strings.Replace(valid, `"reason":"transient"`, `"reason":" "`, 1)), &blank); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(blank); err == nil {
		t.Fatal("model-facing schema accepted a whitespace-only rationale")
	}
	if got := requestedSources(t, prompt); len(got) != 1 || got[0] != sources[0] {
		t.Fatalf("source evidence changed: %+v", got)
	}
}

func TestDraftCorrectionDoesNotRestartForAnotherRejectionKind(t *testing.T) {
	calls := 0
	g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}}
	g.generatorFunc = func(context.Context, string) (string, error) {
		calls++
		if calls == 1 {
			return `{"decisions":[{"source_id":"unknown","action":"discard","reason":"transient"}],"knowledge":[]}`, nil
		}
		return `{"decisions":[{"source_id":"known","action":"discard","reason":" "}],"knowledge":[]}`, nil
	}
	draft, err := GenerateBatched(t.Context(), g, []Source{{ID: "known"}}, "", 8)
	if !errors.Is(err, errBlankRationale) || calls != 2 || len(draft.Decisions) != 0 || len(g.checkpoints) != 0 {
		t.Fatalf("correction restarted or invalid progress saved: draft=%+v calls=%d err=%v", draft, calls, err)
	}
}

func TestDraftContractRepairIsBoundedAndBudgeted(t *testing.T) {
	sources := []Source{{ID: "a"}, {ID: "b"}}
	for _, tc := range []struct {
		name, invalid string
		kind          error
	}{
		{"unknown", `{"decisions":[{"source_id":"task-12","action":"discard","reason":"transient"},{"source_id":"b","action":"discard","reason":"transient"}],"knowledge":[]}`, ErrReferences},
		{"repeated", `{"decisions":[{"source_id":"a","action":"discard","reason":"transient"},{"source_id":"a","action":"discard","reason":"transient"}],"knowledge":[]}`, ErrReferences},
		{"missing", `{"decisions":[{"source_id":"a","action":"discard","reason":"transient"}],"knowledge":[]}`, ErrReferences},
		{"unknown knowledge", `{"decisions":[{"source_id":"a","action":"distill","reason":"reusable"},{"source_id":"b","action":"discard","reason":"transient"}],"knowledge":[{"text":"verified rule","source_ids":["task-12"]}]}`, ErrReferences},
		{"blank rationale", `{"decisions":[{"source_id":"a","action":"discard","reason":" \t\n"},{"source_id":"b","action":"discard","reason":"transient"}],"knowledge":[]}`, errBlankRationale},
		{"unicode blank rationale", `{"decisions":[{"source_id":"a","action":"discard","reason":"\u3000"},{"source_id":"b","action":"discard","reason":"transient"}],"knowledge":[]}`, errBlankRationale},
	} {
		for _, mode := range []string{"corrected", "still invalid", "budget exhausted"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				calls := 0
				g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}}
				g.generatorFunc = func(_ context.Context, prompt string) (string, error) {
					calls++
					if calls == 1 || mode == "still invalid" {
						return tc.invalid, nil
					}
					if !strings.Contains(prompt, "previous response was rejected") {
						t.Fatal("repair omitted validation feedback")
					}
					if tc.name == "unknown knowledge" {
						return strings.Replace(tc.invalid, `["task-12"]`, `["a"]`, 1), nil
					}
					return discardedReply(t, requestedSources(t, prompt)), nil
				}
				budget := 8
				if mode == "budget exhausted" {
					budget = 1
				}
				draft, err := GenerateBatched(t.Context(), g, sources, "", budget)
				if mode == "corrected" {
					if err != nil || Validate(draft, sources) != nil || calls != 2 || len(g.checkpoints) != 1 {
						t.Fatalf("draft=%+v calls=%d err=%v", draft, calls, err)
					}
					return
				}
				if !errors.Is(err, tc.kind) || len(draft.Decisions) != 0 || len(g.checkpoints) != 0 || calls > 2 {
					t.Fatalf("invalid reference accepted or retried without bound: %+v calls=%d err=%v", draft, calls, err)
				}
				if mode == "budget exhausted" && (!errors.Is(err, ErrCallBudget) || calls != 1) {
					t.Fatalf("repair exceeded call budget: calls=%d err=%v", calls, err)
				}
			})
		}
	}
}

func TestAcceptedCheckpointRemainsReusableAfterReferencePromptStrengthening(t *testing.T) {
	sources := []Source{{ID: "fragment_0_3/0", Content: "verified evidence"}}
	canonical, err := draftPrompt(sources, "")
	if err != nil {
		t.Fatal(err)
	}
	// This is the pre-repair request identity and canonical validated payload.
	key := Digest(struct {
		Prompt       string
		SplitContent bool
	}{canonical, true})
	g := &checkpointGenerator{checkpoints: map[string]Checkpoint{key: {Draft: []byte(discardedReply(t, sources))}}}
	g.generatorFunc = func(context.Context, string) (string, error) {
		t.Fatal("accepted checkpoint caused another model call")
		return "", nil
	}
	if draft, err := GenerateBatched(t.Context(), g, sources, "", 1); err != nil || Validate(draft, sources) != nil {
		t.Fatalf("accepted checkpoint rejected: %+v %v", draft, err)
	}
	g.checkpoints[key] = Checkpoint{Draft: []byte(strings.ReplaceAll(discardedReply(t, sources), sources[0].ID, "task-12"))}
	if _, err := GenerateBatched(t.Context(), g, sources, "", 1); !errors.Is(err, ErrReferences) {
		t.Fatalf("invalid cached references were not revalidated: %v", err)
	}
}

func TestSplitReferenceRepairRetainsValidatedProgressWhenBudgetEnds(t *testing.T) {
	sources := []Source{{ID: "fragment_0_3", Content: strings.Repeat("evidence", 800)}}
	calls := []string{}
	g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}}
	g.generatorFunc = func(_ context.Context, prompt string) (string, error) {
		requested := requestedSources(t, prompt)
		id := requested[0].ID
		calls = append(calls, id)
		if id == sources[0].ID {
			return `{"decisions":[`, nil
		}
		if strings.HasSuffix(id, "/0") {
			ref := sources[0].ID // Simulate the model copying the parent ID.
			if strings.Contains(prompt, "previous response was rejected") {
				ref = id
			}
			draft := Draft{Decisions: []Decision{{SourceID: ref, Action: "distill", Reason: "verified constraint"}}, Knowledge: []Knowledge{{Text: "Preserve the verified applicability constraint.", SourceIDs: []string{ref}}}}
			raw, err := json.Marshal(draft)
			return string(raw), err
		}
		return discardedReply(t, requested), nil
	}
	if _, err := GenerateBatched(t.Context(), g, sources, "", 3); !errors.Is(err, ErrCallBudget) || len(calls) != 3 {
		t.Fatalf("unbounded repair or partial acceptance: calls=%v err=%v", calls, err)
	}
	calls = nil
	draft, err := GenerateBatched(t.Context(), g, sources, "", 3)
	if err != nil || len(calls) != 1 || calls[0] != "fragment_0_3/1" || len(draft.Knowledge) != 1 || draft.Knowledge[0].SourceIDs[0] != sources[0].ID {
		t.Fatalf("repaired progress or provenance lost: %+v calls=%v err=%v", draft, calls, err)
	}
	calls = nil
	if _, err := GenerateBatched(t.Context(), g, sources, "", 1); err != nil || len(calls) != 0 {
		t.Fatalf("completed split was regenerated: calls=%v err=%v", calls, err)
	}
}
