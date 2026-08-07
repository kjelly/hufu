package team

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const sessionTreeFile = "session_tree.json"

// BranchState captures branch-scoped metadata and snapshot configuration.
type BranchState struct {
	ActiveModel   string             `json:"active_model,omitempty"`
	SelectedTeam  string             `json:"selected_team,omitempty"`
	ThinkingLevel string             `json:"thinking_level,omitempty"`
	ActiveTools   []string           `json:"active_tools,omitempty"`
	TaskPlan      []*TodoItem        `json:"task_plan,omitempty"`
	Compaction    *StructuredSummary `json:"compaction,omitempty"`
	Labels        map[string]string  `json:"labels,omitempty"`
	Artifacts     []string           `json:"artifacts,omitempty"`
}

// SessionBranch represents a single branch in the session tree.
type SessionBranch struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	ParentID    string      `json:"parent_id,omitempty"`
	ForkEventID string      `json:"fork_event_id,omitempty"`
	CreatedAt   string      `json:"created_at"`
	State       BranchState `json:"state"`
}

// SessionTree manages session branches, fork points, checkpoints, and time travel.
type SessionTree struct {
	ActiveBranch string                    `json:"active_branch"`
	Branches     map[string]*SessionBranch `json:"branches"`
	Labels       map[string]string         `json:"labels"` // label name -> target (event ID, branch ID, or entry ID)
}

// NewSessionTree returns a new SessionTree initialized with a default "main" branch.
func NewSessionTree() *SessionTree {
	now := time.Now().UTC().Format(time.RFC3339)
	mainBranch := &SessionBranch{
		ID:        "main",
		Name:      "main",
		CreatedAt: now,
		State: BranchState{
			Labels: make(map[string]string),
		},
	}
	return &SessionTree{
		ActiveBranch: "main",
		Branches: map[string]*SessionBranch{
			"main": mainBranch,
		},
		Labels: make(map[string]string),
	}
}

// LoadSessionTree loads session tree state from workspace/session_tree.json.
// If the file does not exist, it returns a fresh SessionTree with default "main" branch.
func LoadSessionTree(workspace string) (*SessionTree, error) {
	if workspace == "" {
		return NewSessionTree(), nil
	}
	path := filepath.Join(workspace, sessionTreeFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewSessionTree(), nil
		}
		return nil, fmt.Errorf("load session tree: %w", err)
	}

	var st SessionTree
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("unmarshal session tree: %w", err)
	}

	if st.Branches == nil {
		st.Branches = make(map[string]*SessionBranch)
	}
	if st.Labels == nil {
		st.Labels = make(map[string]string)
	}
	if len(st.Branches) == 0 {
		st.Branches["main"] = &SessionBranch{
			ID:        "main",
			Name:      "main",
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}
	if st.ActiveBranch == "" || st.Branches[st.ActiveBranch] == nil {
		st.ActiveBranch = "main"
		if st.Branches["main"] == nil {
			st.Branches["main"] = &SessionBranch{
				ID:        "main",
				Name:      "main",
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
			}
		}
	}

	return &st, nil
}

// SaveSessionTree atomically writes session tree state to workspace/session_tree.json.
func SaveSessionTree(workspace string, st *SessionTree) error {
	if st == nil {
		return fmt.Errorf("save session tree: tree is nil")
	}
	if workspace == "" {
		return fmt.Errorf("save session tree: empty workspace")
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session tree: %w", err)
	}
	path := filepath.Join(workspace, sessionTreeFile)
	return AtomicWriteFile(path, data, 0o644)
}

// GetBranch retrieves a branch by exact ID, exact Name, or label alias.
func (st *SessionTree) GetBranch(target string) *SessionBranch {
	if b, ok := st.Branches[target]; ok {
		return b
	}
	for _, b := range st.Branches {
		if b.Name == target {
			return b
		}
	}
	if resolvedTarget, ok := st.Labels[target]; ok {
		if b, ok := st.Branches[resolvedTarget]; ok {
			return b
		}
		for _, b := range st.Branches {
			if b.Name == resolvedTarget {
				return b
			}
		}
	}
	return nil
}

