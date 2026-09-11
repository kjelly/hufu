package team

import (
	"sort"

	"github.com/kjelly/hufu/internal/agent"
)

// LockedResourceContextItems converts every locked resource injectable into
// forAgent into an immutable ContextItem (spec.md §8). Content always comes
// from LoadedResource.Content — the exact bytes read at lock time — this
// function never touches disk, so it can never re-read a source that
// changed after locking (runtime invariant 11).
//
// A resource whose InjectInto list does not name forAgent produces no item
// for that agent; it is still locked and hash-verified, just not surfaced
// as context there. An empty InjectInto list means the resource is locked
// but never injected anywhere — a team declaring one gets admission/hash
// enforcement without any context effect.
//
// Marking every item Required:true and Authority:ContextAuthorityNormative
// means the existing ContextCompiler pipeline (ValidateRequiredItems,
// BudgetContextItems — context_compiler.go) already fails closed if one
// cannot fit; no separate budget-gate logic is needed (spec.md §8).
func LockedResourceContextItems(loaded []*LoadedResource, forAgent string) []ContextItem {
	items := make([]ContextItem, 0, len(loaded))
	for _, lr := range loaded {
		if lr == nil || !sliceContainsString(lr.InjectInto, forAgent) {
			continue
		}
		id := "locked_resource:" + lr.Name
		items = append(items, ContextItem{
			ID:          id,
			Kind:        "locked_resource",
			Content:     lr.Content,
			Source:      "resource_lock",
			Priority:    PriorityHardConstraints,
			Required:    true,
			Authority:   ContextAuthorityNormative,
			DedupKey:    hashContentKey(lr.Content),
			ConflictKey: id,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

func sliceContainsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// InjectLockedRequiredResources appends every locked resource injectable
// into forAgent to items. It is the join point a future context-assembly
// call site (CompileCoordinatorContext / CompileWorkerContext) wires in;
// nothing calls it yet — which call sites need it, and in what order
// relative to other context sources, is left as a follow-up decision
// (spec.md §10) rather than guessed at here.
func (c *Coordinator) InjectLockedRequiredResources(items []ContextItem, forAgent string) []ContextItem {
	if c == nil {
		return items
	}
	return append(items, LockedResourceContextItems(c.LoadedRequiredResources(), forAgent)...)
}

// LockedSkillContent returns the immutable locked content for a required
// skill resource, if one was declared and locked under that name. Skill
// progressive disclosure (coordinator_skills.go) can call this to source
// disclosed content from the locked snapshot instead of reading the skill
// file itself (spec.md §8: "Skill 沿用目前 disclosure；來源改為 locked
// snapshot") — that wiring decision is deferred; this only provides the
// lookup.
func LockedSkillContent(loaded []*LoadedResource, skillName string) (string, bool) {
	for _, lr := range loaded {
		if lr != nil && lr.Kind == agent.ResourceSkill && lr.Name == skillName {
			return lr.Content, true
		}
	}
	return "", false
}
