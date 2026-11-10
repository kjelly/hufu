package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/conversation"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/team"
)

func chatImageFixture(t *testing.T) (string, chatImageStage) {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(t.TempDir(), "private-source-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "photo-\x1b[31m\u202e.png")
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, func(ctx context.Context, path string, _ []media.Ref) (*media.StagedImage, error) {
		return media.StageImage(ctx, path, media.Limits{})
	}
}

func requireChatMediaCode(t *testing.T, err error, code media.Code) {
	t.Helper()
	if classified, ok := errors.AsType[*media.Error](err); !ok || classified.Code != code {
		t.Fatalf("image operation: err=%v want=%s", err, code)
	}
}

func TestChatPendingImagesPreserveOrderAndSafeMetadata(t *testing.T) {
	path, stage := chatImageFixture(t)
	pending := &chatPendingImages{}
	t.Cleanup(func() { pending.close() })
	for range 2 {
		if _, err := pending.manage(t.Context(), conversation.ImageCommand{Operation: conversation.ImageAdd, Path: path}, stage); err != nil {
			t.Fatal(err)
		}
	}
	status := pending.status()
	if len(status.Images) != 2 || status.Images[0].ID == status.Images[1].ID || status.Images[0].Name != status.Images[1].Name || status.Images[0].ByteSize <= 0 {
		t.Fatal("pending image occurrences lost identity or validated metadata")
	}
	encoded, err := json.Marshal(status)
	if err != nil || bytes.Contains(encoded, []byte("private-source-parent")) || strings.ContainsRune(status.Images[0].Name, '\x1b') || strings.ContainsRune(status.Images[0].Name, '\u202e') {
		t.Fatal("pending listing exposed a path or terminal controls", err)
	}
	submission, err := pending.begin("first-turn")
	if err != nil || len(submission.images) != 2 || pending.status().Images[0].State != conversation.ImageSubmitting {
		t.Fatal("turn did not freeze its ordered pending input", err)
	}
	for _, command := range []conversation.ImageCommand{{Operation: conversation.ImageAdd, Path: path}, {Operation: conversation.ImageRemove, Index: 1}, {Operation: conversation.ImageClear}} {
		_, err := pending.manage(t.Context(), command, stage)
		requireChatMediaCode(t, err, media.AccessDenied)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if state := submission.finish(); state.State != team.MediaTurnNotCommitted || len(pending.status().Images) != 2 || pending.status().Images[0].State != conversation.ImagePending {
		t.Fatal("uncommitted turn consumed its valid input")
	}
	retry, err := pending.begin("retry-turn")
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range retry.images {
		if err := input.WithBytes(t.Context(), func([]byte, media.Ref) error { return nil }); err != nil {
			t.Fatal("retry re-read its removed original path", err)
		}
	}
	notice := team.MediaTurnAdmission{State: team.MediaTurnCommitted, TurnID: "retry-turn", SnapshotID: "canonical-input", ImageCount: 2}
	retry.admit(notice)
	retry.admit(notice)
	if len(pending.status().Images) != 0 {
		t.Fatal("committed turn retained pending image occurrences")
	}
	for _, input := range retry.images {
		requireChatMediaCode(t, input.WithBytes(t.Context(), func([]byte, media.Ref) error { return nil }), media.InputMissing)
	}
	retry.finish()
	if next, err := pending.begin("next-turn"); err != nil || next != nil {
		t.Fatal("next turn implicitly resent committed images", err)
	}
}

func TestChatPendingImagesRejectUnknownCommitBeforeReuse(t *testing.T) {
	for _, scenario := range []string{"unknown", "wrong turn", "wrong count", "missing snapshot"} {
		t.Run(scenario, func(t *testing.T) {
			path, stage := chatImageFixture(t)
			pending := &chatPendingImages{}
			t.Cleanup(func() { pending.close() })
			if _, err := pending.manage(t.Context(), conversation.ImageCommand{Operation: conversation.ImageAdd, Path: path}, stage); err != nil {
				t.Fatal(err)
			}
			submission, err := pending.begin("turn")
			if err != nil {
				t.Fatal(err)
			}
			notice := team.MediaTurnAdmission{State: team.MediaTurnCommitted, TurnID: "turn", SnapshotID: "snapshot", ImageCount: 1}
			switch scenario {
			case "unknown":
				notice.State = team.MediaTurnRecoveryRequired
			case "wrong turn":
				notice.TurnID = "different-turn"
			case "wrong count":
				notice.ImageCount = 2
			case "missing snapshot":
				notice.SnapshotID = ""
			}
			submission.admit(notice)
			if submission.finish().State != team.MediaTurnRecoveryRequired || pending.status().Images[0].State != conversation.ImageRecoveryRequired {
				t.Fatal("unknown commit became ordinary reusable pending input")
			}
			_, err = pending.begin("another-turn")
			requireChatMediaCode(t, err, media.DurabilityRequired)
			status, err := pending.manage(t.Context(), conversation.ImageCommand{Operation: conversation.ImageClear}, stage)
			if err != nil || status.Removed != 1 || len(status.Images) != 0 {
				t.Fatal("explicit clear did not release uncertain process-local input", err)
			}
		})
	}
}

func TestChatPendingImagesRemoveByDisplayedIndexAndBoundCount(t *testing.T) {
	path, stage := chatImageFixture(t)
	pending := &chatPendingImages{}
	t.Cleanup(func() { pending.close() })
	for range media.MaxCount {
		if _, err := pending.manage(t.Context(), conversation.ImageCommand{Operation: conversation.ImageAdd, Path: path}, stage); err != nil {
			t.Fatal(err)
		}
	}
	_, err := pending.manage(t.Context(), conversation.ImageCommand{Operation: conversation.ImageAdd, Path: path}, stage)
	requireChatMediaCode(t, err, media.LimitExceeded)
	first := pending.images[0].image
	secondID := pending.status().Images[1].ID
	status, err := pending.manage(t.Context(), conversation.ImageCommand{Operation: conversation.ImageRemove, Index: 1}, stage)
	if err != nil || status.Removed != 1 || status.Images[0].ID != secondID {
		t.Fatal("remove changed occurrence order", err)
	}
	requireChatMediaCode(t, first.WithBytes(t.Context(), func([]byte, media.Ref) error { return nil }), media.InputMissing)
	if removed := pending.close(); removed != media.MaxCount-1 || len(pending.status().Images) != 0 || pending.close() != 0 {
		t.Fatal("shutdown retained process-local images")
	}
}

func TestChatPendingImagesConcurrentListingAndCommit(t *testing.T) {
	path, stage := chatImageFixture(t)
	pending := &chatPendingImages{}
	t.Cleanup(func() { pending.close() })
	if _, err := pending.manage(t.Context(), conversation.ImageCommand{Operation: conversation.ImageAdd, Path: path}, stage); err != nil {
		t.Fatal(err)
	}
	submission, err := pending.begin("turn")
	if err != nil {
		t.Fatal(err)
	}
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for range 100 {
				pending.status()
			}
		})
	}
	submission.admit(team.MediaTurnAdmission{State: team.MediaTurnCommitted, TurnID: "turn", SnapshotID: "snapshot", ImageCount: 1})
	submission.finish()
	done := make(chan struct{})
	go func() { readers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pending listing did not drain")
	}
}

