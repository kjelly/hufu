package team

// Judge panel for multi-model results: instead of concatenating the outputs
// of extra-models runs, an LLM judge scores the candidates, picks the best,
// and optionally grafts useful content from runners-up. When no judge model
// is configured (or the judge fails), callers fall back to the plain
// concatenation merge. Judging operates on output text only — the per-model
// workspaces are removed right after execution.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/anomalyco/hufu/internal/utils"
)

const (
	// judgeCandidateMaxRunes caps each candidate's output in the judge prompt.
	judgeCandidateMaxRunes = 8000
	judgeTimeout           = 60 * time.Second
)

// errNoJudgeModel signals the silent fallback path: no judge is configured,
// which is a normal configuration, not a failure worth reporting.
var errNoJudgeModel = errors.New("no judge model configured")

// llmJSONBlockRe extracts a fenced ```json block from an LLM response.
var llmJSONBlockRe = regexp.MustCompile("(?s)```(?:json)?\\s*\\n?(.*?)\\n?```")

// extractJSONPayload returns the fenced JSON block if present, otherwise the
// trimmed response as-is.
func extractJSONPayload(response string) string {
	if m := llmJSONBlockRe.FindStringSubmatch(response); len(m) >= 2 {
		return strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(response)
}

type judgeGraft struct {
	FromIndex int    `json:"from_index"`
	Content   string `json:"content"`
}

type judgeVerdict struct {
	BestIndex int          `json:"best_index"`
	Reason    string       `json:"reason"`
	Grafts    []judgeGraft `json:"grafts,omitempty"`
}

// parseJudgeVerdict parses the judge's JSON response. An out-of-range
// best_index is an error; out-of-range or empty grafts are silently dropped.
func parseJudgeVerdict(response string, numCandidates int) (judgeVerdict, error) {
	var v judgeVerdict
	if err := json.Unmarshal([]byte(extractJSONPayload(response)), &v); err != nil {
		return judgeVerdict{}, fmt.Errorf("parse judge verdict: %w", err)
	}
	if v.BestIndex < 0 || v.BestIndex >= numCandidates {
		return judgeVerdict{}, fmt.Errorf("parse judge verdict: best_index %d out of range [0,%d)", v.BestIndex, numCandidates)
	}
	valid := v.Grafts[:0]
	for _, g := range v.Grafts {
		if g.FromIndex >= 0 && g.FromIndex < numCandidates && g.FromIndex != v.BestIndex && strings.TrimSpace(g.Content) != "" {
			valid = append(valid, g)
		}
	}
	v.Grafts = valid
	return v, nil
}

// buildJudgePrompt renders the comparison prompt. candidates must all be
// successful results (err == nil).
func buildJudgePrompt(goal string, candidates []*agentResult) string {
	var b strings.Builder
	b.WriteString("You are a strict judge comparing candidate outputs produced by different models for the same task.\n\n")
	fmt.Fprintf(&b, "## Task\n\n%s\n\n## Candidates\n\n", goal)
	for i, r := range candidates {
		fmt.Fprintf(&b, "### Candidate %d (model: %s)\n\n%s\n\n", i, r.model, utils.TruncateRunes(r.output, judgeCandidateMaxRunes))
	}
	b.WriteString("Score each candidate on correctness, completeness, and clarity, then pick the single best one. ")
	b.WriteString("If a runner-up contains genuinely useful content the winner lacks, include it as a graft.\n\n")
	b.WriteString("Respond with STRICT JSON only, no other text:\n")
	b.WriteString(`{"best_index": <0-based index>, "reason": "<one sentence>", "grafts": [{"from_index": <index>, "content": "<useful content the winner lacks>"}]}`)
	b.WriteString("\nThe grafts array may be empty.")
	return b.String()
}

// composeJudgedOutput assembles the final output: winner first, then any
// grafted sections, then a one-line judge footer.
func composeJudgedOutput(candidates []*agentResult, v judgeVerdict) string {
	var b strings.Builder
	winner := candidates[v.BestIndex]
	b.WriteString(winner.output)
	for _, g := range v.Grafts {
		fmt.Fprintf(&b, "\n\n## Grafted from %s\n\n%s", candidates[g.FromIndex].model, g.Content)
	}
	if reason := strings.TrimSpace(v.Reason); reason != "" {
		fmt.Fprintf(&b, "\n\n_Judge: selected %s — %s_", winner.model, reason)
	}
	return b.String()
}

// judgeAgentResults runs the judge over the successful candidates and returns
// the composed winner. It errors when no judge model is configured
// (errNoJudgeModel), when no candidate succeeded, or when the judge call or
// its response parsing fails — in all cases the caller falls back to
// mergeAgentResults.
func (c *Coordinator) judgeAgentResults(ctx context.Context, goal, todoID string, results []*agentResult) (string, error) {
	s := c.JudgeSidecar()
	if s == nil {
		return "", fmt.Errorf("judge agent results: %w", errNoJudgeModel)
	}

	var valid []*agentResult
	for _, r := range results {
		if r != nil && r.err == nil && strings.TrimSpace(r.output) != "" {
			valid = append(valid, r)
		}
	}
	if len(valid) == 0 {
		return "", fmt.Errorf("judge agent results: no successful candidates")
	}
	if len(valid) == 1 {
		return valid[0].output, nil
	}

	judgeCtx, cancel := context.WithTimeout(ctx, judgeTimeout)
	defer cancel()
	c.report(c.newEvent("sidecar_call").withMessage("judge"))
	response, err := s.Execute(judgeCtx, buildJudgePrompt(goal, valid))
	if err != nil {
		return "", fmt.Errorf("judge agent results: %w", err)
	}
	verdict, err := parseJudgeVerdict(response, len(valid))
	if err != nil {
		return "", fmt.Errorf("judge agent results: %w", err)
	}
	c.report(c.newEvent("judge").withModel(valid[verdict.BestIndex].model).withMessage(verdict.Reason).withTodoID(todoID))
	return composeJudgedOutput(valid, verdict), nil
}