// ResolveTarget maps a target string (label, branch name/ID, event ID) to (branchID, eventID).
func (st *SessionTree) ResolveTarget(target string, es *EventStore) (branchID string, eventID string) {
	if labelTarget, ok := st.Labels[target]; ok {
		target = labelTarget
	}

	if b := st.GetBranch(target); b != nil {
		return b.ID, ""
	}

	if es != nil {
		events, _ := es.ReadEvents()
		for _, e := range events {
			if e.ID == target {
				// Events without a BranchID tag belong to the main lineage.
				branch := e.BranchID
				if branch == "" {
					branch = "main"
				}
				return branch, e.ID
			}
		}
	}

	return target, ""
}

// CreateBranch creates a new branch forked from target (branch name/ID, label, or event ID).
func (st *SessionTree) CreateBranch(name string, forkTarget string, es *EventStore) (*SessionBranch, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("branch name cannot be empty")
	}

	for _, b := range st.Branches {
		if b.Name == name || b.ID == name {
			return nil, fmt.Errorf("branch %q already exists", name)
		}
	}

	parentBranchID := st.ActiveBranch
	forkEventID := ""

	if forkTarget != "" {
		resolvedBranchID, resolvedEventID := st.ResolveTarget(forkTarget, es)
		if resolvedEventID == "" && st.GetBranch(resolvedBranchID) == nil {
			return nil, fmt.Errorf("fork target %q not found (not a branch, label, or event ID)", forkTarget)
		}
		if resolvedBranchID != "" {
			parentBranchID = resolvedBranchID
		}
		if resolvedEventID != "" {
			forkEventID = resolvedEventID
		}
	}

	parentBranch := st.GetBranch(parentBranchID)
	if parentBranch == nil {
		parentBranch = st.Branches["main"]
		parentBranchID = "main"
	}

	if forkEventID == "" && es != nil {
		events, _ := es.ReadEvents()
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].BranchID == parentBranchID || (events[i].BranchID == "" && parentBranchID == "main") {
				forkEventID = events[i].ID
				break
			}
		}
	}

	branchID := slugifyBranchName(name)
	if _, ok := st.Branches[branchID]; ok {
		branchID = fmt.Sprintf("%s-%d", branchID, time.Now().Unix())
	}

	var initialState BranchState
	if parentBranch != nil {
		initialState = copyBranchState(parentBranch.State)
	}

	newBranch := &SessionBranch{
		ID:          branchID,
		Name:        name,
		ParentID:    parentBranchID,
		ForkEventID: forkEventID,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		State:       initialState,
	}

	st.Branches[branchID] = newBranch
	return newBranch, nil
}

// CheckoutBranch switches active branch to target (branch name/ID, label, or event ID).
func (st *SessionTree) CheckoutBranch(target string, es *EventStore) (*SessionBranch, error) {
	branchID, eventID := st.ResolveTarget(target, es)
	if branchID == "" && target != "" {
		branchID = target
	}

	b := st.GetBranch(branchID)
	if b == nil {
		if eventID != "" && es != nil {
			events, _ := es.ReadEvents()
			for _, e := range events {
				if e.ID == eventID {
					// Events written before branch tagging (or by the current
					// coordinator, which does not tag BranchID yet) belong to
					// the main lineage.
					eBranch := e.BranchID
					if eBranch == "" {
						eBranch = "main"
					}
					if targetBranch := st.GetBranch(eBranch); targetBranch != nil {
						st.ActiveBranch = targetBranch.ID
						return targetBranch, nil
					}
				}
			}
		}
		return nil, fmt.Errorf("branch or target %q not found", target)
	}

	st.ActiveBranch = b.ID
	return b, nil
}

