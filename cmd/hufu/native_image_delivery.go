package main

import (
	"fmt"

	"github.com/kjelly/hufu/internal/team"
)

// Native direct work has already consumed its explicit result view inside the
// original invocation. Its outcome cannot be promoted through another Run or
// an automatic coordinator replan that would inherit the media conversation.
func presentNativeImageDirectResult(tc *teamContext, result *team.DirectAgentResult) (string, error) {
	if tc == nil || tc.coordinator == nil || result == nil {
		return "", fmt.Errorf("direct image execution returned no canonical result")
	}
	outcome := tc.coordinator.LastRunResult()
	if outcome == nil {
		return result.Output, fmt.Errorf("direct image execution has no terminal outcome; automatic replan is unsupported")
	}
	if team.IsRunOutcomeSuccess(outcome.Outcome) && outcome.GoalSatisfied {
		if result.Error != nil || result.ReplanRequired {
			return result.Output, fmt.Errorf("direct image result disagrees with its terminal outcome")
		}
		return result.Output, nil
	}
	return presentExploratoryDelivery(tc, result.Output, &team.RunOutcomeError{Result: outcome})
}
