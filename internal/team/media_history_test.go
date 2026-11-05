package team

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/runtimestore"
)

func TestMediaHistoryPersistsReferencesWithoutClaimingCompaction(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx := openMediaInvocation(t, c)
	task := admittedMediaTask(t, c, ctx, "turn")
	state, exists, err := LoadConversationCompactionState(c.session.Workspace)
	if err != nil || !exists {
		t.Fatalf("canonical media history: %v", err)
	}
	if state.Version != mediaCompactionStateVersion || len(state.Generations) != 1 {
		t.Fatal("media history did not use its versioned canonical owner")
	}
	checkpoint := state.Branches["main"]
	generation := state.Generations[checkpoint.GenerationID]
	if generation.Kind != nativeInputGenerationKind || generation.ModelID != "" || generation.TokensBefore != 0 || generation.TokensAfter != 0 || len(generation.SourceRanges) != 0 {
		t.Fatal("input checkpoint claimed model work or compacted source ranges")
	}
	if !reflect.DeepEqual(generation.Summary, StructuredSummary{}) || c.lastCompactionSummary != nil || c.Metrics().Compactions != 0 {
		t.Fatal("input checkpoint fabricated a summary or compaction")
	}
	records, err := LoadCompactionHistory(c.session.Workspace)
	if err != nil || len(records) != 0 {
		t.Fatal("input checkpoint appeared as a completed compaction")
	}
	locations := historyReferences(checkpoint.History)
	if len(locations) != 2 || locations[0].Reference.Ref.ID != task.MediaBinding.Refs[0].ID || locations[0].Reference.Source.PartIndex != 1 || locations[1].Reference.Source.PartIndex != 2 {
		t.Fatal("input image order or repetition was lost")
	}
	for _, name := range []string{runtimestore.DocumentConversationCompactionState, runtimestore.DocumentSessionHistory} {
		data, exists, err := workspaceDocs(c.session.Workspace).getDocument(name)
		if err != nil || !exists {
			t.Fatalf("document %s: %v", name, err)
		}
		for _, forbidden := range []string{base64.StdEncoding.EncodeToString(mediaStorePNG(t)), "data:image/", filepath.Join(c.session.Workspace, "input.png"), `"filename"`} {
			if bytes.Contains(data, []byte(forbidden)) {
				t.Fatalf("document %s persisted image bytes or source path", name)
			}
		}
	}
	if err := os.Remove(filepath.Join(c.session.Workspace, "input.png")); err != nil {
		t.Fatal(err)
	}
	if err := validateResumeConversation(ctx, c.session.Workspace, "main"); err != nil {
		t.Fatal(err)
	}
	c.conversationHistory = nil
	if err := c.restoreCanonicalCompactionForBranch("main"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(historyReferences(c.conversationHistory), locations) {
		t.Fatal("restore lost references or re-read the original input path")
	}
	compat, err := loadConversationHistoryDocument(c.session.Workspace)
	if err != nil || !compat.mediaProjection || compat.checkpointID != checkpoint.EventID {
		t.Fatalf("compatibility envelope: %v", err)
	}
}

func TestMediaHistoryRejectsRawWritesAndUnadmittedSources(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx := openMediaInvocation(t, c)
	_ = admittedMediaTask(t, c, ctx, "turn")
	before, _, err := workspaceDocs(c.session.Workspace).getDocument(runtimestore.DocumentSessionHistory)
	if err != nil {
		t.Fatal(err)
	}
	raw := []fantasy.Message{{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.FilePart{Data: mediaStorePNG(t), MediaType: "image/png"}}}}
	requireMediaCode(t, SaveConversationHistory(c.session.Workspace, raw), media.Unsupported)
	after, _, err := workspaceDocs(c.session.Workspace).getDocument(runtimestore.DocumentSessionHistory)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected raw media changed persisted history")
	}
	state, _, err := LoadConversationCompactionState(c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := state.Branches["main"]
	checkpoint.History = raw
	state.Branches["main"] = checkpoint
	requireMediaCode(t, SaveConversationCompactionState(c.session.Workspace, state), media.Unsupported)
	state, _, err = LoadConversationCompactionState(c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	message := state.Branches["main"].History[0]
	message.Content = slices.Clone(message.Content)
	part := message.Content[1].(agent.MediaReferencePart)
	other, err := media.Digest("unadmitted snapshot")
	if err != nil {
		t.Fatal(err)
	}
	part.Reference.Source.SnapshotHash = other
	part.Reference.Source.SnapshotID = "media-input-v1-" + other[len("sha256:"):]
	part.Reference.BindingHash = other
	message.Content[1] = part
	requireMediaCode(t, validateMediaHistorySources(ctx, c.session.Workspace, "main", []fantasy.Message{message}), media.AccessDenied)
}

func TestMediaHistoryCannotResumeFromCompatibilityProjection(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx := openMediaInvocation(t, c)
	_ = admittedMediaTask(t, c, ctx, "turn")
	if err := workspaceDocs(c.session.Workspace).deleteDocument(runtimestore.DocumentConversationCompactionState); err != nil {
		t.Fatal(err)
	}
	requireMediaCode(t, validateResumeConversation(ctx, c.session.Workspace, "main"), media.DurabilityRequired)
	if err := SaveConversationHistory(c.session.Workspace, c.conversationHistory); err == nil {
		t.Fatal("compatibility history replaced the missing canonical owner")
	}
}

func TestMediaHistoryPinsReferencesDuringTextCompaction(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx := openMediaInvocation(t, c)
	_ = admittedMediaTask(t, c, ctx, "turn")
	input := cloneMessages(c.conversationHistory)
	messages := append(input, fantasy.NewUserMessage("follow-up text"), fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "text observation"}}})
	counts := []int{1, 1, 1}
	projection := c.buildCompactionProjection(ctx, messages, 0, counts, nil, &mockCompacter{response: `{"goal":"compare image"}`})
	if projection.err != nil || projection.summary == nil {
		t.Fatalf("media-aware text compaction: %v", projection.err)
	}
	if !reflect.DeepEqual(historyReferences(messages), historyReferences(projection.messages)) {
		t.Fatal("compaction moved or dropped input references")
	}
	ranges := historySourceRanges(len(messages), 0, counts, nil)
	projected := projectionHistoryRanges(projection, ranges)
	if err := validateHistorySourceRanges(projected); err != nil {
		t.Fatal(err)
	}
	claims := projectionCompactedRanges(projection, ranges)
	if len(claims) != 1 || claims[0].StartIndex != 1 || claims[0].EndIndex != 2 {
		t.Fatal("text summary claimed the pinned image's source range")
	}
	trimmed, _, removed := trimHistoryPreservingHeadWithProvenance(messages, ranges, 1)
	if removed != 0 || !reflect.DeepEqual(trimmed, messages) {
		t.Fatal("hard trimming deleted an image reference")
	}
	if _, changed := squeezeMessage(input[0], 1); changed {
		t.Fatal("generic squeezing changed a media input message")
	}
	encoded, err := json.Marshal(projection.messages)
	if err != nil || bytes.Contains(encoded, []byte(base64.StdEncoding.EncodeToString(mediaStorePNG(t)))) {
		t.Fatal("compaction serialized native image bytes")
	}
}

