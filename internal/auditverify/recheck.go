package auditverify

import (
	"context"
	"fmt"

	"github.com/kjelly/hufu/internal/team"
)

// recheckable reports whether a verification type may be safely re-executed
// read-only: no shell command, no task-attempt tool-call history, and no
// coordinator workset group state required. spec.md §17 additionally allows
// re-running command_exit when the persisted receipt is marked
// recheck_safe; that marking does not exist yet (it is introduced by a later
// work package's Verifier Metadata addition), so command_exit is excluded
// here rather than assumed safe.
func recheckable(t team.VerificationType) bool {
	switch t {
	case team.VerifyFileExists, team.VerifyFileAbsent, team.VerifyJSONAssert:
		return true
	default:
		return false
	}
}

// recheckOutcome is the result of attempting to reproduce one persisted
// VerificationResult against current on-disk state.
type recheckOutcome struct {
	attempted  bool
	reproduced bool
	detail     string
}

// recheckVerification re-executes a persisted verification's spec via
// team.ExecuteVerificationSpecWithTaskResult -- the same production verifier
// entry point used at run time -- and reports whether the outcome still
// matches what was persisted. It never re-implements assertion evaluation;
// it only decides whether recheck is applicable and compares pass/fail
// polarity.
func recheckVerification(ctx context.Context, persisted *team.VerificationResult) recheckOutcome {
	if persisted == nil || persisted.Spec == nil {
		return recheckOutcome{detail: "no verification spec was persisted for this evidence entry"}
	}
	if !recheckable(persisted.Spec.Type) {
		return recheckOutcome{detail: fmt.Sprintf("recheck unsupported for verification type %q", persisted.Spec.Type)}
	}
	wantPass := persisted.ExitCode == 0 && !persisted.Overturned
	_, err := team.ExecuteVerificationSpecWithTaskResult(ctx, "", persisted.WorkDir, *persisted.Spec, nil)
	gotPass := err == nil
	if gotPass == wantPass {
		return recheckOutcome{attempted: true, reproduced: true}
	}
	return recheckOutcome{
		attempted: true,
		detail:    fmt.Sprintf("recheck no longer reproduces persisted result (persisted pass=%v, now pass=%v)", wantPass, gotPass),
	}
}

// collectRecheckTargets gathers every persisted VerificationResult reachable
// from the run's canonical projection: acceptance criterion evidence and
// every task attempt's own verify result. Duplicates (the same *pointer
// object referenced twice, e.g. a task's VerifyResult and its
// ExecutionReceipt's VerifyResult) are collapsed by fingerprint when
// available.
func collectRecheckTargets(acceptance *team.AcceptanceResult, tasks []*team.TodoItem) []*team.VerificationResult {
	var targets []*team.VerificationResult
	seen := make(map[string]bool)
	add := func(vr *team.VerificationResult) {
		if vr == nil {
			return
		}
		key := vr.Fingerprint
		if key == "" {
			// No fingerprint (very old schema) means we cannot dedupe by
			// identity; keep the entry rather than risk silently dropping it.
			targets = append(targets, vr)
			return
		}
		if seen[key] {
			return
		}
		seen[key] = true
		targets = append(targets, vr)
	}
	if acceptance != nil {
		for _, cr := range acceptance.CriterionResults {
			for _, ev := range cr.Evidence {
				add(ev)
			}
		}
		for _, ev := range acceptance.VerificationEvidence {
			add(ev)
		}
	}
	for _, item := range tasks {
		if item == nil {
			continue
		}
		add(item.VerifyResult)
		for i := range item.ExecutionReceipts {
			add(item.ExecutionReceipts[i].VerifyResult)
		}
	}
	return targets
}