// AddLabel assigns a human-readable label to an event ID, branch, or checkpoint.
func (st *SessionTree) AddLabel(name string, target string) error {
	name = strings.TrimSpace(name)
	target = strings.TrimSpace(target)
	if name == "" || target == "" {
		return fmt.Errorf("label name and target cannot be empty")
	}
	if st.Labels == nil {
		st.Labels = make(map[string]string)
	}
	st.Labels[name] = target
	return nil
}

// ListBranches returns all branches sorted with "main" first, then by creation date.
func (st *SessionTree) ListBranches() []*SessionBranch {
	var list []*SessionBranch
	for _, b := range st.Branches {
		list = append(list, b)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].ID == "main" {
			return true
		}
		if list[j].ID == "main" {
			return false
		}
		return list[i].CreatedAt < list[j].CreatedAt
	})
	return list
}

// RenderTree generates an ASCII visual tree of all branches, fork points, and labels.
func (st *SessionTree) RenderTree(es *EventStore) string {
	var b strings.Builder
	b.WriteString("Session Tree:\n")

	var events []RunEvent
	if es != nil {
		events, _ = es.ReadEvents()
	}
	eventCounts := make(map[string]int)
	for _, e := range events {
		bid := e.BranchID
		if bid == "" {
			bid = "main"
		}
		eventCounts[bid]++
	}

	branches := st.ListBranches()
	for _, branch := range branches {
		activeMarker := " "
		if branch.ID == st.ActiveBranch {
			activeMarker = "*"
		}
		count := eventCounts[branch.ID]

		parentInfo := ""
		if branch.ParentID != "" {
			if branch.ForkEventID != "" {
				parentInfo = fmt.Sprintf(" (forked from %s@%s)", branch.ParentID, branch.ForkEventID)
			} else {
				parentInfo = fmt.Sprintf(" (forked from %s)", branch.ParentID)
			}
		}

		fmt.Fprintf(&b, "%s %s%s [%d events, created %s]\n", activeMarker, branch.Name, parentInfo, count, branch.CreatedAt)

		// Print labels associated with this branch
		for lbl, target := range st.Labels {
			if target == branch.ID || target == branch.Name {
				fmt.Fprintf(&b, "    ├── label: %s -> %s\n", lbl, target)
			} else if es != nil {
				for _, e := range events {
					if e.ID == target && (e.BranchID == branch.ID || (e.BranchID == "" && branch.ID == "main")) {
						fmt.Fprintf(&b, "    ├── label: %s -> %s (%s)\n", lbl, target, e.Type)
						break
					}
				}
			}
		}
	}

	return b.String()
}

// FilterEventsForBranch returns only events in targetBranch's lineage.
func FilterEventsForBranch(events []RunEvent, st *SessionTree, targetBranchID string) []RunEvent {
	if len(events) == 0 {
		return nil
	}
	if targetBranchID == "" {
		targetBranchID = "main"
	}

	// Build event ID to index map for fast lookup
	eventIndexMap := make(map[string]int, len(events))
	for i, e := range events {
		eventIndexMap[e.ID] = i
	}

	inLineage := make(map[string]bool)
	maxIdxMap := make(map[string]int)

	curBranchID := targetBranchID
	for curBranchID != "" {
		b := st.GetBranch(curBranchID)
		if b == nil {
			break
		}
		inLineage[b.ID] = true

		if b.ParentID != "" && b.ForkEventID != "" {
			if forkIdx, ok := eventIndexMap[b.ForkEventID]; ok {
				// Parent branch is restricted to events up to forkIdx
				if existingMax, ok := maxIdxMap[b.ParentID]; !ok || forkIdx < existingMax {
					maxIdxMap[b.ParentID] = forkIdx
				}
			}
		}

		curBranchID = b.ParentID
	}

	var filtered []RunEvent
	for i, e := range events {
		bid := e.BranchID
		if bid == "" {
			bid = "main"
		}

		if !inLineage[bid] {
			continue
		}

		if maxIdx, restricted := maxIdxMap[bid]; restricted && i > maxIdx {
			continue
		}

		filtered = append(filtered, e)
	}

	return filtered
}