func TestChatPendingImagesPreserveExistingInputAfterFailedStaging(t *testing.T) {
	for _, lateCancellation := range []bool{false, true} {
		t.Run(fmt.Sprint(lateCancellation), func(t *testing.T) {
			path, stage := chatImageFixture(t)
			pending := &chatPendingImages{}
			t.Cleanup(func() { pending.close() })
			add := conversation.ImageCommand{Operation: conversation.ImageAdd, Path: path}
			if _, err := pending.manage(t.Context(), add, stage); err != nil {
				t.Fatal(err)
			}
			original := pending.status().Images[0]
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var rejected *media.StagedImage
			failure := errors.New("staging failed")
			failing := func(ctx context.Context, path string, refs []media.Ref) (*media.StagedImage, error) {
				var err error
				rejected, err = stage(ctx, path, refs)
				if err != nil {
					t.Fatal(err)
				}
				if lateCancellation {
					cancel()
					return rejected, nil
				}
				return rejected, failure
			}
			status, err := pending.manage(ctx, add, failing)
			want := failure
			if lateCancellation {
				want = context.Canceled
			}
			if !errors.Is(err, want) || len(status.Images) != 1 || status.Images[0] != original {
				t.Fatal("failed staging changed existing pending input", status, err)
			}
			requireChatMediaCode(t, rejected.WithBytes(t.Context(), func([]byte, media.Ref) error { return nil }), media.InputMissing)
		})
	}
}
