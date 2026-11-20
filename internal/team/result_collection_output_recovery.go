package team

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/kjelly/hufu/internal/agent"
)

type resultRepairMaxOutputKey struct{}

// WithResultRepairMaxOutput selects the ceiling for new collector requests
// after the operator's bounded grant. The frozen worker receipt is unchanged.
func WithResultRepairMaxOutput(ctx context.Context, tokens int) context.Context {
	return context.WithValue(ctx, resultRepairMaxOutputKey{}, tokens)
}

const maxResultRepairOutputTokens = 1 << 20

func (c *Coordinator) validateCollectionOutputIncrease(item *TodoItem, work *resultCollectionWork, proposed int) error {
	if proposed < 1 || proposed > maxResultRepairOutputTokens {
		return fmt.Errorf("collector output limit must be between 1 and %d tokens", maxResultRepairOutputTokens)
	}
	var generation, teamGeneration agent.GenerationParams
	if work.GenerationCaptured {
		generation, teamGeneration = work.Generation, work.TeamGeneration
	} else {
		if def := c.session.Agents[item.Agent]; def != nil {
			generation = def.Generation
		}
		teamGeneration = c.session.Config.Generation
	}
	original, _ := strconv.Atoi(generation.MaxTokens)
	if original <= 0 {
		original, _ = strconv.Atoi(teamGeneration.MaxTokens)
	}
	if original > 0 && proposed <= original {
		return fmt.Errorf("collector output limit %d must exceed the original %d", proposed, original)
	}
	return nil
}

func (c *Coordinator) applyCollectionRecoveryOutputLimit(generation *agent.GenerationParams, checkpoint *ResultCollectionCheckpoint) {
	grants := collectionRepairGrants(collectionReceipt(c.todoItemByID(checkpoint.TaskID), checkpoint))
	for _, grant := range slices.Backward(grants) {
		if grant.BranchID == checkpoint.BranchID && grant.Recovery != nil &&
			checkpoint.TurnsUsed >= grant.AfterAttempts && checkpoint.TurnsUsed-grant.AfterAttempts < grant.AdditionalAttempts {
			if grant.Recovery.MaxOutputTokens > 0 {
				generation.MaxTokens = strconv.Itoa(grant.Recovery.MaxOutputTokens)
			}
			return
		}
	}
}
