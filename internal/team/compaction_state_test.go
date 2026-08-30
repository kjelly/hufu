package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

func testCompactionState(t *testing.T, workspace string) *ConversationCompactionState {
	t.Helper()
	summary := StructuredSummary{Goal: "preserve the coordinator goal"}
	history := []fantasy.Message{
		fantasy.NewUserMessage("old context"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "call-1", ToolName: "inspect", Input: `{"path":"x"}`}}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "call-1", Output: fantasy.ToolResultOutputContentText{Text: "observed"}}}},
	}
	replacement := []fantasy.Message{fantasy.NewUserMessage(verifiedHistoryPrefix + "verified\n\n" + summary.RenderMarkdown())}
	generation := CompactionGeneration{ID: "g-1", BranchID: "main", ModelID: "coordinator", CreatedAt: time.Unix(1, 0).UTC(), TokensBefore: 100, TokensAfter: 20, SourceRanges: []CompactionRange{{StartIndex: 0, EndIndex: 2, MsgCount: 3}}, Summary: summary, Replacement: replacement}
	generation.SummaryDigest = digestStructuredSummary(&generation.Summary)
	generation.ReplacementDigest = digestMessages(generation.Replacement)
	generation.Checksum = digestGeneration(generation)
	state := newCompactionState()
	state.Generations[generation.ID] = generation
	checkpoint := ConversationCompactionCheckpoint{
		BranchID: "main", GenerationID: generation.ID,
		AttestationMode: compactionCheckpointAttestationMode,
		History:         history, SourceCounts: []int{1, 1, 1}, SourceRanges: [][]CompactionRange{
			{{StartIndex: 0, EndIndex: 0, MsgCount: 1}},
			{{StartIndex: 1, EndIndex: 1, MsgCount: 1}},
			{{StartIndex: 2, EndIndex: 2, MsgCount: 1}},
		}, NextSourceIndex: 3, HistoryDigest: digestMessages(history),
	}
	checkpoint.EventID = compactionCheckpointEventID(checkpoint)
	state.Branches["main"] = checkpoint
	state.Checkpoints["main"] = []ConversationCompactionCheckpoint{checkpoint}
	if err := SaveConversationCompactionState(workspace, state); err != nil {
		t.Fatal(err)
	}
	return state
}

func testSnapshotCompactionState(t *testing.T, workspace string) *ConversationCompactionState {
	t.Helper()
	return testCompactionState(t, workspace)
}

func attachCompactionEventStore(t *testing.T, workspace, branchID string, c *Coordinator, state *ConversationCompactionState) {
	t.Helper()
	es, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	es.SetBranchID(branchID)
	c.eventStore = es
	c.emittedTaskTransitions = make(map[string]bool)
	t.Cleanup(func() { _ = es.Close() })
	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]bool, len(events))
	for _, event := range events {
		byID[event.ID] = true
	}
	for _, checkpoint := range state.Checkpoints[branchID] {
		generation := state.Generations[checkpoint.GenerationID]
		generationEventID := compactionGenerationEventID(generation.ID)
		if !byID[generationEventID] {
			if _, err := es.AppendPersisted(RunEvent{
				ID: generationEventID, BranchID: branchID, Type: compactionGenerationEventType,
				Actor: "coordinator", Payload: compactionJSON(CompactionReference{GenerationID: generation.ID, BranchID: generation.BranchID, Checksum: generation.Checksum}),
			}); err != nil {
				t.Fatal(err)
			}
			byID[generationEventID] = true
		}
		if checkpointAttestationMode(checkpoint) == compactionCheckpointAttestationMode && !byID[checkpoint.EventID] {
			if _, err := es.AppendPersisted(RunEvent{
				ID: checkpoint.EventID, BranchID: branchID, Type: compactionCheckpointEventType,
				Actor: "coordinator", Payload: compactionJSON(CompactionCheckpointReference{
					BranchID: checkpoint.BranchID, GenerationID: generation.ID, GenerationChecksum: generation.Checksum,
					CheckpointDigest: digestCompactionCheckpoint(checkpoint),
				}),
			}); err != nil {
				t.Fatal(err)
			}
			byID[checkpoint.EventID] = true
		}
	}
}

func materializedCompactionBranchFixture(t *testing.T) (string, string, *SessionTree) {
	t.Helper()
	workspace := t.TempDir()
	state := testCompactionState(t, workspace)
	parentGeneration := state.Generations["g-1"]
	parentEventID := compactionGenerationEventID(parentGeneration.ID)
	parentCheckpoint := state.Branches["main"]
	es, err := NewEventStore(workspace, "run-parent", "session-parent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := es.AppendPersisted(RunEvent{
		ID: parentEventID, BranchID: "main", Type: compactionGenerationEventType,
		Actor: "coordinator", Payload: compactionJSON(CompactionReference{GenerationID: parentGeneration.ID, BranchID: parentGeneration.BranchID, Checksum: parentGeneration.Checksum}),
	}); err != nil {
		t.Fatal(err)
	}
	checkpointEvent, err := compactionCheckpointAttestationEvent(parentCheckpoint, parentGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := es.AppendPersisted(checkpointEvent); err != nil {
		t.Fatal(err)
	}
	tree := NewSessionTree()
	branch, err := tree.CreateBranch("feature", parentCheckpoint.EventID, es)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeCompactionBranch(workspace, "main", branch.ID, branch.ForkEventID); err != nil {
		t.Fatal(err)
	}
	if err := es.Close(); err != nil {
		t.Fatal(err)
	}
	return workspace, branch.ID, tree
}

func TestConversationCompactionStatePersistsLineageAndToolPairs(t *testing.T) {
	workspace := t.TempDir()
	state := testCompactionState(t, workspace)
	first := state.Generations["g-1"]
	second := first
	second.ID = "g-2"
	second.PredecessorID = first.ID
	second.CreatedAt = time.Unix(2, 0).UTC()
	second.SourceRanges = []CompactionRange{{StartIndex: 3, EndIndex: 5, MsgCount: 3}}
	second.Summary.CompletedTasks = []string{"task-1"}
	second.SummaryDigest = digestStructuredSummary(&second.Summary)
	second.ReplacementDigest = digestMessages(second.Replacement)
	second.Checksum = digestGeneration(second)
	state.Generations[second.ID] = second
	checkpoint := ConversationCompactionCheckpoint{
		BranchID: "main", GenerationID: second.ID, History: first.Replacement,
		AttestationMode: compactionCheckpointAttestationMode,
		SourceCounts:    []int{3}, SourceRanges: [][]CompactionRange{{{StartIndex: 3, EndIndex: 5, MsgCount: 3}}}, NextSourceIndex: 6,
		HistoryDigest: digestMessages(first.Replacement),
	}
	checkpoint.EventID = compactionCheckpointEventID(checkpoint)
	state.Branches["main"] = checkpoint
	state.Checkpoints["main"] = []ConversationCompactionCheckpoint{state.Branches["main"]}
	if err := SaveConversationCompactionState(workspace, state); err != nil {
		t.Fatal(err)
	}
	loaded, exists, err := LoadConversationCompactionState(workspace)
	if err != nil || !exists {
		t.Fatalf("load canonical state = (%v, %v)", loaded, err)
	}
	if got := loaded.Generations["g-2"].PredecessorID; got != "g-1" {
		t.Fatalf("predecessor = %q, want g-1", got)
	}
	if !toolPairsIntact(loaded.Branches["main"].History) {
		t.Fatal("loaded history did not preserve tool pairs")
	}
}

func TestConversationCompactionStateRejectsCrossGenerationOverlap(t *testing.T) {
	workspace := t.TempDir()
	state := testCompactionState(t, workspace)
	first := state.Generations["g-1"]
	second := first
	second.ID = "g-2"
	second.PredecessorID = first.ID
	second.CreatedAt = time.Unix(2, 0).UTC()
	second.SourceRanges = []CompactionRange{{StartIndex: 2, EndIndex: 4, MsgCount: 3}}
	second.Summary.CompletedTasks = []string{"task-1"}
	second.SummaryDigest = digestStructuredSummary(&second.Summary)
	second.ReplacementDigest = digestMessages(second.Replacement)
	second.Checksum = digestGeneration(second)
	state.Generations[second.ID] = second
	checkpoint := ConversationCompactionCheckpoint{
		BranchID: "main", GenerationID: second.ID, History: first.Replacement,
		AttestationMode: compactionCheckpointAttestationMode,
		SourceCounts:    []int{3}, SourceRanges: [][]CompactionRange{{{StartIndex: 3, EndIndex: 5, MsgCount: 3}}}, NextSourceIndex: 6,
		HistoryDigest: digestMessages(first.Replacement),
	}
	checkpoint.EventID = compactionCheckpointEventID(checkpoint)
	state.Branches["main"] = checkpoint
	state.Checkpoints["main"] = []ConversationCompactionCheckpoint{state.Branches["main"]}
	if err := SaveConversationCompactionState(workspace, state); err == nil || !strings.Contains(err.Error(), "overlaps predecessor") {
		t.Fatalf("cross-generation overlap result = %v", err)
	}
}