func isVerifySuccess(vr *VerificationResult) bool {
	return vr != nil && vr.ExitCode == 0 && !vr.TimedOut
}

// TaskDiffItem represents a single task status difference between two branches.
type TaskDiffItem struct {
	TaskID   string `json:"task_id"`
	Subject  string `json:"subject"`
	StatusA  string `json:"status_a,omitempty"`
	StatusB  string `json:"status_b,omitempty"`
	DiffType string `json:"diff_type"` // "added_in_b", "removed_in_b", "status_changed", "same"
}

// ArtifactDiffItem represents an artifact file difference between two branches.
type ArtifactDiffItem struct {
	Path     string `json:"path"`
	DiffType string `json:"diff_type"` // "only_in_a", "only_in_b", "modified", "same"
}

// VerificationDiffItem represents verification check differences between two branches.
type VerificationDiffItem struct {
	TaskID    string `json:"task_id"`
	VerifyCmd string `json:"verify_cmd"`
	PassedA   bool   `json:"passed_a"`
	PassedB   bool   `json:"passed_b"`
	DiffType  string `json:"diff_type"` // "status_changed", "only_in_a", "only_in_b"
}

// MemoryDiffItem identifies a canonical worker-memory record that is visible
// in only one branch lineage. Content is deliberately omitted: session diff
// is an observability surface, not a private-memory disclosure path.
type MemoryDiffItem struct {
	ItemID   string `json:"item_id"`
	DiffType string `json:"diff_type"` // "only_in_a" or "only_in_b"
}

// SessionDiff contains full diff analysis between two branches.
type SessionDiff struct {
	BranchA       string                 `json:"branch_a"`
	BranchB       string                 `json:"branch_b"`
	ForkPointID   string                 `json:"fork_point_id,omitempty"`
	TaskDiffs     []TaskDiffItem         `json:"task_diffs"`
	ArtifactDiffs []ArtifactDiffItem     `json:"artifact_diffs"`
	VerifyDiffs   []VerificationDiffItem `json:"verify_diffs"`
	MemoryDiffs   []MemoryDiffItem       `json:"memory_diffs,omitempty"`
	EventCountA   int                    `json:"event_count_a"`
	EventCountB   int                    `json:"event_count_b"`
}

