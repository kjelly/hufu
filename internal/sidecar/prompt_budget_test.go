package sidecar

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBuildBoundedPromptPreservesStructureWithinLimit(t *testing.T) {
	sections := []PromptSection{
		{Header: "## Goal\n", Content: strings.Repeat("目", 200), Footer: "\n", Weight: 1, MaxRunes: 40},
		{Header: "## Evidence\n", Content: strings.Repeat("證", 500), Footer: "\n", Weight: 3},
	}

	prompt, err := BuildBoundedPrompt("BEGIN\n", sections, "END", 180)
	if err != nil {
		t.Fatal(err)
	}
	if got := utf8.RuneCountInString(prompt); got > 180 {
		t.Fatalf("prompt = %d runes, want at most 180", got)
	}
	for _, want := range []string{"BEGIN", "## Goal", "## Evidence", "runes omitted", "END"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("bounded prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Count(prompt, "證") <= strings.Count(prompt, "目") {
		t.Fatalf("weighted evidence section did not receive more budget:\n%s", prompt)
	}
}

func TestBuildBoundedPromptRejectsOversizedStructure(t *testing.T) {
	_, err := BuildBoundedPrompt(strings.Repeat("x", 9), nil, "yy", 10)
	if err == nil || !strings.Contains(err.Error(), "structure exceeds") {
		t.Fatalf("BuildBoundedPrompt() error = %v, want structural limit error", err)
	}
}

func TestBuildBoundedPromptIsDeterministic(t *testing.T) {
	sections := []PromptSection{
		{Header: "A:", Content: strings.Repeat("a", 50), Footer: "\n", Weight: 1},
		{Header: "B:", Content: strings.Repeat("b", 50), Footer: "\n", Weight: 2},
	}
	first, err := BuildBoundedPrompt("", sections, "done", 60)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildBoundedPrompt("", sections, "done", 60)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("bounded prompt is nondeterministic:\nfirst: %q\nsecond: %q", first, second)
	}
}