func TestConversationCompactionStateRejectsNonAdjacentAncestorOverlap(t *testing.T) {
	workspace := t.TempDir()
	state := testCompactionState(t, workspace)
	first := state.Generations["g-1"]
	second := first
	second.ID = "g-2"
	second.PredecessorID = first.ID
	second.CreatedAt = time.Unix(2, 0).UTC()
	second.SourceRanges = []CompactionRange{{StartIndex: 3, EndIndex: 5, MsgCount: 3}}
	second.SummaryDigest = digestStructuredSummary(&second.Summary)
	second.ReplacementDigest = digestMessages(second.Replacement)
	second.Checksum = digestGeneration(second)
	state.Generations[second.ID] = second
	third := second
	third.ID = "g-3"
	third.PredecessorID = second.ID
	third.CreatedAt = time.Unix(3, 0).UTC()
	third.SourceRanges = []CompactionRange{{StartIndex: 1, EndIndex: 1, MsgCount: 1}}
	third.SummaryDigest = digestStructuredSummary(&third.Summary)
	third.ReplacementDigest = digestMessages(third.Replacement)
	third.Checksum = digestGeneration(third)
	state.Generations[third.ID] = third
	checkpoint := ConversationCompactionCheckpoint{
		BranchID: "main", GenerationID: third.ID, History: first.Replacement,
		AttestationMode: compactionCheckpointAttestationMode,
		SourceCounts:    []int{3}, SourceRanges: [][]CompactionRange{{{StartIndex: 3, EndIndex: 5, MsgCount: 3}}}, NextSourceIndex: 6,
		HistoryDigest: digestMessages(first.Replacement),
	}
	checkpoint.EventID = compactionCheckpointEventID(checkpoint)
	state.Branches["main"] = checkpoint
	state.Checkpoints["main"] = []ConversationCompactionCheckpoint{state.Branches["main"]}
	if err := SaveConversationCompactionState(workspace, state); err == nil || !strings.Contains(err.Error(), "overlaps predecessor") {
		t.Fatalf("non-adjacent cross-generation overlap result = %v", err)
	}
}

func TestAppendHistorySequentialCompactionsPreserveProvenanceAcrossRestart(t *testing.T) {
	workspace := t.TempDir()
	session := &TeamSession{Workspace: workspace, Dir: workspace, Config: agent.TeamConfig{Name: "test", GoalMode: "exploratory"}}
	c := &Coordinator{session: session}
	for i := 0; i < maxConversationHistory; i++ {
		c.conversationHistory = append(c.conversationHistory, fantasy.NewUserMessage(fmt.Sprintf("history-%03d", i)))
	}
	c.appendHistory(context.Background(), []fantasy.StepResult{{Messages: []fantasy.Message{fantasy.NewUserMessage("first new message")}}})
	if len(c.conversationHistory) != compactHistoryThreshold+1 {
		t.Fatalf("first compaction history length = %d, want %d", len(c.conversationHistory), compactHistoryThreshold+1)
	}
	c.appendHistory(context.Background(), []fantasy.StepResult{{Messages: makeMessages(20, "second")}})
	state, exists, err := LoadConversationCompactionState(workspace)
	if err != nil || !exists {
		t.Fatalf("canonical state after sequential compactions = (%v, %v)", state, err)
	}
	if len(state.Generations) != 2 {
		t.Fatalf("generation count = %d, want 2", len(state.Generations))
	}
	ids := sortedGenerationIDs(state, "main")
	if len(ids) != 2 || state.Generations[ids[0]].SourceRanges[0].StartIndex != 0 || state.Generations[ids[1]].SourceRanges[0].StartIndex <= state.Generations[ids[0]].SourceRanges[0].EndIndex {
		t.Fatalf("sequential generation ranges = %#v", []CompactionGeneration{state.Generations[ids[0]], state.Generations[ids[1]]})
	}
	checkpoint := state.Branches["main"]
	if len(checkpoint.SourceRanges) != len(checkpoint.History) || checkpoint.NextSourceIndex != 121 {
		t.Fatalf("checkpoint provenance = messages %d ranges %d next %d", len(checkpoint.History), len(checkpoint.SourceRanges), checkpoint.NextSourceIndex)
	}
	restarted := &Coordinator{session: session}
	attachCompactionEventStore(t, workspace, "main", restarted, state)
	if err := restarted.restoreCanonicalCompactionForBranch("main"); err != nil {
		t.Fatal(err)
	}
	if digestMessages(restarted.conversationHistory) != checkpoint.HistoryDigest || restarted.conversationHistoryNextSourceIndex != checkpoint.NextSourceIndex {
		t.Fatalf("restart lost canonical history/provenance: history digest %q next %d", digestMessages(restarted.conversationHistory), restarted.conversationHistoryNextSourceIndex)
	}
}

func TestAppendHistoryCompactsSecretBearingHistory(t *testing.T) {
	workspace := t.TempDir()
	session := &TeamSession{Workspace: workspace, Dir: workspace, Config: agent.TeamConfig{Name: "test", GoalMode: "exploratory"}}
	c := &Coordinator{session: session}
	// Credential-shaped history forces the canonical redaction pass to rewrite
	// the persisted payload; the checkpoint identity must be derived after
	// redaction or every save fails identity validation.
	for i := 0; i < maxConversationHistory; i++ {
		c.conversationHistory = append(c.conversationHistory, fantasy.NewUserMessage(fmt.Sprintf("history-%03d api_key: \"sk-live-%03d\"", i, i)))
	}
	c.appendHistory(context.Background(), []fantasy.StepResult{{Messages: []fantasy.Message{fantasy.NewUserMessage("first new message")}}})
	if len(c.conversationHistory) != compactHistoryThreshold+1 {
		t.Fatalf("compacted history length = %d, want %d", len(c.conversationHistory), compactHistoryThreshold+1)
	}
	state, exists, err := LoadConversationCompactionState(workspace)
	if err != nil || !exists {
		t.Fatalf("canonical state after secret-bearing compaction = (%v, %v)", state, err)
	}
	checkpoint := state.Branches["main"]
	if checkpoint.EventID != compactionCheckpointEventID(checkpoint) {
		t.Fatalf("checkpoint identity = %q, want %q", checkpoint.EventID, compactionCheckpointEventID(checkpoint))
	}
	data, err := os.ReadFile(filepath.Join(workspace, conversationCompactionStateFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sk-live-") {
		t.Fatal("canonical compaction state leaked unredacted secrets")
	}
	if !strings.Contains(string(data), "[REDACTED]") {
		t.Fatal("canonical compaction state does not contain redacted content")
	}
}

func TestAppendHistoryCapacityFailureStillTrimsHistory(t *testing.T) {
	workspace := t.TempDir()
	session := &TeamSession{Workspace: workspace, Dir: workspace, Config: agent.TeamConfig{Name: "test", GoalMode: "exploratory"}}
	c := &Coordinator{session: session}
	// A tool result whose mandatory evidence envelope exceeds the output cap
	// makes verified-history compaction fail; the hard history cap must still
	// trim instead of retaining unbounded history.
	call := fantasy.ToolCallPart{ToolCallID: "cap-fail-call", ToolName: "bash", Input: `{"command":"go test ./..."}`}
	result := fantasy.ToolResultPart{ToolCallID: "cap-fail-call", Output: fantasy.ToolResultOutputContentText{Text: strings.Repeat("x", 30_000)}}
	c.conversationHistory = append(c.conversationHistory,
		fantasy.NewUserMessage("original goal"),
		fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{call}},
		fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{result}},
	)
	for i := 0; i < maxConversationHistory; i++ {
		c.conversationHistory = append(c.conversationHistory, fantasy.NewUserMessage(fmt.Sprintf("history-%03d", i)))
	}
	c.appendHistory(context.Background(), []fantasy.StepResult{{Messages: []fantasy.Message{fantasy.NewUserMessage("new message")}}})
	policy := c.compactionPolicy()
	if len(c.conversationHistory) > policy.MaxHistoryMessages {
		t.Fatalf("history length = %d after capacity failure, want <= %d", len(c.conversationHistory), policy.MaxHistoryMessages)
	}
	state, exists, err := LoadConversationCompactionState(workspace)
	if err != nil || !exists {
		t.Fatalf("canonical state after capacity failure = (%v, %v)", state, err)
	}
	if len(state.Branches["main"].History) != len(c.conversationHistory) {
		t.Fatalf("persisted checkpoint history = %d, want %d matching in-memory history", len(state.Branches["main"].History), len(c.conversationHistory))
	}
}