func TestMediaHistoryLoadsInertRefsAndRejectsMissingContentBeforeResume(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx := openMediaInvocation(t, c)
	task := admittedMediaTask(t, c, ctx, "turn")
	store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.mediaPath("data", task.MediaBinding.Refs[0].ID)); err != nil {
		t.Fatal(err)
	}
	state, exists, err := LoadConversationCompactionState(c.session.Workspace)
	if err != nil || !exists || len(historyReferences(state.Branches["main"].History)) != 2 {
		t.Fatal("loading references eagerly read image bytes")
	}
	if err := c.restoreCanonicalCompactionForBranch("main"); err != nil {
		t.Fatal(err)
	}
	requireMediaCode(t, validateResumeConversation(ctx, c.session.Workspace, "main"), media.InputMissing)
}

func TestMediaAuditRequiresAtomicTaskBindingAndCanonicalContent(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx := openMediaInvocation(t, c)
	task := admittedMediaTask(t, c, ctx, "turn")
	item := createMediaOccurrence(t, c, ctx, task)
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyNativeMediaLineage(ctx, c.session.Workspace, events); err != nil {
		t.Fatal(err)
	}
	var missingBinding []RunEvent
	for _, event := range events {
		if event.Type != string(EventTaskMediaBound) {
			missingBinding = append(missingBinding, event)
		}
	}
	requireMediaCode(t, VerifyNativeMediaLineage(ctx, c.session.Workspace, missingBinding), media.DurabilityRequired)
	var missingSources []RunEvent
	for _, event := range events {
		if event.Type != string(EventTaskMediaBound) && event.Type != string(EventMediaInputAdmitted) {
			missingSources = append(missingSources, event)
		}
	}
	if err := VerifyNativeMediaLineage(ctx, c.session.Workspace, missingSources); err == nil {
		t.Fatal("a media task with all source events missing bypassed native audit")
	}
	store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.mediaPath("data", item.MediaBinding.Refs[0].ID)); err != nil {
		t.Fatal(err)
	}
	requireMediaCode(t, VerifyNativeMediaLineage(ctx, c.session.Workspace, events), media.InputMissing)
}