// DiffBranches compares branch A and branch B using event lineage and session projections.
func DiffBranches(workspace string, st *SessionTree, es *EventStore, branchA, branchB string) (*SessionDiff, error) {
	if st == nil {
		return nil, fmt.Errorf("diff branches: session tree is nil")
	}

	bA := st.GetBranch(branchA)
	if bA == nil {
		return nil, fmt.Errorf("branch %q not found", branchA)
	}
	bB := st.GetBranch(branchB)
	if bB == nil {
		return nil, fmt.Errorf("branch %q not found", branchB)
	}

	var events []RunEvent
	if es != nil {
		events, _ = es.ReadEvents()
	}

	eventsA := FilterEventsForBranch(events, st, bA.ID)
	eventsB := FilterEventsForBranch(events, st, bB.ID)

	todoA := ReduceToTodoList(eventsA)
	todoB := ReduceToTodoList(eventsB)

	diff := &SessionDiff{
		BranchA:     bA.Name,
		BranchB:     bB.Name,
		ForkPointID: bB.ForkEventID,
		EventCountA: len(eventsA),
		EventCountB: len(eventsB),
	}

	// Compare Tasks
	mapA := make(map[string]*TodoItem)
	for _, t := range todoA {
		mapA[t.ID] = t
	}
	mapB := make(map[string]*TodoItem)
	for _, t := range todoB {
		mapB[t.ID] = t
	}

	for id, tB := range mapB {
		if tA, ok := mapA[id]; ok {
			if tA.Status != tB.Status {
				diff.TaskDiffs = append(diff.TaskDiffs, TaskDiffItem{
					TaskID:   id,
					Subject:  tB.Desc,
					StatusA:  string(tA.Status),
					StatusB:  string(tB.Status),
					DiffType: "status_changed",
				})
			}
		} else {
			diff.TaskDiffs = append(diff.TaskDiffs, TaskDiffItem{
				TaskID:   id,
				Subject:  tB.Desc,
				StatusB:  string(tB.Status),
				DiffType: "added_in_b",
			})
		}
	}
	for id, tA := range mapA {
		if _, ok := mapB[id]; !ok {
			diff.TaskDiffs = append(diff.TaskDiffs, TaskDiffItem{
				TaskID:   id,
				Subject:  tA.Desc,
				StatusA:  string(tA.Status),
				DiffType: "removed_in_b",
			})
		}
	}

	// Compare Verifications
	for id, tB := range mapB {
		if tB.Verify != "" {
			tA := mapA[id]
			passedA := tA != nil && isVerifySuccess(tA.VerifyResult)
			passedB := isVerifySuccess(tB.VerifyResult)

			if tA == nil {
				diff.VerifyDiffs = append(diff.VerifyDiffs, VerificationDiffItem{
					TaskID:    id,
					VerifyCmd: tB.Verify,
					PassedB:   passedB,
					DiffType:  "only_in_b",
				})
			} else if passedA != passedB {
				diff.VerifyDiffs = append(diff.VerifyDiffs, VerificationDiffItem{
					TaskID:    id,
					VerifyCmd: tB.Verify,
					PassedA:   passedA,
					PassedB:   passedB,
					DiffType:  "status_changed",
				})
			}
		}
	}
	for id, tA := range mapA {
		if tA.Verify != "" {
			if _, ok := mapB[id]; !ok {
				passedA := isVerifySuccess(tA.VerifyResult)
				diff.VerifyDiffs = append(diff.VerifyDiffs, VerificationDiffItem{
					TaskID:    id,
					VerifyCmd: tA.Verify,
					PassedA:   passedA,
					DiffType:  "only_in_a",
				})
			}
		}
	}

	// Compare Artifacts
	artA := extractArtifacts(eventsA, bA.State)
	artB := extractArtifacts(eventsB, bB.State)
	for p := range artB {
		if _, ok := artA[p]; !ok {
			diff.ArtifactDiffs = append(diff.ArtifactDiffs, ArtifactDiffItem{
				Path:     p,
				DiffType: "only_in_b",
			})
		}
	}
	for p := range artA {
		if _, ok := artB[p]; !ok {
			diff.ArtifactDiffs = append(diff.ArtifactDiffs, ArtifactDiffItem{
				Path:     p,
				DiffType: "only_in_a",
			})
		}
	}

	// Compare canonical memory IDs from the branch event lineage. This keeps
	// private content out of a general session diff while making fork/checkout
	// memory visibility auditable.
	memoryA := extractMemoryItemIDs(eventsA)
	memoryB := extractMemoryItemIDs(eventsB)
	for id := range memoryB {
		if !memoryA[id] {
			diff.MemoryDiffs = append(diff.MemoryDiffs, MemoryDiffItem{ItemID: id, DiffType: "only_in_b"})
		}
	}
	for id := range memoryA {
		if !memoryB[id] {
			diff.MemoryDiffs = append(diff.MemoryDiffs, MemoryDiffItem{ItemID: id, DiffType: "only_in_a"})
		}
	}
	sort.Slice(diff.MemoryDiffs, func(i, j int) bool {
		if diff.MemoryDiffs[i].DiffType != diff.MemoryDiffs[j].DiffType {
			return diff.MemoryDiffs[i].DiffType < diff.MemoryDiffs[j].DiffType
		}
		return diff.MemoryDiffs[i].ItemID < diff.MemoryDiffs[j].ItemID
	})

	return diff, nil
}

