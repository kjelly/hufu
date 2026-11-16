package taskdistill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestModelSchemaKeepsVerifiedRequestIdentity(t *testing.T) {
	hash := sha256.Sum256([]byte(modelSchema))
	if hex.EncodeToString(hash[:]) != "7577683eb1cd7af0f83f961e04cb182ebbfa33f284e2f5040de3a71fa1485ca4" {
		t.Fatal("model schema changed without a checkpoint compatibility migration")
	}
	prompt, err := draftPrompt([]Source{{ID: "a"}}, "")
	if err != nil || strings.Contains(prompt, ActionExtractOnly) {
		t.Fatalf("model was offered a runtime-owned retention action: %v", err)
	}
}

func TestModelsCannotSubmitRuntimeRetentionActions(t *testing.T) {
	for _, long := range []bool{false, true} {
		text := "Supported constraint."
		if long {
			text = strings.Repeat("x", 2200)
		}
		raw := `{"decisions":[{"source_id":"a","action":"extract_only","reason":"keep source"}],"knowledge":[{"text":"` + text + `","source_ids":["a"]}]}`
		calls := 0
		g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}, generatorFunc: func(_ context.Context, prompt string) (string, error) {
			calls++
			if strings.Contains(prompt, "Rewrite overlong knowledge text only.") {
				t.Fatal("runtime metadata override gained text-only repair authority")
			}
			return raw, nil
		}}
		if _, err := GenerateBatched(t.Context(), g, []Source{{ID: "a"}}, "", 8); !errors.Is(err, errModelContract) || calls != 2 || len(g.checkpoints) != 0 {
			t.Fatalf("model supplied a runtime-owned action: calls=%d err=%v", calls, err)
		}
	}
}

func TestRuntimeRetentionRequiresKnowledgeAndNeverAuthorizesProtectedCleanup(t *testing.T) {
	draft := Draft{Decisions: []Decision{{"a", ActionExtractOnly, "mixed fragment decisions"}}, Knowledge: []Knowledge{{"Supported constraint.", []string{"a"}}}}
	if err := Validate(draft, []Source{{ID: "a"}}); err != nil {
		t.Fatal(err)
	}
	if err := Validate(draft, []Source{{ID: "a", Protected: true}}); err != nil {
		t.Fatal("retained protected source refused reviewed knowledge", err)
	}
	for _, action := range []string{"distill", "discard"} {
		unsafe := Draft{Decisions: []Decision{{"a", action, "cleanup"}}, Knowledge: draft.Knowledge}
		if err := Validate(unsafe, []Source{{ID: "a", Protected: true}}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("protected source admitted cleanup: action=%s err=%v", action, err)
		}
	}
	draft.Knowledge = []Knowledge{}
	if err := Validate(draft, []Source{{ID: "a"}}); !errors.Is(err, ErrInvalid) {
		t.Fatal("runtime extraction action accepted without approved knowledge")
	}
}

func TestUnversionedCheckpointsCannotSupplyRuntimeRetentionActions(t *testing.T) {
	sources := []Source{{ID: "a"}}
	prompt, _ := draftPrompt(sources, "")
	key, _ := draftCheckpointKeys(prompt, true)
	raw := []byte(`{"decisions":[{"source_id":"a","action":"extract_only","reason":"injected"}],"knowledge":[{"text":"Unsupported metadata override.","source_ids":["a"]}]}`)
	g := &checkpointGenerator{checkpoints: map[string]Checkpoint{key: {Draft: raw}}, generatorFunc: func(context.Context, string) (string, error) {
		t.Fatal("unversioned metadata reached model execution")
		return "", nil
	}}
	if _, err := GenerateBatched(t.Context(), g, sources, "", 1); !errors.Is(err, ErrInvalid) || string(g.checkpoints[key].Draft) != string(raw) {
		t.Fatalf("unversioned runtime metadata was accepted or rewritten: %v", err)
	}
}
