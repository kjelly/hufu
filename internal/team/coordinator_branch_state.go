package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// updateBranchState snapshots the coordinator's live state (task plan, active
// model, selected team, latest compaction summary) into the active session
// branch, so `hufu session` checkout/time-travel can restore it later (§8).
// Best-effort: any failure leaves the checkpoint path unaffected. The task
// plan is read from a session snapshot so a concurrent checkpoint cannot race
// the read.
//
// Rewriting session_tree.json is the most expensive step of a checkpoint. The
// tree keeps one branch per run, and SaveSessionTree redacts the whole
// document on every write. Checkpoints fire on every task mutation, and most
// of them do not change what the branch records, so the tree is rewritten only
// when that state changes. branchStateMu serializes the load-modify-save:
// checkpoints from parallel tasks would otherwise overwrite each other's
// update.
func (c *Coordinator) updateBranchState() {
	// Take the snapshot under the lock too: a snapshot taken before it could
	// be written after a newer one and roll the branch back.
	c.branchStateMu.Lock()
	defer c.branchStateMu.Unlock()
	var plan []*TodoItem
	c.viewSessionData(func(sd *SessionData) {
		if len(sd.Tasks) > 0 {
			plan = make([]*TodoItem, len(sd.Tasks))
			for i, t := range sd.Tasks {
				plan[i] = cloneTodoItem(t)
			}
		}
	})
	model := c.session.Config.Generation.Model
	teamName := c.session.Config.Name
	var compaction *StructuredSummary
	if c.lastCompactionSummary != nil {
		compaction = cloneStructuredSummary(c.lastCompactionSummary)
	}
	fingerprint := branchStateFingerprint(plan, model, teamName, compaction)
	if fingerprint != "" && fingerprint == c.branchStateFingerprint {
		return
	}
	st, err := LoadSessionTree(c.session.Workspace)
	if err != nil {
		return
	}
	b := st.Branches[st.ActiveBranch]
	if b == nil {
		return
	}
	if len(plan) > 0 {
		b.State.TaskPlan = plan
	}
	b.State.ActiveModel = model
	b.State.SelectedTeam = teamName
	if compaction != nil {
		b.State.Compaction = compaction
	}
	if err := SaveSessionTree(c.session.Workspace, st); err != nil {
		return
	}
	c.branchStateFingerprint = fingerprint
}

// branchStateFingerprint summarizes the branch state updateBranchState
// records. It covers each task's identity and lifecycle position rather than
// its full content: those fields change whenever a task advances, retries,
// produces output, or is resolved. Detail that changes without any of them
// waits for the next change; `hufu session` also refreshes the active
// branch from session.json before it switches branches.
func branchStateFingerprint(plan []*TodoItem, model, teamName string, compaction *StructuredSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "model=%q team=%q\n", model, teamName)
	for _, item := range plan {
		if item == nil {
			b.WriteString("nil\n")
			continue
		}
		resolution := ""
		if item.Resolution != nil {
			resolution = item.Resolution.Status
		}
		fmt.Fprintf(&b, "task=%q status=%q retries=%d desc=%d output=%d typed=%t verify=%t resolution=%q dispatch=%q\n",
			item.ID, item.Status, item.Retries, len(item.Desc), len(item.Output),
			item.TypedResult != nil, item.VerifyResult != nil, resolution, item.DispatchID)
	}
	if compaction != nil {
		data, err := json.Marshal(compaction)
		if err != nil {
			// A summary that cannot be marshaled cannot be compared either.
			// The empty fingerprint never matches, so the tree is written.
			return ""
		}
		b.WriteString("compaction=")
		b.Write(data)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
