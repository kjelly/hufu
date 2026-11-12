package taskdistill

import (
	"context"
	"fmt"
	"slices"
)

const fragmentGuidance = "These are ordered fragments of larger sources, possibly split inside JSON or text. Do not infer missing context or claim a verification passed without its evidence. Retain ambiguous or conflicting fragments. Operator policy: "

func extractionPolicy(sources []Source, batches [][]Source, policy string) string {
	if len(batches) == 1 && slices.Equal(batches[0], sources) {
		return policy
	}
	return fragmentGuidance + policy
}

// ProbeBatch validates only the selected extraction batch, including its
// bounded correction/subdivision. It shares normal extraction checkpoint keys,
// but returns no whole-source proposal and never performs consolidation/cleanup.
func ProbeBatch(ctx context.Context, generator TextGenerator, sources []Source, policy string, batch, maxCalls int) (Draft, error) {
	if maxCalls < 1 || maxCalls > MaxModelCalls {
		return Draft{}, fmt.Errorf("%w: invalid probe call budget", ErrInvalid)
	}
	batches, err := PrepareBatches(sources, policy, MaxModelCalls)
	if err != nil {
		return Draft{}, err
	}
	if batch < 1 || batch > len(batches) {
		return Draft{}, fmt.Errorf("%w: probe batch must be between 1 and %d", ErrInvalid, len(batches))
	}
	return newDraftRunner(generator, maxCalls).generate(ctx, batches[batch-1], extractionPolicy(sources, batches, policy), 0)
}