func TestMediaHistoryResetPersistsWithoutReleasingQuota(t *testing.T) {
	c := newMediaPolicyCoordinator(t, media.Limits{MaxBranchBlobs: 1})
	ctx := openMediaInvocation(t, c)
	_ = admittedMediaTask(t, c, ctx, "turn")
	if err := c.ResetConversationChecked(ctx); err != nil {
		t.Fatal(err)
	}
	state, exists, err := LoadConversationCompactionState(c.session.Workspace)
	if err != nil || !exists {
		t.Fatalf("reset checkpoint: %v", err)
	}
	checkpoint := state.Branches["main"]
	if len(checkpoint.History) != 0 || state.Generations[checkpoint.GenerationID].Kind != nativeResetGenerationKind {
		t.Fatal("reset did not persist an empty canonical context")
	}
	if err := c.restoreCanonicalCompactionForBranch("main"); err != nil || len(c.conversationHistory) != 0 || c.lastCompactionSummary != nil {
		t.Fatalf("reset restored prior media context: %v", err)
	}
	if len(c.SessionData().MediaInputSnapshots) != 1 {
		t.Fatal("reset erased admitted quota evidence")
	}
	_, err = c.admitMediaInput(ctx, mediaQuotaCandidate(t, c, "second", 3))
	requireMediaCode(t, err, media.LimitExceeded)
	if err := validateResumeConversation(ctx, c.session.Workspace, "main"); err != nil {
		t.Fatal(err)
	}
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyNativeMediaLineage(ctx, c.session.Workspace, events); err != nil {
		t.Fatal(err)
	}
}

