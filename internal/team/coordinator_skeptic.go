package team

// Adversarial verification: after a task succeeds (and passes its shell
// verify), N independent skeptic votes each try to REFUTE the result from a
// distinct lens. A majority refutation turns the success into a failure that
// flows into the existing retry path, where reflectOnFailure feeds the
// refutation reason back as the retry hint. Skeptics are one-shot sidecar
// calls, not full agents: a spawned agent would get memory/delegation tools
// force-injected, while the sidecar is read-only by construction.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/utils"
)

const (
	maxSkeptics           = 3
	skepticTimeout        = 60 * time.Second
	skepticOutputMaxRunes = 8000
)

type skepticVote struct {
	Refuted bool   `json:"refuted"`
	Reason  string `json:"reason"`
}

func parseSkepticVote(response string) (skepticVote, error) {
	var v skepticVote
	if err := json.Unmarshal([]byte(extractJSONPayload(response)), &v); err != nil {
		return skepticVote{}, fmt.Errorf("parse skeptic vote: %w", err)
	}
	return v, nil
}

// skepticAllLenses are the review perspectives, in assignment order. Distinct
// lenses catch failure modes that N identical reviewers would all miss.
var skepticAllLenses = []string{"correctness", "completeness", "reproducibility"}

func skepticLenses(n int) []string {
	if n < 1 {
		n = 1
	}
	if n > maxSkeptics {
		n = maxSkeptics
	}
	return skepticAllLenses[:n]
}

func buildSkepticPrompt(lens, goal, constraints, output, verifyCmd string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are a skeptical reviewer focusing on %s. Your job is to REFUTE the claim that the output below achieves the goal.\n\n", lens)
	fmt.Fprintf(&b, "## Goal\n\n%s\n\n", goal)
	if constraints != "" {
		fmt.Fprintf(&b, "## Constraints\n\n%s\n\n", constraints)
	}
	if verifyCmd != "" {
		fmt.Fprintf(&b, "## Objective check already passed\n\nThe shell command `%s` exited 0.\n\n", verifyCmd)
	}
	fmt.Fprintf(&b, "## Claimed result\n\n%s\n\n", utils.TruncateRunes(output, skepticOutputMaxRunes))
	b.WriteString("Only refute with concrete, specific evidence from the output — vague doubts do not count. ")
	b.WriteString("Respond with STRICT JSON only, no other text:\n")
	b.WriteString(`{"refuted": <true|false>, "reason": "<one sentence citing the evidence>"}`)
	return b.String()
}

// tallySkepticVotes applies majority rule: the result fails only when strictly
// more votes refute than confirm. Abstentions (zero-valued votes from errored
// skeptics) count as confirmations — fail-open, so a broken skeptic model can
// never deadlock tasks.
func tallySkepticVotes(votes []skepticVote) (refuted bool, reason string) {
	refutedCount := 0
	var reasons []string
	for _, v := range votes {
		if v.Refuted {
			refutedCount++
			if r := strings.TrimSpace(v.Reason); r != "" {
				reasons = append(reasons, r)
			}
		}
	}
	if refutedCount <= len(votes)-refutedCount {
		return false, ""
	}
	reason = strings.Join(reasons, "; ")
	if reason == "" {
		reason = "majority of skeptics refuted the result"
	}
	return true, reason
}

// adversarialVerify runs the task's skeptic votes concurrently and returns a
// non-nil error when a majority refutes the result. It is a silent no-op when
// the task does not request it or no sidecar model is configured (the same
// degradation as reflectOnFailure).
func (c *Coordinator) adversarialVerify(parentCtx context.Context, task TaskDef, output string) error {
	if task.AdversarialVerify <= 0 {
		return nil
	}
	s := c.AgentPool().Sidecar()
	if s == nil {
		_ = c.recordAuxiliaryFallback(parentCtx, "skeptic", "no_model_fallback")
		return nil
	}

	lenses := skepticLenses(task.AdversarialVerify)
	votes := make([]skepticVote, len(lenses))
	var wg sync.WaitGroup
	for i, lens := range lenses {
		wg.Add(1)
		go func(i int, lens string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(parentCtx, skepticTimeout)
			defer cancel()
			resp, err := s.Execute(sidecar.WithPurpose(ctx, "skeptic"), buildSkepticPrompt(lens, task.Goal, task.Constraints, output, task.Verify))
			if err != nil {
				return // abstain (zero value = confirm)
			}
			v, err := parseSkepticVote(resp)
			if err != nil {
				return // abstain
			}
			votes[i] = v
		}(i, lens)
	}
	wg.Wait()

	if refuted, reason := tallySkepticVotes(votes); refuted {
		return fmt.Errorf("adversarial verification refuted the result: %s", utils.TruncateString(reason, 500))
	}
	return nil
}
