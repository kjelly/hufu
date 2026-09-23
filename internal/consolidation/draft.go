// Package consolidation drafts one generalized memory from several confirmed
// source memories. It only produces candidate text: persistence, review, and
// approval stay with hufu context consolidate and consolidation approve.
package consolidation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/utils"
)

const (
	// MaxDraftRunes bounds the consolidated memory text.
	MaxDraftRunes = 2000
	// MaxSourceRunes bounds the combined source content sent to the model,
	// far below the 24000-rune auxiliary prompt truncation, so the JSON
	// instructions are never cut off.
	MaxSourceRunes = 16000
)

// ErrInvalidDraft wraps model output that is not one strict JSON object.
var ErrInvalidDraft = errors.New("invalid consolidation draft")

type DraftSource struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

type DraftResult struct {
	Text             string   `json:"text"`
	CoveredSourceIDs []string `json:"covered_source_ids"`
}

type TextGenerator interface {
	GenerateText(ctx context.Context, prompt string) (string, error)
}

type JSONDrafter struct{ Generator TextGenerator }

// SourceRunes is the combined content size of sources.
func SourceRunes(sources []DraftSource) int {
	total := 0
	for _, source := range sources {
		total += utf8.RuneCountInString(source.Content)
	}
	return total
}

func (d JSONDrafter) Prompt(sources []DraftSource) string {
	input, _ := json.Marshal(map[string][]DraftSource{"sources": sources})
	return fmt.Sprintf(`Merge these confirmed long-term memories of one software project team into one self-contained memory that keeps every verified fact and procedure they share. Do not invent facts, steps, or credentials, and do not add anything the sources do not support. Return ONLY one JSON object with exactly these fields: text, covered_source_ids. text is at most %d characters. covered_source_ids lists the id of every source you merged. Input: %s`, MaxDraftRunes, input)
}

// Draft asks the model for a consolidated memory and decodes it strictly: no
// code fence, no unknown fields, no trailing data.
func (d JSONDrafter) Draft(ctx context.Context, sources []DraftSource) (DraftResult, error) {
	if d.Generator == nil {
		return DraftResult{}, errors.New("consolidation drafter is not configured")
	}
	raw, err := d.Generator.GenerateText(ctx, d.Prompt(sources))
	if err != nil {
		return DraftResult{}, err
	}
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		return DraftResult{}, fmt.Errorf("%w: fenced JSON", ErrInvalidDraft)
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var out DraftResult
	if err = dec.Decode(&out); err != nil {
		return DraftResult{}, fmt.Errorf("%w: %v", ErrInvalidDraft, err)
	}
	var extra any
	if err = dec.Decode(&extra); err != io.EOF {
		return DraftResult{}, fmt.Errorf("%w: trailing JSON", ErrInvalidDraft)
	}
	return out, nil
}

// ValidateDraft requires non-empty text of at most MaxDraftRunes runes, no
// secret-like material, text that is not a verbatim copy of one source, and
// covered source IDs exactly equal to the source ID set.
func ValidateDraft(result DraftResult, sources []DraftSource) error {
	text := strings.TrimSpace(result.Text)
	if text == "" {
		return fmt.Errorf("%w: empty text", ErrInvalidDraft)
	}
	if n := utf8.RuneCountInString(text); n > MaxDraftRunes {
		return fmt.Errorf("%w: text has %d runes, limit %d", ErrInvalidDraft, n, MaxDraftRunes)
	}
	if utils.RedactSecrets(text) != text || strings.Contains(text, "[REDACTED]") || strings.Contains(text, "<REDACTED:") {
		return fmt.Errorf("%w: text contains secret-like material", ErrInvalidDraft)
	}
	want := make([]string, 0, len(sources))
	for _, source := range sources {
		if strings.TrimSpace(source.Content) == text {
			return fmt.Errorf("%w: text copies source %s verbatim", ErrInvalidDraft, source.ID)
		}
		want = append(want, source.ID)
	}
	got := append([]string(nil), result.CoveredSourceIDs...)
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		return fmt.Errorf("%w: covered_source_ids %v must equal the sources %v", ErrInvalidDraft, got, want)
	}
	return nil
}