func TestMediaHistoryResetReportsFailedAttestation(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx := openMediaInvocation(t, c)
	_ = admittedMediaTask(t, c, ctx, "turn")
	c.eventStore.commitHook = func() error { return errors.New("injected reset attestation failure") }
	if err := c.ResetConversationChecked(ctx); err == nil || c.compactionRecoveryError() == nil {
		t.Fatal("reset reported success despite missing durable attestation")
	}
	c.eventStore.commitHook = nil
	if err := c.eventStore.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewEventStore(c.session.Workspace, c.eventStore.runID, c.eventStore.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	reopened.SetBranchID("main")
	c.eventStore = reopened
	c.SetEventJournal(eventStoreJournal{store: reopened})
	if err := c.reconcileCompactionState(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if err := c.restoreCanonicalCompactionForBranch("main"); err != nil || len(c.conversationHistory) != 0 {
		t.Fatalf("recovery did not retain the committed reset: %v", err)
	}
}

func TestMediaHistoryRetryRestoresCommittedInput(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx := openMediaInvocation(t, c)
	_ = admittedMediaTask(t, c, ctx, "turn")
	view, err := c.mediaLineage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c.conversationHistory = nil
	for _, snapshot := range view.snapshots {
		if err := c.persistMediaInputHistory(ctx, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	if len(historyReferences(c.conversationHistory)) != 2 {
		t.Fatal("idempotent retry did not restore committed reference occurrences")
	}
}

func TestMediaHistoryRejectsSourcesAfterBranchCutoff(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx := openMediaInvocation(t, c)
	tree, err := LoadSessionTree(c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := tree.CreateBranch("before-image", "main", c.eventStore)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveSessionTree(c.session.Workspace, tree); err != nil {
		t.Fatal(err)
	}
	_ = admittedMediaTask(t, c, ctx, "turn")
	requireMediaCode(t, validateMediaHistorySources(ctx, c.session.Workspace, branch.ID, c.conversationHistory), media.AccessDenied)
	if err := validateMediaHistorySources(ctx, c.session.Workspace, "main", c.conversationHistory); err != nil {
		t.Fatal(err)
	}
}

func TestMediaHistoryTextCompactionReplaysCanonicalReferences(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx := openMediaInvocation(t, c)
	_ = admittedMediaTask(t, c, ctx, "turn")
	messages := append(cloneMessages(c.conversationHistory), fantasy.NewUserMessage("follow-up"), fantasy.NewUserMessage("more text"))
	ranges := historySourceRanges(len(messages), 0, nil, nil)
	projection := c.buildCompactionProjection(ctx, messages, 0, nil, nil, &mockCompacter{response: `{"goal":"compare image"}`})
	if projection.err != nil || projection.summary == nil {
		t.Fatalf("text compaction: %v", projection.err)
	}
	projectedRanges := projectionHistoryRanges(projection, ranges)
	_, err := c.commitCompactionCheckpointWithProvenance(ctx, projection.messages, 0, sourceCountsForRanges(projectedRanges), projectionCompactedRanges(projection, ranges), projectedRanges, 3, projection)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.restoreCanonicalCompactionForBranch("main"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(historyReferences(c.conversationHistory), historyReferences(messages)) {
		t.Fatal("canonical compaction replay changed native references")
	}
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyNativeMediaLineage(ctx, c.session.Workspace, events); err != nil {
		t.Fatal(err)
	}
}

type rawHistoryMarshalTrap struct{}

func (rawHistoryMarshalTrap) GetType() fantasy.ContentType     { return fantasy.ContentTypeFile }
func (rawHistoryMarshalTrap) Options() fantasy.ProviderOptions { return nil }
func (rawHistoryMarshalTrap) MarshalJSON() ([]byte, error) {
	panic("raw media reached a checkpoint JSON encoder")
}

func TestMediaHistoryRejectsRawBeforeCheckpointDigests(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx := openMediaInvocation(t, c)
	_ = admittedMediaTask(t, c, ctx, "turn")
	raw := []fantasy.Message{{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{rawHistoryMarshalTrap{}}}}
	for _, test := range []struct {
		name  string
		write func(context.Context) error
	}{
		{"history", func(context.Context) error { return SaveConversationHistory(c.session.Workspace, raw) }},
		{"checkpoint", func(context.Context) error {
			return c.persistConversationCheckpointWithProvenance(raw, 0, []int{1}, nil, 1)
		}},
		{"compaction", func(ctx context.Context) error {
			_, err := c.commitCompactionCheckpoint(ctx, raw, 0, []int{1}, []int{1}, compactionProjection{messages: raw, summary: &StructuredSummary{Goal: "fixture"}})
			return err
		}},
		{"native_input", func(ctx context.Context) error { return c.commitNativeInputCheckpoint(ctx, nil, "main", raw, nil, 1) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireMediaCode(t, test.write(ctx), media.Unsupported)
		})
	}
}
