package main

import (
	"context"
	"strings"
)

// forwardReferenceChecker re-checks a documentation reference that is missing
// at a historical reviewed revision against the repository's current commit,
// when that commit descends from the reviewed revision.
// A plan document names files and declarations that later commits add; a
// review of the range that only added the plan would otherwise fail on
// references that were never broken. A reference that is missing at both
// revisions is still a verification issue. Reviews of the working tree or of
// a range that ends at the current commit have no later state to consult.
type forwardReferenceChecker struct {
	ctx      context.Context
	repo     string
	reviewed reviewRange

	resolved bool
	tip      string
	symbols  *reviewGoSymbolResolver
}

func newForwardReferenceChecker(ctx context.Context, repo string, reviewed reviewRange) *forwardReferenceChecker {
	return &forwardReferenceChecker{ctx: ctx, repo: repo, reviewed: reviewed}
}

// tipRange returns the current commit when it descends from the reviewed
// revision. Only a descendant can provide a reference later; a commit on
// another branch that happens to contain it says nothing about the reviewed
// history. An unresolvable HEAD, an unrelated HEAD, or a failed ancestry check
// disables the check, so a missing reference stays an issue.
func (f *forwardReferenceChecker) tipRange() (reviewRange, bool) {
	if f.reviewed.isWorkingTree() {
		return reviewRange{}, false
	}
	if !f.resolved {
		f.resolved = true
		head, headErr := git(f.ctx, f.repo, "rev-parse", "--verify", "HEAD^{commit}")
		end, endErr := git(f.ctx, f.repo, "rev-parse", "--verify", f.reviewed.End+"^{commit}")
		head, end = strings.TrimSpace(head), strings.TrimSpace(end)
		if headErr == nil && endErr == nil && head != "" && head != end {
			if _, ancestryErr := git(f.ctx, f.repo, "merge-base", "--is-ancestor", end, head); ancestryErr == nil {
				f.tip = head
			}
		}
	}
	if f.tip == "" {
		return reviewRange{}, false
	}
	return reviewRange{End: f.tip}, true
}

func (f *forwardReferenceChecker) pathExists(path string) (bool, error) {
	tip, ok := f.tipRange()
	if !ok {
		return false, nil
	}
	return reviewPathExists(f.ctx, f.repo, tip, path)
}

func (f *forwardReferenceChecker) symbolExists(symbol string) (bool, error) {
	tip, ok := f.tipRange()
	if !ok {
		return false, nil
	}
	if f.symbols == nil {
		f.symbols = newReviewGoSymbolResolver(f.ctx, f.repo, tip)
	}
	return f.symbols.exists(symbol)
}

func (f *forwardReferenceChecker) close() {
	if f.symbols != nil {
		f.symbols.close()
	}
}

// recordMissingReference files a reference that is missing at the reviewed
// revision: as a forward reference when the current commit provides it, and
// otherwise as a verification issue. It is a function, not a method: Hufu's
// embedded Go interpreter encodes a struct type that has a pointer method as
// an empty JSON object, which would drop documentation_verification from the
// action outputs.
func recordMissingReference(v *documentationVerification, issue string, providedLater bool, tip string) {
	if !providedLater {
		v.Issues = append(v.Issues, issue)
		return
	}
	v.ForwardReferences = append(v.ForwardReferences, issue+"; it exists at the current commit "+tip)
	v.ReferenceTip = tip
}
