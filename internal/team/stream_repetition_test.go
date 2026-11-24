package team

import (
	"errors"
	"strings"
	"testing"
)

func TestStreamRepetitionDetector_NormalText(t *testing.T) {
	t.Parallel()
	d := NewStreamRepetitionDetector()
	chunks := []string{
		"# Title\n\nThis is a normal paragraph discussing code review.\n",
		"```go\nfunc main() {\n\tprintln(\"hello world\")\n}\n```\n",
		"| Header 1 | Header 2 |\n|----------|----------|\n| Val 1    | Val 2    |\n",
		"Here is a divider:\n----------------------------------------\nEnd of section.\n",
	}

	for _, chunk := range chunks {
		if err := d.Process(chunk); err != nil {
			t.Fatalf("unexpected error on normal text: %v", err)
		}
	}
}

func TestStreamRepetitionDetector_WhitespaceWithProgress(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		whitespace string
	}{
		{"75 spaces", strings.Repeat(" ", 75)},
		{"100 spaces", strings.Repeat(" ", 100)},
		{"mixed whitespace", strings.Repeat(" \t\n\r ", 20)},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := `{"result":` + test.whitespace + `true}`
			for split := 1; split <= len(output); split++ {
				detector := NewStreamRepetitionDetector()
				for _, chunk := range []string{output[:split], output[split:]} {
					if err := detector.Process(chunk); err != nil {
						t.Errorf("split after byte %d: %v", split, err)
						break
					}
				}
			}
		})
	}
}

func TestStreamRepetitionDetector_ConsecutiveWhitespaceLimit(t *testing.T) {
	t.Parallel()
	belowLimit := strings.Repeat("\u2003", maxConsecutiveWhitespaceRunes-1)
	for _, test := range []struct {
		name   string
		chunks []string
	}{
		{"whole run", []string{belowLimit + "\tresumed"}},
		{"across chunks", []string{belowLimit, "\nresumed"}},
		{"resets after non-whitespace", []string{belowLimit, "progress", belowLimit, "\rresumed"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			detector := NewStreamRepetitionDetector()
			last := len(test.chunks) - 1
			for _, chunk := range test.chunks[:last] {
				if err := detector.Process(chunk); err != nil {
					t.Fatalf("rejected before the whitespace limit: %v", err)
				}
			}
			if err := detector.Process(test.chunks[last]); !errors.Is(err, ErrDegenerateRepetitionLoop) {
				t.Fatalf("expected ErrDegenerateRepetitionLoop at the whitespace limit, got %v", err)
			}
		})
	}
}

func TestStreamRepetitionDetector_SingleRuneRepetition(t *testing.T) {
	t.Parallel()
	d := NewStreamRepetitionDetector()
	// Feed 50 slashes
	if err := d.Process(strings.Repeat("/", 50)); err != nil {
		t.Fatalf("unexpected error on 50 slashes: %v", err)
	}

	// Feed 50 more slashes -> reaches 100
	err := d.Process(strings.Repeat("/", 50))
	if err == nil {
		t.Fatal("expected error on 100 consecutive slashes, got nil")
	}
	if !errors.Is(err, ErrDegenerateRepetitionLoop) {
		t.Fatalf("expected ErrDegenerateRepetitionLoop, got: %v", err)
	}
	if !strings.Contains(err.Error(), "repeated 100 times") {
		t.Errorf("error message = %q, want mention of 100 times", err.Error())
	}
}

func TestStreamRepetitionDetector_ShortPatternRepetition(t *testing.T) {
	t.Parallel()
	d := NewStreamRepetitionDetector()
	pattern := "/ " // 2 runes
	// 40 repeats
	for i := 0; i < 39; i++ {
		if err := d.Process(pattern); err != nil {
			t.Fatalf("unexpected error before threshold: %v", err)
		}
	}

	err := d.Process(pattern) // 40th repeat
	if err == nil {
		t.Fatal("expected error on 40th repeat of 2-rune pattern, got nil")
	}
	if !errors.Is(err, ErrDegenerateRepetitionLoop) {
		t.Fatalf("expected ErrDegenerateRepetitionLoop, got: %v", err)
	}
}

func TestStreamRepetitionDetector_PhraseRepetition(t *testing.T) {
	t.Parallel()
	d := NewStreamRepetitionDetector()
	phrase := "thinking... " // 12 runes
	for i := 0; i < 14; i++ {
		if err := d.Process(phrase); err != nil {
			t.Fatalf("unexpected error before threshold: %v", err)
		}
	}

	err := d.Process(phrase) // 15th repeat
	if err == nil {
		t.Fatal("expected error on 15th repeat of phrase, got nil")
	}
	if !errors.Is(err, ErrDegenerateRepetitionLoop) {
		t.Fatalf("expected ErrDegenerateRepetitionLoop, got: %v", err)
	}
}

func TestStreamRepetitionDetector_LongPhraseRepetition(t *testing.T) {
	t.Parallel()
	d := NewStreamRepetitionDetector()
	phrase := "The quick brown fox jumps over the lazy dog. " // 45 runes
	for i := 0; i < 7; i++ {
		if err := d.Process(phrase); err != nil {
			t.Fatalf("unexpected error before threshold: %v", err)
		}
	}

	err := d.Process(phrase) // 8th repeat
	if err == nil {
		t.Fatal("expected error on 8th repeat of long phrase, got nil")
	}
	if !errors.Is(err, ErrDegenerateRepetitionLoop) {
		t.Fatalf("expected ErrDegenerateRepetitionLoop, got: %v", err)
	}
}