func TestAppendHistoryBoundaryFallbackUsesResolvedHistoryCap(t *testing.T) {
	workspace := t.TempDir()
	policy := agent.DefaultCompactionPolicy()
	policy.MaxHistoryMessages = 40
	policy.RetainHistoryMessages = 30
	session := &TeamSession{Workspace: workspace, Dir: workspace, Config: agent.TeamConfig{Name: "history-cap", GoalMode: "exploratory", Compaction: policy}}
	c := &Coordinator{session: session}
	c.conversationHistory = append(c.conversationHistory, fantasy.NewUserMessage("original goal"))
	// Keep the matching result at the end of the compacted prefix candidate so
	// boundary adjustment must use the fallback path rather than splitting the pair.
	c.conversationHistory = append(c.conversationHistory,
		fantasy.NewUserMessage("history before tool"),
		fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "cap-call", ToolName: "view", Input: `{"path":"src/main.go"}`}}},
	)
	for len(c.conversationHistory) < 40 {
		c.conversationHistory = append(c.conversationHistory, fantasy.NewUserMessage(fmt.Sprintf("history-%02d", len(c.conversationHistory))))
	}
	c.appendHistory(context.Background(), []fantasy.StepResult{{Messages: []fantasy.Message{{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "cap-call", Output: fantasy.ToolResultOutputContentText{Text: "viewed"}}}}}}})

	if len(c.conversationHistory) > policy.MaxHistoryMessages {
		t.Fatalf("history length = %d, want <= %d", len(c.conversationHistory), policy.MaxHistoryMessages)
	}
	if !isToolPairBoundaryClean(c.conversationHistory, len(c.conversationHistory)) {
		t.Fatal("history retained an incomplete tool pair")
	}
	var foundCall, foundResult bool
	for _, msg := range c.conversationHistory {
		if len(extractToolCallIDs(msg)) > 0 {
			foundCall = true
		}
		if len(extractToolResultCallIDs(msg)) > 0 {
			foundResult = true
		}
	}
	if !foundCall || !foundResult {
		t.Fatal("fallback history dropped the tool call/result pair")
	}
}

func makeMessages(count int, prefix string) []fantasy.Message {
	messages := make([]fantasy.Message, count)
	for i := range messages {
		messages[i] = fantasy.NewUserMessage(fmt.Sprintf("%s-%02d", prefix, i))
	}
	return messages
}

func appendLegacyAttestationForTest(t *testing.T, workspace, branchID string, record CompactionRecord, history []fantasy.Message) string {
	t.Helper()
	generation, err := legacyMigrationGeneration(record, branchID, history)
	if err != nil {
		t.Fatal(err)
	}
	es, err := NewEventStore(workspace, "legacy-attestation", "legacy-attestation")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(CompactionReference{GenerationID: generation.ID, BranchID: branchID, Checksum: generation.Checksum})
	if err != nil {
		_ = es.Close()
		t.Fatal(err)
	}
	durable, err := es.AppendPersisted(RunEvent{
		ID: "legacy-attestation-" + generation.ID, BranchID: branchID, Type: compactionGenerationEventType,
		Actor: "coordinator", Payload: payload,
	})
	closeErr := es.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return durable.ID
}