// RenderText formats session diff results into human readable text.
func (sd *SessionDiff) RenderText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Session Diff: %s <-> %s\n", sd.BranchA, sd.BranchB)
	if sd.ForkPointID != "" {
		fmt.Fprintf(&b, "Fork Point Event: %s\n", sd.ForkPointID)
	}
	fmt.Fprintf(&b, "Events: %s (%d) | %s (%d)\n\n", sd.BranchA, sd.EventCountA, sd.BranchB, sd.EventCountB)

	if len(sd.TaskDiffs) == 0 && len(sd.ArtifactDiffs) == 0 && len(sd.VerifyDiffs) == 0 && len(sd.MemoryDiffs) == 0 {
		b.WriteString("No differences found between branches.\n")
		return b.String()
	}

	if len(sd.TaskDiffs) > 0 {
		b.WriteString("Tasks Diffs:\n")
		for _, td := range sd.TaskDiffs {
			switch td.DiffType {
			case "added_in_b":
				fmt.Fprintf(&b, "  + [%s] %s (status in %s: %s)\n", td.TaskID, td.Subject, sd.BranchB, td.StatusB)
			case "removed_in_b":
				fmt.Fprintf(&b, "  - [%s] %s (was in %s: %s)\n", td.TaskID, td.Subject, sd.BranchA, td.StatusA)
			case "status_changed":
				fmt.Fprintf(&b, "  ~ [%s] %s: %s (%s) -> %s (%s)\n", td.TaskID, td.Subject, sd.BranchA, td.StatusA, sd.BranchB, td.StatusB)
			}
		}
		b.WriteString("\n")
	}

	if len(sd.VerifyDiffs) > 0 {
		b.WriteString("Verification Diffs:\n")
		for _, vd := range sd.VerifyDiffs {
			fmt.Fprintf(&b, "  ~ Task %s (%s): %s pass=%v | %s pass=%v\n",
				vd.TaskID, vd.VerifyCmd, sd.BranchA, vd.PassedA, sd.BranchB, vd.PassedB)
		}
		b.WriteString("\n")
	}

	if len(sd.ArtifactDiffs) > 0 {
		b.WriteString("Artifact Diffs:\n")
		for _, ad := range sd.ArtifactDiffs {
			fmt.Fprintf(&b, "  %s %s\n", ad.DiffType, ad.Path)
		}
		b.WriteString("\n")
	}

	if len(sd.MemoryDiffs) > 0 {
		b.WriteString("Memory Item Diffs:\n")
		for _, md := range sd.MemoryDiffs {
			marker := "+"
			if md.DiffType == "only_in_a" {
				marker = "-"
			}
			fmt.Fprintf(&b, "  %s %s\n", marker, md.ItemID)
		}
		b.WriteString("\n")
	}

	return b.String()
}

func extractArtifacts(events []RunEvent, state BranchState) map[string]bool {
	m := make(map[string]bool)
	for _, a := range state.Artifacts {
		if a != "" {
			m[a] = true
		}
	}
	for _, e := range events {
		if e.Type == "artifact_created" {
			var payload struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err == nil && payload.Path != "" {
				m[payload.Path] = true
			}
		}
	}
	return m
}

