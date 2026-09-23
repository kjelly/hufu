package memoryconflict

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	contextstore "github.com/kjelly/hufu/internal/context"
)

// ErrInvalidJudgment wraps judge output that is not one strict JSON object
// with a known verdict. Transport and model errors are returned unwrapped.
var ErrInvalidJudgment = errors.New("invalid conflict judgment")

// TextGenerator runs one prompt; the CLI adapts a sidecar to it.
type TextGenerator interface {
	GenerateText(ctx context.Context, prompt string) (string, error)
}

type Judgment struct {
	Verdict   contextstore.PairVerdict `json:"verdict"`
	Rationale string                   `json:"rationale"`
}

type Judge interface {
	// Prompt is the exact text Judge sends, so callers can check its size.
	Prompt(a, b contextstore.ContextItem) string
	Judge(ctx context.Context, a, b contextstore.ContextItem) (Judgment, error)
}

// JSONJudge asks a model for a strict JSON verdict. Changing the prompt or
// its rules requires bumping contextstore.CurrentConflictJudgePolicyVersion.
type JSONJudge struct{ Generator TextGenerator }

type judgeMemory struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

func (j JSONJudge) Prompt(a, b contextstore.ContextItem) string {
	input, _ := json.Marshal(map[string]judgeMemory{
		"memory_a": {ID: a.ID, Kind: string(a.Kind), Content: a.Content},
		"memory_b": {ID: b.ID, Kind: string(b.Kind), Content: b.Content},
	})
	return `Compare two long-term memories of one software project team. Decide whether following both would give incompatible guidance about the same subject. Return ONLY one JSON object with exactly these fields: verdict, rationale. verdict must be one of: "contradicts" (following both is impossible or gives opposite guidance about the same subject), "duplicate" (same guidance), "refines" (one narrows or extends the other without conflict), "compatible" (different subjects, or both can be followed). rationale is at most 300 characters, cites no credentials, and does not quote either memory in full. Input: ` + string(input)
}

func (j JSONJudge) Judge(ctx context.Context, a, b contextstore.ContextItem) (Judgment, error) {
	if j.Generator == nil {
		return Judgment{}, errors.New("conflict judge is not configured")
	}
	raw, err := j.Generator.GenerateText(ctx, j.Prompt(a, b))
	if err != nil {
		return Judgment{}, err
	}
	return parseJudgment(raw)
}

// parseJudgment applies the same strict decoding as the promotion draft
// generator: no code fence, no unknown fields, no trailing data.
func parseJudgment(raw string) (Judgment, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		return Judgment{}, fmt.Errorf("%w: fenced JSON", ErrInvalidJudgment)
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var out Judgment
	if err := dec.Decode(&out); err != nil {
		return Judgment{}, fmt.Errorf("%w: %v", ErrInvalidJudgment, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Judgment{}, fmt.Errorf("%w: trailing JSON", ErrInvalidJudgment)
	}
	switch out.Verdict {
	case contextstore.PairVerdictContradicts, contextstore.PairVerdictCompatible, contextstore.PairVerdictDuplicate, contextstore.PairVerdictRefines:
		return out, nil
	default:
		return Judgment{}, fmt.Errorf("%w: unknown verdict %q", ErrInvalidJudgment, out.Verdict)
	}
}
