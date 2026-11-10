package team

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/media"
)

func mediaQuotaCandidate(t *testing.T, c *Coordinator, turn string, side int) media.InputSnapshot {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, side, side))); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileArtifactStore(c.session.Workspace, c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	put, err := store.Put(t.Context(), PutArtifactRequest{Kind: "image", Content: buf.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := store.mediaMetadata(t.Context(), put.ID)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := c.requiredMediaPolicy()
	if err != nil {
		t.Fatal(err)
	}
	s := media.InputSnapshot{RunID: c.executionRunID, BranchID: c.activeBranchID(), InvocationID: c.currentInvocationID(), TurnID: turn, PolicyHash: policy.Hash,
		Input: media.TurnInput{Parts: []media.Part{{Type: "text", Text: "inspect image"}, {Type: "image", MediaID: put.ID}}}, Refs: []media.Ref{*metadata.Image},
		Occurrences: []media.Occurrence{{RunID: c.executionRunID, BranchID: c.activeBranchID(), InvocationID: c.currentInvocationID(), TurnID: turn, PartIndex: 1, MediaID: put.ID, Origin: "trusted_artifact"}}}
	s, err = media.SealSnapshot(s, policy.Policy.Image)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mediaJournalPeer(t *testing.T, parent *Coordinator, runID string) *Coordinator {
	t.Helper()
	c := newTestCoordinator(t)
	session := *parent.session
	c.session = &session
	c.projectDir = parent.projectDir
	c.executionRunID = runID
	c.executionPolicy = parent.executionPolicy
	c.SetSessionData(parent.SessionData())
	store, err := NewEventStore(session.Workspace, runID, parent.eventStore.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	store.SetBranchID(c.activeBranchID())
	c.eventStore = store
	c.SetInvocationID(parent.currentInvocationID())
	return c
}

type mediaQuotaBarrier struct {
	ConditionalEventJournal
	arrived *atomic.Int64
	ready   chan struct{}
}

func (j mediaQuotaBarrier) AppendAtHead(ctx context.Context, e RunEvent, head string) (RunEvent, error) {
	if j.arrived.Add(1) == 2 {
		close(j.ready)
	}
	select {
	case <-j.ready:
	case <-ctx.Done():
		return RunEvent{}, ctx.Err()
	}
	return j.ConditionalEventJournal.AppendAtHead(ctx, e, head)
}

func TestMediaQuotaAdmissionAcrossEventStoreHandles(t *testing.T) {
	c := newMediaPolicyCoordinator(t, media.Limits{MaxBranchBlobs: 1})
	ctx := openMediaInvocation(t, c)
	peer := mediaJournalPeer(t, c, c.executionRunID)
	inputs := []media.InputSnapshot{mediaQuotaCandidate(t, c, "first", 2), mediaQuotaCandidate(t, peer, "second", 3)}
	ready := make(chan struct{})
	var arrived atomic.Int64
	for _, coord := range []*Coordinator{c, peer} {
		coord.SetEventJournal(mediaQuotaBarrier{ConditionalEventJournal: coord.EventJournal().(ConditionalEventJournal), arrived: &arrived, ready: ready})
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for index, coord := range []*Coordinator{c, peer} {
		wg.Go(func() { _, err := coord.admitMediaInput(ctx, inputs[index]); results <- err })
	}
	wg.Wait()
	close(results)
	winners, denied := 0, 0
	for err := range results {
		if err == nil {
			winners++
			continue
		}
		if e, ok := errors.AsType[*media.Error](err); ok && e.Code == media.LimitExceeded {
			denied++
			continue
		}
		t.Fatalf("unexpected admission failure: %v", err)
	}
	if winners != 1 || denied != 1 {
		t.Fatalf("quota winners=%d denied=%d", winners, denied)
	}
	view, err := c.mediaLineage(t.Context())
	if err != nil || len(view.snapshots) != 1 {
		t.Fatalf("canonical admitted inputs=%d error=%v", len(view.snapshots), err)
	}
}

func TestMediaQuotaFollowsForkLineageAcrossRuns(t *testing.T) {
	c := newMediaPolicyCoordinator(t, media.Limits{MaxBranchBlobs: 1})
	ctx := openMediaInvocation(t, c)
	first := mediaQuotaCandidate(t, c, "parent-turn", 2)
	if _, err := c.admitMediaInput(ctx, first); err != nil {
		t.Fatal(err)
	}
	tree, err := LoadSessionTree(c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := tree.CreateBranch("child", "main", c.eventStore)
	if err != nil {
		t.Fatal(err)
	}
	tree.ActiveBranch = branch.ID
	if err := SaveSessionTree(c.session.Workspace, tree); err != nil {
		t.Fatal(err)
	}
	child := mediaJournalPeer(t, c, newExecutionRunID())
	second := mediaQuotaCandidate(t, child, "child-turn", 3)
	_, err = child.admitMediaInput(t.Context(), second)
	requireMediaCode(t, err, media.LimitExceeded)
	// An explicitly reauthorized identical blob is free in branch storage;
	// its new occurrence still counts against the turn/request bundle limits.
	same := mediaQuotaCandidate(t, child, "same-content", 2)
	if _, err := child.admitMediaInput(t.Context(), same); err != nil {
		t.Fatal(err)
	}
	view, err := child.mediaLineage(t.Context())
	if err != nil || len(view.snapshots) != 2 {
		t.Fatalf("inherited media inputs: %d %v", len(view.snapshots), err)
	}
	// The old live coordinator cannot follow a branch switch implicitly.
	_, err = c.admitMediaInput(t.Context(), first)
	requireMediaCode(t, err, media.AccessDenied)
	for _, target := range []struct {
		branch string
		count  int
	}{{branch.ID, 2}, {"main", 1}, {branch.ID, 2}} {
		tree.ActiveBranch = target.branch
		if err := SaveSessionTree(c.session.Workspace, tree); err != nil {
			t.Fatal(err)
		}
		if err := RebuildSessionForBranch(c.session.Workspace, tree, c.eventStore, target.branch); err != nil {
			t.Fatal(err)
		}
		restored := LoadSession(c.session.Workspace)
		if restored == nil || len(restored.MediaInputSnapshots) != target.count {
			t.Fatal("branch checkout lost or leaked media input snapshots")
		}
		// Refresh must repair a stale checkpoint from the same canonical lineage.
		restored.MediaInputSnapshots = nil
		if err := SaveSession(c.session.Workspace, restored); err != nil {
			t.Fatal(err)
		}
		if err := RefreshActiveBranchProjection(c.session.Workspace, tree, c.eventStore); err != nil {
			t.Fatal(err)
		}
		if refreshed := LoadSession(c.session.Workspace); refreshed == nil || len(refreshed.MediaInputSnapshots) != target.count {
			t.Fatal("branch refresh did not restore canonical media snapshots")
		}
	}
}
