package team

import (
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"os"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func pinnedCodexUserInputSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile("testdata/codex/0.166.0/user-input.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	definitions, ok := document["definitions"].(map[string]any)
	if !ok || definitions["UserInput"] == nil {
		t.Fatal("missing pinned UserInput definition")
	}
	canonical, err := json.Marshal(definitions["UserInput"], json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	const wantDigest = "b082a38701b9ec3eb1ac56572e9d5d55c9016c1428ff6ddff647e24c5b9e139b"
	if got := fmt.Sprintf("%x", sha256.Sum256(canonical)); got != wantDigest {
		t.Fatalf("pinned Codex 0.166.0 UserInput digest=%s, want %s", got, wantDigest)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("user-input.schema.json", document); err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile("user-input.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func TestPinnedCodexUserInputSchema(t *testing.T) {
	schema := pinnedCodexUserInputSchema(t)
	for _, tc := range []struct {
		name  string
		input map[string]any
		valid bool
	}{
		{name: "text", input: map[string]any{"type": "text", "text": "Describe this image"}, valid: true},
		{name: "local image", input: map[string]any{"type": "localImage", "path": "/fixture/owned/image.png"}, valid: true},
		{name: "local image detail", input: map[string]any{"type": "localImage", "path": "/fixture/owned/image.png", "detail": "original"}, valid: true},
		{name: "missing image path", input: map[string]any{"type": "localImage"}},
		{name: "wrong field for image", input: map[string]any{"type": "localImage", "text": "/fixture/owned/image.png"}},
		{name: "invalid detail", input: map[string]any{"type": "localImage", "path": "/fixture/owned/image.png", "detail": "unsupported"}},
		{name: "invalid variant case", input: map[string]any{"type": "localimage", "path": "/fixture/owned/image.png"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := schema.Validate(tc.input)
			if (err == nil) != tc.valid {
				t.Fatalf("schema valid=%t, want %t: %v", err == nil, tc.valid, err)
			}
		})
	}
}

func TestCodexTextInputMatchesPinnedSchema(t *testing.T) {
	schema := pinnedCodexUserInputSchema(t)
	raw, err := json.Marshal(codexUserInput{Type: "text", Text: "Preserve text-only input"})
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(value); err != nil {
		t.Fatalf("current Codex text input violates the pinned schema: %v", err)
	}
}
