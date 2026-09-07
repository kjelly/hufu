package team

import (
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

// Goal-driven routing hints (spec.md v2 §16, §30-31).
//
// A hint widens a routed role's preferred-capability list for one specific
// decision, based on the task's own question text — never its required
// list, so a hint can only ever change which already-qualified candidate
// ranks highest, never grant eligibility to one that failed the
// required-capability check.

// applyRoutingHints returns preferred augmented with every hint whose
// selector matches question. It never mutates preferred and is pure: the
// same (hints, question, preferred) always produces the same result, which
// is what lets the JUDGE and REVISE construction sites apply it
// independently and still land on the same resolved candidate.
func applyRoutingHints(hints []agent.RoutingHint, question string, preferred []string) []string {
	if len(hints) == 0 {
		return preferred
	}
	augmented := append([]string(nil), preferred...)
	for _, hint := range hints {
		if hint.WhenGoalContains == "" || !strings.Contains(question, hint.WhenGoalContains) {
			continue
		}
		augmented = append(augmented, hint.PreferredCapabilities...)
	}
	return augmented
}

// hintedReferenceRole returns role with PreferredCapabilities augmented by
// any matching hint, or role unchanged if there are none to apply. It never
// modifies the value role points to.
func hintedReferenceRole(role *agent.ReferenceRolePolicy, hints []agent.RoutingHint, question string) *agent.ReferenceRolePolicy {
	if role == nil || len(hints) == 0 {
		return role
	}
	augmented := *role
	augmented.PreferredCapabilities = applyRoutingHints(hints, question, role.PreferredCapabilities)
	return &augmented
}

// hintedJudgeRole mirrors hintedReferenceRole for JudgeRolePolicy. It is
// also what REVISE uses (with the same hints and question JUDGE round 1
// saw) so a revision resolves to the identical binding rather than a fresh
// one influenced by a different effective preferred list.
func hintedJudgeRole(role *agent.JudgeRolePolicy, hints []agent.RoutingHint, question string) *agent.JudgeRolePolicy {
	if role == nil || len(hints) == 0 {
		return role
	}
	augmented := *role
	augmented.PreferredCapabilities = applyRoutingHints(hints, question, role.PreferredCapabilities)
	return &augmented
}

// hintedChallengeRole mirrors hintedReferenceRole for ChallengeRolePolicy.
func hintedChallengeRole(role *agent.ChallengeRolePolicy, hints []agent.RoutingHint, question string) *agent.ChallengeRolePolicy {
	if role == nil || len(hints) == 0 {
		return role
	}
	augmented := *role
	augmented.PreferredCapabilities = applyRoutingHints(hints, question, role.PreferredCapabilities)
	return &augmented
}
