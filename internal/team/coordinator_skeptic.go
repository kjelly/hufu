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
	maxSkeptics                  = 3
	skepticTimeout               = 60 * time.Second
	skepticGoalMaxRunes          = 1000
	skepticConstraintsMaxRunes   = 1000
	skepticVerifyCommandMaxRunes = 500
	skepticGoalWeight            = 2
	skepticSupportingFieldWeight = 1
	skepticClaimedResultWeight   = 5
)

type skepticVoteStatus uint8

const (
	skepticVoteAbstained skepticVoteStatus = iota
	skepticVoteCompleted
)

type skepticVote struct {
	Refuted bool   `json:"refuted"`
	Reason  string `json:"reason"`
	status  skepticVoteStatus
}

type skepticTally struct {
	Refuted     bool
	Reason      string
	Signal      string
	Abstentions int
}

func parseSkepticVote(response string) (skepticVote, error) {
	var v skepticVote
	if err := json.Unmarshal([]byte(extractJSONPayload(response)), &v); err != nil {
		return skepticVote{}, fmt.Errorf("parse skeptic vote: %w", err)
	}
	v.status = skepticVoteCompleted
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

func buildSkepticPrompt(lens, goal, constraints, output, verifyCmd string) (string, error) {
	prefix := fmt.Sprintf("You are a skeptical reviewer focusing on %s. Your job is to REFUTE the claim that the output below achieves the goal.\n\n", lens)
	sections := []sidecar.PromptSection{
		{Header: "## Goal\n\n", Content: goal, Footer: "\n\n", Weight: skepticGoalWeight, MaxRunes: skepticGoalMaxRunes},
	}
	if constraints != "" {
		sections = append(sections, sidecar.PromptSection{
			Header: "## Constraints\n\n", Content: constraints, Footer: "\n\n",
			Weight: skepticSupportingFieldWeight, MaxRunes: skepticConstraintsMaxRunes,
		})
	}
	if verifyCmd != "" {
		sections = append(sections, sidecar.PromptSection{
			Header: "## Objective check already passed\n\nThe shell command `", Content: verifyCmd, Footer: "` exited 0.\n\n",
			Weight: skepticSupportingFieldWeight, MaxRunes: skepticVerifyCommandMaxRunes,
		})
	}
	sections = append(sections, sidecar.PromptSection{
		Header: "## Claimed result\n\n", Content: output, Footer: "\n\n", Weight: skepticClaimedResultWeight,
	})
	suffix := "Only refute with concrete, specific evidence from the output — vague doubts do not count. " +
		"Respond with STRICT JSON only, no other text:\n" +
		`{"refuted": <true|false>, "reason": "<one sentence citing the evidence>"}`
	return sidecar.BuildBoundedPrompt(prefix, sections, suffix, sidecar.ClassifierProfile.InputRuneLimit())
}

// tallySkepticVotes applies majority rule: the result fails only when strictly
// more votes refute than confirm. Abstentions (zero-valued votes from errored
// skeptics) count as confirmations — fail-open, so a broken skeptic model can
// never deadlock tasks.
func tallySkepticVotes(votes []skepticVote) skepticTally {
	refutedCount := 0
	completedCount := 0
	var reasons []string
	for _, v := range votes {
		if v.status != skepticVoteCompleted {
			continue
		}
		completedCount++
		if v.Refuted {
			refutedCount++
			if r := strings.TrimSpace(v.Reason); r != "" {
				reasons = append(reasons, r)
			}
		}
	}
	tally := skepticTally{Abstentions: len(votes) - completedCount}
	if refutedCount <= len(votes)-refutedCount {
		switch {
		case completedCount == 0:
			tally.Signal = "abstained"
		case tally.Abstentions > 0:
			tally.Signal = "confirmed_with_abstentions"
		default:
			tally.Signal = "confirmed"
		}
		return tally
	}
	tally.Refuted = true
	tally.Signal = "refuted"
	tally.Reason = strings.Join(reasons, "; ")
	if tally.Reason == "" {
		tally.Reason = "majority of skeptics refuted the result"
	}
	return tally
}

// adversarialVerify runs the task's skeptic votes concurrently and returns a
// non-nil error when a majority refutes the result. It is a silent no-op when
// the task does not request it or no sidecar model is configured (the same
// degradation as reflectOnFailure).
func (c *Coordinator) adversarialVerify(parentCtx context.Context, task TaskDef, todoID, output string) error {
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
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(parentCtx, skepticTimeout)
			defer cancel()
			prompt, err := buildSkepticPrompt(lens, task.Goal, task.Constraints, output, task.Verify)
			if err != nil {
				return
			}
			resp, err := s.Execute(sidecar.WithPurpose(ctx, "skeptic"), prompt)
			if err != nil {
				return // abstain (zero value = confirm)
			}
			v, err := parseSkepticVote(resp)
			if err != nil {
				return // abstain
			}
			votes[i] = v
		})
	}
	wg.Wait()

	tally := tallySkepticVotes(votes)
	if tally.Abstentions > 0 {
		c.report(c.newEvent("step").withMessage(fmt.Sprintf("skeptic review degraded: %d of %d votes abstained", tally.Abstentions, len(votes))).withTodoID(todoID))
	}
	if tally.Refuted {
		if err := c.recordAuxiliaryContextSignal(todoID, "skeptic", "skeptic_signal", tally.Signal); err != nil {
			return err
		}
		return fmt.Errorf("adversarial verification refuted the result: %s", utils.TruncateString(tally.Reason, 500))
	}
	if err := c.recordAuxiliaryContextSignal(todoID, "skeptic", "skeptic_signal", tally.Signal); err != nil {
		return err
	}
	return nil
}