func extractMemoryItemIDs(events []RunEvent) map[string]bool {
	ids := make(map[string]bool)
	for _, event := range events {
		switch event.Type {
		case "worker_memory_candidate_saved", "worker_memory_confirmed", "worker_memory_rejected":
			var payload struct {
				ItemID string `json:"item_id"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.ItemID != "" {
				ids[payload.ItemID] = true
			}
		}
	}
	return ids
}

func slugifyBranchName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else if r == ' ' || r == '/' {
			b.WriteRune('-')
		}
	}
	res := strings.Trim(b.String(), "-")
	if res == "" {
		return "branch"
	}
	return res
}

// SnapshotBranchState captures the live session.json task plan (and the latest
// compaction summary) into the given branch's state, so switching away from a
// branch preserves its in-progress work. Missing session data is a no-op.
// ActiveModel/SelectedTeam are owned by the coordinator's runtime populate and
// are not touched here.
func SnapshotBranchState(workspace string, st *SessionTree, branchID string) {
	if st == nil || workspace == "" {
		return
	}
	b := st.Branches[branchID]
	if b == nil {
		return
	}
	sd := LoadSession(workspace)
	if sd != nil && len(sd.Tasks) > 0 {
		plan := make([]*TodoItem, len(sd.Tasks))
		for i, t := range sd.Tasks {
			plan[i] = cloneTodoItem(t)
		}
		b.State.TaskPlan = plan
	}
	if summary := GetLatestCompactionSummary(workspace); summary != nil {
		b.State.Compaction = cloneStructuredSummary(summary)
	}
}

// RebuildSessionForBranch rewrites workspace/session.json from the branch's
// event lineage (tasks + conversation), so the next run resumes the checked-out
// branch's state. When the lineage has no task events it falls back to the
// branch's stored TaskPlan snapshot. A branch with neither events nor snapshot
// leaves session.json untouched.
func RebuildSessionForBranch(workspace string, st *SessionTree, es *EventStore, branchID string) error {
	if st == nil {
		return fmt.Errorf("rebuild session: session tree is nil")
	}
	b := st.GetBranch(branchID)
	if b == nil {
		return fmt.Errorf("rebuild session: branch %q not found", branchID)
	}

	var events []RunEvent
	if es != nil {
		events, _ = es.ReadEvents()
	}
	lineage := FilterEventsForBranch(events, st, b.ID)

	tasks := ReduceToTodoList(lineage)
	if len(tasks) == 0 && len(b.State.TaskPlan) > 0 {
		tasks = make([]*TodoItem, len(b.State.TaskPlan))
		for i, t := range b.State.TaskPlan {
			tasks[i] = cloneTodoItem(t)
		}
	}
	replayed := ReduceToSessionData(lineage)
	entries := replayed.Entries

	// Nothing to rebuild from: leave the live session untouched.
	if len(tasks) == 0 && len(entries) == 0 && len(replayed.CriterionResults) == 0 && len(replayed.CriterionCheckpoints) == 0 && replayed.LastCriterionProgressAt == "" {
		return nil
	}

	sd := LoadSession(workspace)
	if sd == nil {
		sd = NewSession()
	}
	sd.Tasks = tasks
	sd.Entries = entries
	sd.CriterionResults = replayed.CriterionResults
	sd.CriterionCheckpoints = replayed.CriterionCheckpoints
	sd.LastCriterionProgressAt = replayed.LastCriterionProgressAt
	if err := SaveSession(workspace, sd); err != nil {
		return fmt.Errorf("rebuild session for branch %q: %w", b.ID, err)
	}
	return nil
}

func copyBranchState(orig BranchState) BranchState {
	clone := orig
	if len(orig.ActiveTools) > 0 {
		clone.ActiveTools = append([]string(nil), orig.ActiveTools...)
	}
	if len(orig.Artifacts) > 0 {
		clone.Artifacts = append([]string(nil), orig.Artifacts...)
	}
	if orig.Labels != nil {
		clone.Labels = make(map[string]string)
		for k, v := range orig.Labels {
			clone.Labels[k] = v
		}
	}
	if len(orig.TaskPlan) > 0 {
		clone.TaskPlan = make([]*TodoItem, len(orig.TaskPlan))
		for i, item := range orig.TaskPlan {
			clone.TaskPlan[i] = cloneTodoItem(item)
		}
	}
	clone.Compaction = cloneStructuredSummary(orig.Compaction)
	return clone
}