func TestConversationCompactionStateCorruptionIsRecoveryRequired(t *testing.T) {
	workspace := t.TempDir()
	testCompactionState(t, workspace)
	path := filepath.Join(workspace, conversationCompactionStateFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"history_digest": "`, `"history_digest": "bad`, 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := LoadConversationCompactionState(workspace); !exists || err == nil {
		t.Fatalf("corrupt state result = exists %v, err %v", exists, err)
	}
}

func TestEmptyCanonicalCompactionStateIsRecoveryRequired(t *testing.T) {
	workspace := t.TempDir()
	state := newCompactionState()
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, conversationCompactionStateFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := LoadConversationCompactionState(workspace); !exists || err == nil {
		t.Fatalf("empty state result = exists %v, err %v", exists, err)
	}
}

func TestCompactionAttestationMissingIsRepairedIdempotently(t *testing.T) {
	workspace := t.TempDir()
	state := testSnapshotCompactionState(t, workspace)
	es, err := NewEventStore(workspace, "run-test", "session-test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = es.Close() }()
	coordinator := &Coordinator{session: &TeamSession{Workspace: workspace}, eventStore: es, emittedTaskTransitions: make(map[string]bool)}
	if err := coordinator.reconcileCompactionState(nil, "main"); err != nil {
		t.Fatalf("first reconciliation: %v", err)
	}
	if err := coordinator.reconcileCompactionState(nil, "main"); err != nil {
		t.Fatalf("second reconciliation: %v", err)
	}
	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != compactionGenerationEventType || events[1].Type != compactionCheckpointEventType {
		t.Fatalf("repaired attestation events = %+v", events)
	}
	if err := coordinator.restoreCanonicalCompactionForBranch("main"); err != nil {
		t.Fatal(err)
	}
	historyDigest := digestMessages(coordinator.conversationHistory)
	if historyDigest != state.Branches["main"].HistoryDigest {
		t.Fatalf("repaired snapshot history digest = %q, want %q", historyDigest, state.Branches["main"].HistoryDigest)
	}
	if err := es.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := &Coordinator{session: &TeamSession{Workspace: workspace}, emittedTaskTransitions: make(map[string]bool)}
	restartedES, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	restarted.eventStore = restartedES
	if err := restarted.reconcileCompactionState(nil, "main"); err != nil {
		_ = restartedES.Close()
		t.Fatalf("restart reconciliation: %v", err)
	}
	if err := restarted.restoreCanonicalCompactionForBranch("main"); err != nil {
		_ = restartedES.Close()
		t.Fatalf("restart restore: %v", err)
	}
	if got := digestMessages(restarted.conversationHistory); got != historyDigest {
		_ = restartedES.Close()
		t.Fatalf("restart changed repaired snapshot digest = %q, want %q", got, historyDigest)
	}
	if err := restartedES.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCompactionAttestationEffectiveEventBranch(t *testing.T) {
	tests := []struct {
		name        string
		eventBranch string
		generation  string
		want        bool
	}{
		{name: "untagged main event matches main generation", generation: "main", want: true},
		{name: "untagged main event does not match child generation", generation: "feature", want: false},
		{name: "explicit child event matches child generation", eventBranch: "feature", generation: "feature", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			generation := CompactionGeneration{ID: "g-" + test.generation, BranchID: test.generation, Checksum: "checksum-" + test.generation}
			event := RunEvent{
				ID: compactionGenerationEventID(generation.ID), BranchID: test.eventBranch, Type: compactionGenerationEventType,
				Payload: compactionJSON(CompactionReference{GenerationID: generation.ID, BranchID: generation.BranchID, Checksum: generation.Checksum}),
			}
			if got := compactionAttestationMatches(event, generation); got != test.want {
				t.Fatalf("compaction attestation match = %v, want %v", got, test.want)
			}
		})
	}
}

func TestChildCompactionRejectsInheritedUntaggedMainAttestation(t *testing.T) {
	workspace, branchID, tree := materializedCompactionBranchFixture(t)
	state, _, err := LoadConversationCompactionState(workspace)
	if err != nil {
		t.Fatal(err)
	}
	child := state.Branches[branchID]
	generation := state.Generations[child.GenerationID]

	es, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	es.SetBranchID("")
	if _, err := es.AppendPersisted(RunEvent{
		ID: child.EventID, BranchID: "", Type: compactionGenerationEventType, Actor: "coordinator",
		Payload: compactionJSON(CompactionReference{GenerationID: generation.ID, BranchID: generation.BranchID, Checksum: generation.Checksum}),
	}); err != nil {
		_ = es.Close()
		t.Fatal(err)
	}
	// Make the untagged event part of the child's inherited main lineage.
	tree.Branches[branchID].ForkEventID = child.EventID
	if err := SaveSessionTree(workspace, tree); err != nil {
		_ = es.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Close() })

	coordinator := &Coordinator{
		session: &TeamSession{Workspace: workspace}, eventStore: es, compactionBranchID: branchID,
		emittedTaskTransitions: make(map[string]bool),
	}
	sentinel := []fantasy.Message{fantasy.NewUserMessage("preexisting child history")}
	coordinator.conversationHistory = cloneMessages(sentinel)
	eventsBefore, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.reconcileCompactionState(context.Background(), branchID); err == nil || !strings.Contains(err.Error(), "cross-branch") {
		t.Fatalf("inherited untagged child attestation reconciliation = %v", err)
	}
	eventsAfter, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("reconciliation appended repair attestation: before=%d after=%d", len(eventsBefore), len(eventsAfter))
	}
	for _, event := range eventsAfter {
		if event.ID == child.EventID && event.BranchID != "" {
			t.Fatalf("untagged inherited attestation was rewritten as branch %q", event.BranchID)
		}
	}

	if err := coordinator.restoreCanonicalCompactionForBranch(branchID); err == nil || !strings.Contains(err.Error(), "cross-branch") {
		t.Fatalf("inherited untagged child attestation restore = %v", err)
	}
	if !reflect.DeepEqual(coordinator.conversationHistory, sentinel) || coordinator.compactionState != nil || coordinator.lastCompactionSummary != nil {
		t.Fatalf("failed restore published child history/state: history=%#v state=%#v summary=%#v", coordinator.conversationHistory, coordinator.compactionState, coordinator.lastCompactionSummary)
	}
}

func TestCompactionAttestationUnknownGenerationBlocksRecovery(t *testing.T) {
	workspace := t.TempDir()
	testCompactionState(t, workspace)
	es, err := NewEventStore(workspace, "run-test", "session-test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = es.Close() }()
	payload, _ := json.Marshal(CompactionReference{GenerationID: "missing", BranchID: "main", Checksum: "bad"})
	if _, err := es.AppendPersisted(RunEvent{Type: compactionGenerationEventType, BranchID: "main", Actor: "coordinator", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{session: &TeamSession{Workspace: workspace}, eventStore: es}
	if err := coordinator.reconcileCompactionState(nil, "main"); err == nil {
		t.Fatal("unknown generation attestation was accepted")
	}
}

func TestCurrentSnapshotWithEmptyModeAndGenerationEventIsRejected(t *testing.T) {
	workspace := t.TempDir()
	state := testSnapshotCompactionState(t, workspace)
	checkpoint := state.Branches["main"]
	checkpoint.AttestationMode = ""
	checkpoint.EventID = compactionGenerationEventID(checkpoint.GenerationID)
	state.Branches["main"] = checkpoint
	state.Checkpoints["main"] = []ConversationCompactionCheckpoint{checkpoint}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, conversationCompactionStateFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := LoadConversationCompactionState(workspace); !exists || err == nil || !strings.Contains(err.Error(), "attestation mode") {
		t.Fatalf("empty-mode current snapshot result = exists:%v err:%v", exists, err)
	}
}

func TestCurrentSnapshotDowngradeToLegacyGenerationFailsClosed(t *testing.T) {
	workspace := t.TempDir()
	state := testSnapshotCompactionState(t, workspace)
	checkpoint := state.Branches["main"]
	checkpoint.AttestationMode = compactionLegacyAttestationMode
	checkpoint.EventID = compactionGenerationEventID(checkpoint.GenerationID)
	checkpoint.History = []fantasy.Message{fantasy.NewUserMessage("substituted history")}
	checkpoint.SourceOffset = 0
	checkpoint.SourceCounts = []int{1}
	checkpoint.SourceRanges = [][]CompactionRange{{{StartIndex: 0, EndIndex: 0, MsgCount: 1}}}
	checkpoint.NextSourceIndex = 1
	checkpoint.HistoryDigest = digestMessages(checkpoint.History)
	state.Branches["main"] = checkpoint
	state.Checkpoints["main"] = []ConversationCompactionCheckpoint{checkpoint}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, conversationCompactionStateFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	es, err := NewEventStore(workspace, "run-downgrade", "session-downgrade")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = es.Close() }()
	generation := state.Generations[checkpoint.GenerationID]
	generationEvent, err := compactionGenerationAttestationEvent(generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := es.AppendPersisted(generationEvent); err != nil {
		t.Fatal(err)
	}
	eventsBefore := mustReadEvents(t, es)
	if _, exists, err := LoadConversationCompactionState(workspace); !exists || err == nil || !strings.Contains(err.Error(), "attestation mode") {
		t.Fatalf("legacy downgrade load = exists:%v err:%v", exists, err)
	}
	coordinator := &Coordinator{session: &TeamSession{Workspace: workspace}, eventStore: es, emittedTaskTransitions: make(map[string]bool)}
	if err := coordinator.reconcileCompactionState(context.Background(), "main"); err == nil || !strings.Contains(err.Error(), "attestation mode") {
		t.Fatalf("legacy downgrade reconciliation = %v", err)
	}
	if err := coordinator.restoreCanonicalCompactionForBranch("main"); err == nil || !strings.Contains(err.Error(), "attestation mode") {
		t.Fatalf("legacy downgrade restore = %v", err)
	}
	eventsAfter := mustReadEvents(t, es)
	if len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("legacy downgrade appended repair event: before=%d after=%d", len(eventsBefore), len(eventsAfter))
	}
}

func TestSnapshotCheckpointReferenceMismatchIsRejected(t *testing.T) {
	workspace := t.TempDir()
	state := testSnapshotCompactionState(t, workspace)
	checkpoint := state.Branches["main"]
	generation := state.Generations[checkpoint.GenerationID]
	es, err := NewEventStore(workspace, "run-mismatch", "session-mismatch")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = es.Close() }()
	if _, err := es.AppendPersisted(RunEvent{
		ID: checkpoint.EventID, BranchID: checkpoint.BranchID, Type: compactionCheckpointEventType, Actor: "coordinator",
		Payload: compactionJSON(CompactionCheckpointReference{
			BranchID: checkpoint.BranchID, GenerationID: generation.ID, GenerationChecksum: generation.Checksum,
			CheckpointDigest: "tampered-checkpoint-digest",
		}),
	}); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{session: &TeamSession{Workspace: workspace}, eventStore: es}
	if err := coordinator.reconcileCompactionState(context.Background(), "main"); err == nil || !strings.Contains(err.Error(), "canonical checkpoint") {
		t.Fatalf("mismatched snapshot reference reconciliation = %v", err)
	}
}

func TestLegacyCompactionMigratesToCanonicalState(t *testing.T) {
	workspace := t.TempDir()
	history := []fantasy.Message{fantasy.NewUserMessage(verifiedHistoryPrefix + "legacy replacement"), fantasy.NewUserMessage("retained tail")}
	if err := SaveConversationHistory(workspace, history); err != nil {
		t.Fatal(err)
	}
	legacy := CompactionRecord{ID: "legacy-1", Timestamp: time.Unix(3, 0).UTC(), SourceRange: CompactionRange{StartIndex: 0, EndIndex: 0, MsgCount: 1}, Summary: StructuredSummary{Goal: "legacy goal"}}
	if err := SaveCompactionRecord(workspace, legacy); err != nil {
		t.Fatal(err)
	}
	attestationID := appendLegacyAttestationForTest(t, workspace, "main", legacy, history)
	if err := MigrateLegacyCompactionState(workspace, "main"); err != nil {
		t.Fatal(err)
	}
	state, exists, err := LoadConversationCompactionState(workspace)
	if err != nil || !exists {
		t.Fatalf("migrated state = (%v, %v)", state, err)
	}
	if state.Branches["main"].GenerationID == "" || state.Generations[state.Branches["main"].GenerationID].Summary.Goal != "legacy goal" {
		t.Fatalf("legacy summary was not migrated: %+v", state)
	}
	checkpoint := state.Branches["main"]
	if checkpoint.AttestationMode != compactionCheckpointAttestationMode {
		t.Fatalf("migrated checkpoint mode = %q, want snapshot mode", checkpoint.AttestationMode)
	}
	if checkpoint.EventID != compactionCheckpointEventID(checkpoint) || len(checkpoint.SourceRanges) != len(history) || checkpoint.NextSourceIndex != len(history) {
		t.Fatalf("legacy checkpoint is incomplete: %+v", checkpoint)
	}
	if got := state.Checkpoints["main"]; len(got) != 1 || got[0].EventID != checkpoint.EventID {
		t.Fatalf("legacy immutable checkpoints = %+v", got)
	}
	es, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	events, err := es.ReadEvents()
	closeErr := es.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	var checkpointEvent *RunEvent
	for i := range events {
		if events[i].ID == checkpoint.EventID {
			checkpointEvent = &events[i]
			break
		}
	}
	if checkpointEvent == nil || !compactionCheckpointAttestationMatches(*checkpointEvent, checkpoint, state.Generations[checkpoint.GenerationID]) {
		t.Fatalf("migrated checkpoint attestation = %#v, want exact snapshot attestation (legacy input %q)", checkpointEvent, attestationID)
	}
}

func TestLegacyMigrationMultipleRecordsUsesLatest(t *testing.T) {
	workspace := t.TempDir()
	history := []fantasy.Message{fantasy.NewUserMessage(verifiedHistoryPrefix + "legacy replacement"), fantasy.NewUserMessage("retained tail")}
	if err := SaveConversationHistory(workspace, history); err != nil {
		t.Fatal(err)
	}
	// Pre-canonical workspaces compact more than once: only the latest record
	// maps onto the current history, so migration must use it instead of
	// refusing the workspace outright.
	older := CompactionRecord{ID: "legacy-old", Timestamp: time.Unix(1, 0).UTC(), SourceRange: CompactionRange{StartIndex: 0, EndIndex: 0, MsgCount: 1}, Summary: StructuredSummary{Goal: "older goal"}}
	latest := CompactionRecord{ID: "legacy-latest", Timestamp: time.Unix(3, 0).UTC(), SourceRange: CompactionRange{StartIndex: 0, EndIndex: 0, MsgCount: 1}, Summary: StructuredSummary{Goal: "latest goal"}}
	if err := saveLegacyCompactionHistory(workspace, []CompactionRecord{older, latest}); err != nil {
		t.Fatal(err)
	}
	appendLegacyAttestationForTest(t, workspace, "main", latest, history)
	if err := MigrateLegacyCompactionState(workspace, "main"); err != nil {
		t.Fatal(err)
	}
	state, exists, err := LoadConversationCompactionState(workspace)
	if err != nil || !exists {
		t.Fatalf("migrated multi-record state = (%v, %v)", state, err)
	}
	generation := state.Generations[state.Branches["main"].GenerationID]
	if generation.Summary.Goal != "latest goal" {
		t.Fatalf("migrated summary goal = %q, want the latest record", generation.Summary.Goal)
	}
}

func TestLegacyMigrationMultipleRecordsWithoutAttestationStaysCompatible(t *testing.T) {
	workspace := t.TempDir()
	history := []fantasy.Message{fantasy.NewUserMessage(verifiedHistoryPrefix + "legacy replacement"), fantasy.NewUserMessage("retained tail")}
	if err := SaveConversationHistory(workspace, history); err != nil {
		t.Fatal(err)
	}
	older := CompactionRecord{ID: "legacy-old-unattested", Timestamp: time.Unix(1, 0).UTC(), SourceRange: CompactionRange{StartIndex: 0, EndIndex: 0, MsgCount: 1}, Summary: StructuredSummary{Goal: "older goal"}}
	latest := CompactionRecord{ID: "legacy-latest-unattested", Timestamp: time.Unix(3, 0).UTC(), SourceRange: CompactionRange{StartIndex: 0, EndIndex: 0, MsgCount: 1}, Summary: StructuredSummary{Goal: "latest goal"}}
	if err := saveLegacyCompactionHistory(workspace, []CompactionRecord{older, latest}); err != nil {
		t.Fatal(err)
	}
	if err := MigrateLegacyCompactionState(workspace, "main"); err != nil {
		t.Fatal(err)
	}
	if state, exists, err := LoadConversationCompactionState(workspace); err != nil || exists || state != nil {
		t.Fatalf("unattested multi-record migration created canonical state: state=%#v exists=%v err=%v", state, exists, err)
	}
}

func TestLegacyMigrationWithoutAttestationDoesNotCreateCanonicalState(t *testing.T) {
	workspace := t.TempDir()
	history := []fantasy.Message{fantasy.NewUserMessage(verifiedHistoryPrefix + "legacy replacement"), fantasy.NewUserMessage("retained tail")}
	if err := SaveConversationHistory(workspace, history); err != nil {
		t.Fatal(err)
	}
	if err := SaveCompactionRecord(workspace, CompactionRecord{
		ID: "legacy-unattested", SourceRange: CompactionRange{StartIndex: 0, EndIndex: 2, MsgCount: 3}, Summary: StructuredSummary{Goal: "legacy goal"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := MigrateLegacyCompactionState(workspace, "main"); err != nil {
		t.Fatal(err)
	}
	if state, exists, err := LoadConversationCompactionState(workspace); err != nil || exists || state != nil {
		t.Fatalf("unattested migration created canonical state: state=%#v exists=%v err=%v", state, exists, err)
	}
}

func TestLegacyMigrationRejectsUntaggedChildAttestation(t *testing.T) {
	workspace := t.TempDir()
	history := []fantasy.Message{fantasy.NewUserMessage(verifiedHistoryPrefix + "legacy replacement"), fantasy.NewUserMessage("retained tail")}
	if err := SaveConversationHistory(workspace, history); err != nil {
		t.Fatal(err)
	}
	legacy := CompactionRecord{
		ID: "legacy-child", BranchID: "feature", Timestamp: time.Unix(6, 0).UTC(),
		SourceRange: CompactionRange{StartIndex: 0, EndIndex: 0, MsgCount: 1}, Summary: StructuredSummary{Goal: "legacy child goal"},
	}
	if err := SaveCompactionRecord(workspace, legacy); err != nil {
		t.Fatal(err)
	}
	generation, err := legacyMigrationGeneration(legacy, "feature", history)
	if err != nil {
		t.Fatal(err)
	}
	es, err := NewEventStore(workspace, "legacy-child-attestation", "legacy-child-attestation")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := es.AppendPersisted(RunEvent{
		ID: "legacy-attestation-child", BranchID: "", Type: compactionGenerationEventType, Actor: "coordinator",
		Payload: compactionJSON(CompactionReference{GenerationID: generation.ID, BranchID: generation.BranchID, Checksum: generation.Checksum}),
	}); err != nil {
		_ = es.Close()
		t.Fatal(err)
	}
	if err := es.Close(); err != nil {
		t.Fatal(err)
	}

	if err := MigrateLegacyCompactionState(workspace, "feature"); err == nil || !strings.Contains(err.Error(), "cross-branch") {
		t.Fatalf("untagged child legacy migration = %v", err)
	}
	if state, exists, err := LoadConversationCompactionState(workspace); err != nil || exists || state != nil {
		t.Fatalf("untagged child migration promoted canonical state: state=%#v exists=%v err=%v", state, exists, err)
	}
}

func TestLegacyMigrationPreservesCompactedReplacementProvenance(t *testing.T) {
	workspace := t.TempDir()
	history := []fantasy.Message{
		fantasy.NewUserMessage(verifiedHistoryPrefix + "legacy replacement"),
		fantasy.NewUserMessage("retained tail"),
	}
	if err := SaveConversationHistory(workspace, history); err != nil {
		t.Fatal(err)
	}
	legacy := CompactionRecord{
		ID: "legacy-range", SourceRange: CompactionRange{StartIndex: 7, EndIndex: 9, MsgCount: 3},
		Summary: StructuredSummary{Goal: "legacy range goal"},
	}
	if err := SaveCompactionRecord(workspace, legacy); err != nil {
		t.Fatal(err)
	}
	appendLegacyAttestationForTest(t, workspace, "main", legacy, history)
	if err := MigrateLegacyCompactionState(workspace, "main"); err != nil {
		t.Fatal(err)
	}
	state, exists, err := LoadConversationCompactionState(workspace)
	if err != nil || !exists {
		t.Fatalf("migrated range state = (%v, %v)", state, err)
	}
	checkpoint := state.Branches["main"]
	if checkpoint.SourceCounts[0] != 3 || checkpoint.SourceRanges[0][0] != (CompactionRange{StartIndex: 7, EndIndex: 9, MsgCount: 3}) || checkpoint.SourceRanges[1][0] != (CompactionRange{StartIndex: 10, EndIndex: 10, MsgCount: 1}) || checkpoint.NextSourceIndex != 11 {
		t.Fatalf("migrated compacted provenance = counts:%v ranges:%v next:%d", checkpoint.SourceCounts, checkpoint.SourceRanges, checkpoint.NextSourceIndex)
	}
}

func TestLegacyMigrationRestartAppendPreservesSequentialProvenance(t *testing.T) {
	workspace := t.TempDir()
	history := make([]fantasy.Message, 85)
	history[0] = fantasy.NewUserMessage(verifiedHistoryPrefix + "legacy replacement")
	for i := 1; i < len(history); i++ {
		history[i] = fantasy.NewUserMessage(fmt.Sprintf("legacy-tail-%02d", i))
	}
	if err := SaveConversationHistory(workspace, history); err != nil {
		t.Fatal(err)
	}
	if err := SaveCompactionRecord(workspace, CompactionRecord{
		ID: "legacy-sequential", Timestamp: time.Unix(4, 0).UTC(),
		SourceRange: CompactionRange{StartIndex: 0, EndIndex: 0, MsgCount: 1}, Summary: StructuredSummary{Goal: "legacy sequential goal"},
	}); err != nil {
		t.Fatal(err)
	}
	legacy := CompactionRecord{ID: "legacy-sequential", Timestamp: time.Unix(4, 0).UTC(), SourceRange: CompactionRange{StartIndex: 0, EndIndex: 0, MsgCount: 1}, Summary: StructuredSummary{Goal: "legacy sequential goal"}}
	appendLegacyAttestationForTest(t, workspace, "main", legacy, history)
	if err := MigrateLegacyCompactionState(workspace, "main"); err != nil {
		t.Fatal(err)
	}

	session := &TeamSession{Workspace: workspace, Dir: workspace, Config: agent.TeamConfig{Name: "test", GoalMode: "exploratory"}}
	restarted := &Coordinator{session: session}
	state, _, err := LoadConversationCompactionState(workspace)
	if err != nil {
		t.Fatal(err)
	}
	attachCompactionEventStore(t, workspace, "main", restarted, state)
	if err := restarted.restoreCanonicalCompactionForBranch("main"); err != nil {
		t.Fatal(err)
	}
	initial := append([]fantasy.Message(nil), restarted.conversationHistory...)
	restarted.appendHistory(context.Background(), []fantasy.StepResult{{Messages: makeMessages(18, "post-restart")}})
	state, exists, err := LoadConversationCompactionState(workspace)
	if err != nil || !exists {
		t.Fatalf("canonical state after migration append = (%v, %v)", state, err)
	}
	if len(state.Generations) != 2 {
		t.Fatalf("generation count after migration append = %d, want 2", len(state.Generations))
	}
	checkpoint := state.Branches["main"]
	if len(initial) != 85 || len(checkpoint.History) != len(restarted.conversationHistory) || len(checkpoint.SourceRanges) != len(checkpoint.History) {
		t.Fatalf("post-restart checkpoint lost history/provenance: initial=%d checkpoint=%d ranges=%d", len(initial), len(checkpoint.History), len(checkpoint.SourceRanges))
	}
	if checkpoint.NextSourceIndex != 103 {
		t.Fatalf("post-restart next source index = %d, want 103", checkpoint.NextSourceIndex)
	}
	ids := sortedGenerationIDs(state, "main")
	if len(ids) != 2 || state.Generations[ids[1]].SourceRanges[0].StartIndex <= state.Generations[ids[0]].SourceRanges[0].EndIndex {
		t.Fatalf("migrated sequential generation ranges = %#v", []CompactionGeneration{state.Generations[ids[0]], state.Generations[ids[1]]})
	}

	finalRestart := &Coordinator{session: session}
	attachCompactionEventStore(t, workspace, "main", finalRestart, state)
	if err := finalRestart.restoreCanonicalCompactionForBranch("main"); err != nil {
		t.Fatal(err)
	}
	if digestMessages(finalRestart.conversationHistory) != checkpoint.HistoryDigest || finalRestart.conversationHistoryNextSourceIndex != checkpoint.NextSourceIndex {
		t.Fatalf("restart after sequential append lost canonical state: history digest=%q next=%d", digestMessages(finalRestart.conversationHistory), finalRestart.conversationHistoryNextSourceIndex)
	}
}

func TestLegacyMigrationAttestationExactForkPreservesTail(t *testing.T) {
	workspace := t.TempDir()
	history := []fantasy.Message{
		fantasy.NewUserMessage(verifiedHistoryPrefix + "legacy replacement"),
		fantasy.NewUserMessage("retained tail"),
	}
	if err := SaveConversationHistory(workspace, history); err != nil {
		t.Fatal(err)
	}
	legacy := CompactionRecord{
		ID: "legacy-fork", Timestamp: time.Unix(5, 0).UTC(),
		SourceRange: CompactionRange{StartIndex: 0, EndIndex: 0, MsgCount: 1}, Summary: StructuredSummary{Goal: "legacy fork goal"},
	}
	if err := SaveCompactionRecord(workspace, legacy); err != nil {
		t.Fatal(err)
	}
	appendLegacyAttestationForTest(t, workspace, "main", legacy, history)
	if err := MigrateLegacyCompactionState(workspace, "main"); err != nil {
		t.Fatal(err)
	}
	state, _, err := LoadConversationCompactionState(workspace)
	if err != nil {
		t.Fatal(err)
	}
	parent := state.Branches["main"]
	if err := MaterializeCompactionBranch(workspace, "main", "feature", parent.EventID); err != nil {
		t.Fatal(err)
	}
	materialized, _, err := LoadConversationCompactionState(workspace)
	if err != nil {
		t.Fatal(err)
	}
	child := materialized.Branches["feature"]
	if child.AttestationMode != compactionCheckpointAttestationMode || child.EventID != compactionCheckpointEventID(child) || child.EventID == parent.EventID || child.HistoryDigest != parent.HistoryDigest || len(child.History) != len(history) || messageText(child.History[1]) != "retained tail" {
		t.Fatalf("migrated exact fork checkpoint = %+v, want complete parent history", child)
	}
	if len(child.SourceRanges) != len(history) || child.NextSourceIndex != parent.NextSourceIndex {
		t.Fatalf("migrated exact fork provenance = ranges:%d next:%d", len(child.SourceRanges), child.NextSourceIndex)
	}
}

func TestLegacyMigrationFailsClosedForProvenanceMismatch(t *testing.T) {
	workspace := t.TempDir()
	history := []fantasy.Message{fantasy.NewUserMessage(verifiedHistoryPrefix + "replacement"), fantasy.NewUserMessage("retained tail")}
	if err := SaveConversationHistory(workspace, history); err != nil {
		t.Fatal(err)
	}
	// Session provenance says the replacement covers five source messages, but
	// the latest legacy record claims one: migrating would attach wrong
	// provenance, so the migration must fail closed instead of guessing.
	if err := SaveSession(workspace, &SessionData{ConversationHistorySourceOffset: 0, ConversationHistorySourceCounts: []int{5, 1}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := SaveCompactionRecord(workspace, CompactionRecord{ID: fmt.Sprintf("legacy-%d", i), SourceRange: CompactionRange{StartIndex: i, EndIndex: i, MsgCount: 1}, Summary: StructuredSummary{Goal: "ambiguous"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := MigrateLegacyCompactionState(workspace, "main"); err == nil || !strings.Contains(err.Error(), "does not match compacted range count") {
		t.Fatalf("provenance-mismatched migration result = %v", err)
	}
	if _, exists, err := LoadConversationCompactionState(workspace); err != nil || exists {
		t.Fatalf("failed migration created canonical state: exists=%v err=%v", exists, err)
	}
}

func TestConversationCompactionStateRejectsScalarOnlyCheckpoint(t *testing.T) {
	workspace := t.TempDir()
	state := testCompactionState(t, workspace)
	checkpoint := state.Branches["main"]
	checkpoint.SourceRanges = nil
	checkpoint.EventID = ""
	checkpoint.EventID = compactionCheckpointEventID(checkpoint)
	state.Branches["main"] = checkpoint
	state.Checkpoints["main"] = []ConversationCompactionCheckpoint{checkpoint}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, conversationCompactionStateFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := LoadConversationCompactionState(workspace); !exists || err == nil || !strings.Contains(err.Error(), "source ranges do not match history") {
		t.Fatalf("scalar-only canonical checkpoint result = exists:%v err:%v", exists, err)
	}
}

func TestMaterializeCompactionBranchDoesNotShareHistory(t *testing.T) {
	workspace := t.TempDir()
	testCompactionState(t, workspace)
	if err := MaterializeCompactionBranch(workspace, "main", "feature"); err != nil {
		t.Fatal(err)
	}
	state, _, err := LoadConversationCompactionState(workspace)
	if err != nil {
		t.Fatal(err)
	}
	parent := state.Branches["main"]
	child := state.Branches["feature"]
	if parent.GenerationID == child.GenerationID {
		t.Fatal("fork reused the parent's generation identity")
	}
	if parent.HistoryDigest != child.HistoryDigest {
		t.Fatalf("fork changed history: parent %q child %q", parent.HistoryDigest, child.HistoryDigest)
	}
	child.History[0] = fantasy.NewUserMessage("feature only")
	if messageText(parent.History[0]) == "feature only" {
		t.Fatal("forked history aliases parent")
	}
}

func TestMaterializeCompactionBranchUsesForkEventState(t *testing.T) {
	workspace := t.TempDir()
	state := testCompactionState(t, workspace)
	first := state.Generations["g-1"]
	second := first
	second.ID = "g-2"
	second.PredecessorID = first.ID
	second.CreatedAt = time.Unix(2, 0).UTC()
	second.SourceRanges = []CompactionRange{{StartIndex: 3, EndIndex: 5, MsgCount: 3}}
	second.Summary.Goal = "future summary"
	second.SummaryDigest = digestStructuredSummary(&second.Summary)
	second.Replacement = []fantasy.Message{fantasy.NewUserMessage(verifiedHistoryPrefix + "future replacement")}
	second.ReplacementDigest = digestMessages(second.Replacement)
	second.Checksum = digestGeneration(second)
	state.Generations[second.ID] = second
	firstCheckpoint := ConversationCompactionCheckpoint{
		BranchID: "main", GenerationID: first.ID, AttestationMode: compactionCheckpointAttestationMode,
		History:      append(cloneMessages(first.Replacement), fantasy.NewUserMessage("retained tail")),
		SourceCounts: []int{3, 1}, SourceRanges: [][]CompactionRange{
			{{StartIndex: 0, EndIndex: 2, MsgCount: 3}},
			{{StartIndex: 3, EndIndex: 3, MsgCount: 1}},
		}, NextSourceIndex: 4,
	}
	firstCheckpoint.HistoryDigest = digestMessages(firstCheckpoint.History)
	firstCheckpoint.EventID = compactionCheckpointEventID(firstCheckpoint)
	secondCheckpoint := ConversationCompactionCheckpoint{
		BranchID: "main", GenerationID: second.ID, AttestationMode: compactionCheckpointAttestationMode,
		History:      append(cloneMessages(second.Replacement), fantasy.NewUserMessage("future conversation")),
		SourceCounts: []int{6, 1}, SourceRanges: [][]CompactionRange{
			{{StartIndex: 0, EndIndex: 5, MsgCount: 6}},
			{{StartIndex: 6, EndIndex: 6, MsgCount: 1}},
		}, NextSourceIndex: 7,
	}
	secondCheckpoint.HistoryDigest = digestMessages(secondCheckpoint.History)
	secondCheckpoint.EventID = compactionCheckpointEventID(secondCheckpoint)
	state.Branches["main"] = secondCheckpoint
	state.Checkpoints["main"] = []ConversationCompactionCheckpoint{firstCheckpoint, secondCheckpoint}
	if err := SaveConversationCompactionState(workspace, state); err != nil {
		t.Fatal(err)
	}

	es, err := NewEventStore(workspace, "run-fork", "session-fork")
	if err != nil {
		t.Fatal(err)
	}
	for _, checkpoint := range []ConversationCompactionCheckpoint{firstCheckpoint, secondCheckpoint} {
		generation := state.Generations[checkpoint.GenerationID]
		generationEvent, err := compactionGenerationAttestationEvent(generation)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := es.AppendPersisted(generationEvent); err != nil {
			t.Fatal(err)
		}
		checkpointEvent, err := compactionCheckpointAttestationEvent(checkpoint, generation)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := es.AppendPersisted(checkpointEvent); err != nil {
			t.Fatal(err)
		}
	}
	if err := es.Close(); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeCompactionBranch(workspace, "main", "feature", firstCheckpoint.EventID); err != nil {
		t.Fatal(err)
	}
	materialized, _, err := LoadConversationCompactionState(workspace)
	if err != nil {
		t.Fatal(err)
	}
	child := materialized.Branches["feature"]
	if child.GenerationID == "" {
		t.Fatal("fork did not receive a canonical checkpoint")
	}
	childGeneration := materialized.Generations[child.GenerationID]
	if childGeneration.Summary.Goal != first.Summary.Goal {
		t.Fatalf("fork inherited future generation summary %q", childGeneration.Summary.Goal)
	}
	if len(child.History) != len(firstCheckpoint.History) || messageText(child.History[0]) != messageText(first.Replacement[0]) || messageText(child.History[1]) != "retained tail" {
		t.Fatalf("fork history = %#v, want full first checkpoint %#v", child.History, firstCheckpoint.History)
	}
	if strings.Contains(messageText(child.History[len(child.History)-1]), "future conversation") {
		t.Fatal("fork inherited conversation after the requested event")
	}
}

func TestMaterializeCompactionBranchUsesLaterFullCheckpoint(t *testing.T) {
	workspace := t.TempDir()
	state := testCompactionState(t, workspace)
	generation := state.Generations["g-1"]
	base := state.Branches["main"]
	base.EventID = ""
	base.SourceRanges = [][]CompactionRange{{{StartIndex: 0, EndIndex: 0, MsgCount: 1}}, {{StartIndex: 1, EndIndex: 1, MsgCount: 1}}, {{StartIndex: 2, EndIndex: 2, MsgCount: 1}}}
	base.NextSourceIndex = 3
	base.HistoryDigest = digestMessages(base.History)
	base.EventID = compactionCheckpointEventID(base)
	later := base
	later.EventID = ""
	later.History = append(cloneMessages(base.History), fantasy.NewUserMessage("later history"))
	later.SourceRanges = append(cloneSourceRanges(base.SourceRanges), []CompactionRange{{StartIndex: 3, EndIndex: 3, MsgCount: 1}})
	later.SourceCounts = sourceCountsForRanges(later.SourceRanges)
	later.NextSourceIndex = 4
	later.HistoryDigest = digestMessages(later.History)
	later.EventID = compactionCheckpointEventID(later)
	state.Branches["main"] = later
	state.Checkpoints["main"] = []ConversationCompactionCheckpoint{base, later}
	if err := SaveConversationCompactionState(workspace, state); err != nil {
		t.Fatal(err)
	}
	es, err := NewEventStore(workspace, "run-fork-later", "session-fork-later")
	if err != nil {
		t.Fatal(err)
	}
	baseGenerationEvent, err := compactionGenerationAttestationEvent(generation)
	if err != nil {
		t.Fatal(err)
	}
	baseCheckpointEvent, err := compactionCheckpointAttestationEvent(base, generation)
	if err != nil {
		t.Fatal(err)
	}
	laterCheckpointEvent, err := compactionCheckpointAttestationEvent(later, generation)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []RunEvent{
		baseGenerationEvent,
		baseCheckpointEvent,
		{ID: "later-event", BranchID: "main", Type: modelContinuationEventType, Actor: "coordinator", Payload: compactionJSON(map[string]string{"status": "ok"})},
		laterCheckpointEvent,
	} {
		if _, err := es.AppendPersisted(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := es.Close(); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeCompactionBranch(workspace, "main", "feature", later.EventID); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := LoadConversationCompactionState(workspace)
	if err != nil {
		t.Fatal(err)
	}
	child := loaded.Branches["feature"]
	if child.AttestationMode != compactionCheckpointAttestationMode || child.EventID != compactionCheckpointEventID(child) || digestMessages(child.History) != later.HistoryDigest || messageText(child.History[len(child.History)-1]) != "later history" {
		t.Fatalf("later fork checkpoint = %#v, want full later history %#v", child, later.History)
	}
}

func TestMaterializedChildCheckpointAttestationBindsChildIdentityAcrossRestart(t *testing.T) {
	workspace, branchID, _ := materializedCompactionBranchFixture(t)
	state, exists, err := LoadConversationCompactionState(workspace)
	if err != nil || !exists {
		t.Fatalf("load materialized state = (%v, %v)", state, err)
	}
	checkpoint := state.Branches[branchID]
	if checkpoint.AttestationMode != compactionCheckpointAttestationMode || checkpoint.EventID != compactionCheckpointEventID(checkpoint) {
		t.Fatalf("child checkpoint attestation = mode:%q event:%q, want snapshot %q", checkpoint.AttestationMode, checkpoint.EventID, compactionCheckpointEventID(checkpoint))
	}
	generation := state.Generations[checkpoint.GenerationID]
	coordinator := &Coordinator{session: &TeamSession{Workspace: workspace}, compactionBranchID: branchID}
	es, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	es.SetBranchID(branchID)
	coordinator.eventStore = es
	coordinator.emittedTaskTransitions = make(map[string]bool)
	if err := coordinator.reconcileCompactionState(context.Background(), branchID); err != nil {
		t.Fatal(err)
	}
	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	var childEvent *RunEvent
	for i := range events {
		if events[i].ID == checkpoint.EventID {
			childEvent = &events[i]
			break
		}
	}
	if childEvent == nil {
		t.Fatalf("child attestation %q was not repaired", checkpoint.EventID)
	}
	var reference CompactionCheckpointReference
	if err := json.Unmarshal(childEvent.Payload, &reference); err != nil {
		t.Fatal(err)
	}
	wantReference := CompactionCheckpointReference{GenerationID: generation.ID, BranchID: generation.BranchID, GenerationChecksum: generation.Checksum, CheckpointDigest: digestCompactionCheckpoint(checkpoint)}
	if reference != wantReference || childEvent.Type != compactionCheckpointEventType || childEvent.BranchID != branchID {
		t.Fatalf("child attestation = event:%+v reference:%+v, want branch-bound %+v", childEvent, reference, wantReference)
	}
	if err := coordinator.restoreCanonicalCompactionForBranch(branchID); err != nil {
		t.Fatal(err)
	}
	historyDigest := digestMessages(coordinator.conversationHistory)
	sourceRanges := cloneSourceRanges(coordinator.conversationHistorySourceRanges)
	if err := es.Close(); err != nil {
		t.Fatal(err)
	}

	restarted := &Coordinator{session: &TeamSession{Workspace: workspace}, compactionBranchID: branchID}
	restartedES, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	restartedES.SetBranchID(branchID)
	restarted.eventStore = restartedES
	restarted.emittedTaskTransitions = make(map[string]bool)
	if err := restarted.reconcileCompactionState(context.Background(), branchID); err != nil {
		t.Fatal(err)
	}
	if err := restarted.restoreCanonicalCompactionForBranch(branchID); err != nil {
		t.Fatal(err)
	}
	if digestMessages(restarted.conversationHistory) != historyDigest || !reflect.DeepEqual(restarted.conversationHistorySourceRanges, sourceRanges) {
		t.Fatalf("restart changed child history/provenance: digest=%q ranges=%#v", digestMessages(restarted.conversationHistory), restarted.conversationHistorySourceRanges)
	}
	if err := restartedES.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMalformedMaterializedChildCheckpointRetainedParentEventRequiresRecovery(t *testing.T) {
	workspace, branchID, tree := materializedCompactionBranchFixture(t)
	state, _, err := LoadConversationCompactionState(workspace)
	if err != nil {
		t.Fatal(err)
	}
	child := state.Branches[branchID]
	parentEventID := tree.Branches[branchID].ForkEventID
	child.EventID = parentEventID
	state.Branches[branchID] = child
	state.Checkpoints[branchID] = []ConversationCompactionCheckpoint{child}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, conversationCompactionStateFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	es, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	es.SetBranchID(branchID)
	coordinator := &Coordinator{session: &TeamSession{Workspace: workspace}, eventStore: es, emittedTaskTransitions: make(map[string]bool)}
	if err := coordinator.reconcileCompactionState(context.Background(), branchID); err == nil || !strings.Contains(err.Error(), "identity-mismatched") {
		t.Fatalf("retained parent event reconciliation = %v", err)
	}
	for _, event := range mustReadEvents(t, es) {
		if event.ID == compactionGenerationEventID(child.GenerationID) {
			t.Fatal("reconciliation appended a child attestation for a mismatched checkpoint")
		}
	}
	_ = es.Close()
}

func TestMaterializeCompactionBranchRejectsAttestationAfterRequestedFork(t *testing.T) {
	workspace := t.TempDir()
	state := testCompactionState(t, workspace)
	checkpoint := state.Branches["main"]
	state.Branches["main"] = checkpoint
	state.Checkpoints["main"] = []ConversationCompactionCheckpoint{checkpoint}
	if err := SaveConversationCompactionState(workspace, state); err != nil {
		t.Fatal(err)
	}
	es, err := NewEventStore(workspace, "run-time-travel", "session-time-travel")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []RunEvent{
		{ID: checkpoint.EventID, BranchID: "main", Type: compactionCheckpointEventType, Actor: "coordinator", Payload: compactionJSON(CompactionCheckpointReference{
			BranchID: "main", GenerationID: "g-1", GenerationChecksum: state.Generations["g-1"].Checksum,
			CheckpointDigest: digestCompactionCheckpoint(checkpoint),
		})},
		{ID: "requested-fork", BranchID: "main", Type: modelContinuationEventType, Actor: "coordinator", Payload: compactionJSON(map[string]string{"status": "fork"})},
		{ID: compactionGenerationEventID("g-1"), BranchID: "main", Type: compactionGenerationEventType, Actor: "coordinator", Payload: compactionJSON(CompactionReference{GenerationID: "g-1", BranchID: "main", Checksum: state.Generations["g-1"].Checksum})},
	} {
		if _, err := es.AppendPersisted(event); err != nil {
			t.Fatal(err)
		}
	}
	tree := NewSessionTree()
	if _, err := tree.CreateBranch("feature", "requested-fork", es); err != nil {
		t.Fatal(err)
	}
	if err := SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	if err := es.Close(); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeCompactionBranch(workspace, "main", "feature", "requested-fork"); err == nil || !strings.Contains(err.Error(), "matching attestation") {
		t.Fatalf("later-only attestation materialization = %v", err)
	}
}

func mustReadEvents(t *testing.T, es *EventStore) []RunEvent {
	t.Helper()
	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestMaterializeCompactionBranchBeforeFirstCompactionClearsProjection(t *testing.T) {
	workspace := t.TempDir()
	state := testCompactionState(t, workspace)
	generation := state.Generations["g-1"]
	checkpoint := state.Branches["main"]
	checkpoint.SourceRanges = [][]CompactionRange{{{StartIndex: 0, EndIndex: 0, MsgCount: 1}}, {{StartIndex: 1, EndIndex: 1, MsgCount: 1}}, {{StartIndex: 2, EndIndex: 2, MsgCount: 1}}}
	checkpoint.SourceCounts = sourceCountsForRanges(checkpoint.SourceRanges)
	checkpoint.NextSourceIndex = 3
	checkpoint.HistoryDigest = digestMessages(checkpoint.History)
	checkpoint.EventID = ""
	checkpoint.EventID = compactionCheckpointEventID(checkpoint)
	state.Branches["main"] = checkpoint
	state.Checkpoints["main"] = []ConversationCompactionCheckpoint{checkpoint}
	if err := SaveConversationCompactionState(workspace, state); err != nil {
		t.Fatal(err)
	}
	es, err := NewEventStore(workspace, "run-fork-before", "session-fork-before")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := es.AppendPersisted(RunEvent{ID: "before-event", BranchID: "main", Type: "user_message_added", Actor: "user", Payload: compactionJSON(map[string]string{"role": "user", "content": "before"})}); err != nil {
		t.Fatal(err)
	}
	if _, err := es.AppendPersisted(RunEvent{ID: compactionGenerationEventID(generation.ID), BranchID: "main", Type: compactionGenerationEventType, Actor: "coordinator", Payload: compactionJSON(CompactionReference{GenerationID: generation.ID, BranchID: "main", Checksum: generation.Checksum})}); err != nil {
		t.Fatal(err)
	}
	if err := es.Close(); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeCompactionBranch(workspace, "main", "before", "before-event"); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := LoadConversationCompactionState(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := loaded.Branches["before"]; exists {
		t.Fatal("pre-compaction fork retained a compaction checkpoint")
	}
	st := NewSessionTree()
	branch, err := st.CreateBranch("before", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	branch.State.Compaction = &StructuredSummary{Goal: "stale future summary"}
	SnapshotBranchState(workspace, st, branch.ID)
	if branch.State.Compaction != nil {
		t.Fatal("pre-compaction branch retained stale compaction projection")
	}
}

func compactionJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

func TestCompactionCanonicalCommitWinsPostCanonicalFailures(t *testing.T) {
	for _, test := range []struct {
		name       string
		fail       func(t *testing.T, workspace string, c *Coordinator)
		wantReason string
	}{
		{
			name: "legacy projection",
			fail: func(t *testing.T, workspace string, _ *Coordinator) {
				if err := os.Mkdir(filepath.Join(workspace, CompactionHistoryFile), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: "compatibility projection",
		},
		{
			name: "event attestation",
			fail: func(t *testing.T, _ string, c *Coordinator) {
				store, err := NewEventStore(c.session.Workspace, "run-failure", "session-failure")
				if err != nil {
					t.Fatal(err)
				}
				store.syncFile = func() error { return errors.New("injected attestation sync failure") }
				c.eventStore = store
				t.Cleanup(func() { _ = store.Close() })
			},
			wantReason: "attest compaction generation",
		},
		{
			name: "post-commit telemetry",
			fail: func(t *testing.T, _ string, c *Coordinator) {
				store, err := NewEventStore(c.session.Workspace, "run-failure", "session-failure")
				if err != nil {
					t.Fatal(err)
				}
				syncCalls := 0
				store.syncFile = func() error {
					syncCalls++
					if syncCalls == 3 {
						return errors.New("injected telemetry sync failure")
					}
					return nil
				}
				c.eventStore = store
				t.Cleanup(func() { _ = store.Close() })
			},
			wantReason: "persist context window telemetry",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			c := &Coordinator{session: &TeamSession{Workspace: workspace}, compactionBranchID: "main"}
			test.fail(t, workspace, c)
			projection := compactionProjection{
				messages:     []fantasy.Message{fantasy.NewUserMessage(verifiedHistoryPrefix + "committed replacement")},
				summary:      &StructuredSummary{Goal: "committed goal"},
				tokensBefore: 100,
				tokensAfter:  20,
			}
			record, err := c.commitCompactionCheckpoint(t.Context(), projection.messages, 0, []int{1}, []int{1}, projection)
			if err == nil || record.ID == "" || !strings.Contains(err.Error(), test.wantReason) {
				t.Fatalf("commit result = (%+v, %v)", record, err)
			}
			state, exists, loadErr := LoadConversationCompactionState(workspace)
			if loadErr != nil || !exists || len(state.Generations) != 1 {
				t.Fatalf("canonical state after post-commit failure = (exists %v, err %v, state %#v)", exists, loadErr, state)
			}
			if c.compactionState == nil || c.compactionState.Branches["main"].GenerationID != record.ID {
				t.Fatalf("in-memory canonical checkpoint was not published: %#v", c.compactionState)
			}
			if c.lastCompactionSummary == nil || c.lastCompactionSummary.Goal != "committed goal" {
				t.Fatalf("in-memory canonical summary = %#v", c.lastCompactionSummary)
			}
			if c.compactionRecoveryError() == nil {
				t.Fatal("post-canonical projection gap was not marked recoverable")
			}
			if test.name == "event attestation" {
				data, readErr := os.ReadFile(filepath.Join(workspace, logsDir, eventStoreFile))
				if readErr != nil {
					t.Fatal(readErr)
				}
				if strings.Contains(string(data), string(EventContextWindowCompactionCommitted)) {
					t.Fatal("compaction telemetry was persisted after P2 attestation failure")
				}
			}

			if c.eventStore != nil {
				_ = c.eventStore.Close()
			}
			restarted := &Coordinator{session: &TeamSession{Workspace: workspace}}
			attachCompactionEventStore(t, workspace, "main", restarted, state)
			if err := restarted.restoreCanonicalCompactionForBranch("main"); err != nil {
				t.Fatal(err)
			}
			if len(restarted.conversationHistory) != 1 || !strings.Contains(messageText(restarted.conversationHistory[0]), "committed replacement") {
				t.Fatalf("canonical reload history = %#v", restarted.conversationHistory)
			}
		})
	}
}

func TestCompactionTelemetryFollowsProjectionAndAttestations(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-success", "session-success")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	c := &Coordinator{
		session:            &TeamSession{Workspace: workspace},
		eventStore:         store,
		compactionBranchID: "main",
		reportStatus:       func(StatusEvent) {},
	}
	projection := compactionProjection{
		messages:     []fantasy.Message{fantasy.NewUserMessage(verifiedHistoryPrefix + "committed replacement")},
		summary:      &StructuredSummary{Goal: "committed goal"},
		tokensBefore: 100,
		tokensAfter:  20,
	}
	if _, err := c.commitCompactionCheckpoint(t.Context(), projection.messages, 0, []int{1}, []int{1}, projection); err != nil {
		t.Fatal(err)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []string{compactionGenerationEventType, compactionCheckpointEventType, string(EventContextWindowCompactionCommitted)}
	if len(events) != len(wantTypes) {
		t.Fatalf("compaction commit events = %d, want %d: %+v", len(events), len(wantTypes), events)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Fatalf("event %d type = %q, want %q", i, events[i].Type, want)
		}
	}
}

func TestCompactionReferenceReducerDoesNotStoreTranscript(t *testing.T) {
	payload, _ := json.Marshal(CompactionReference{GenerationID: "g-1", BranchID: "main", Checksum: "abc"})
	session := ReduceToSessionData([]RunEvent{{Type: compactionGenerationEventType, Payload: payload}})
	if len(session.CompactionReferences) != 1 || session.CompactionReferences[0].GenerationID != "g-1" {
		t.Fatalf("references = %+v", session.CompactionReferences)
	}
	if strings.Contains(string(payload), "summary") || strings.Contains(string(payload), "history") {
		t.Fatal("compaction event contains transcript fields")
	}
}

func TestCannotFitErrorIsPreProviderOnly(t *testing.T) {
	err := &CannotFitError{ModelID: "m", RequestTokens: 98437, Available: 93232, ProvenNoSend: true}
	got, ok := isProvenPreProviderCannotFit(err)
	if !ok || got.RequestTokens != 98437 || !got.ProvenNoSend {
		t.Fatalf("cannot-fit classification = (%v, %v)", got, ok)
	}
}
