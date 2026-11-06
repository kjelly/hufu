package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/conversation"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/team"
)

type fakeMediaChatCoordinator struct {
	*fakeChatCoordinator
	admissions     []team.MediaTurnAdmissionState
	afterAdmission func(team.MediaTurnRequest)
	mediaCalls     int
}

func (f *fakeMediaChatCoordinator) mediaTurn(prompt, kind string, request team.MediaTurnRequest) (string, error) {
	f.prompts = append(f.prompts, prompt)
	if f.mediaCalls < len(f.admissions) && f.admissions[f.mediaCalls] != "" {
		request.OnAdmission(team.MediaTurnAdmission{State: f.admissions[f.mediaCalls], TurnID: request.TurnID, SnapshotID: "checkpoint-" + request.TurnID, ImageCount: len(request.StagedImages)})
	}
	f.mediaCalls++
	if f.afterAdmission != nil {
		f.afterAdmission(request)
	}
	return f.next(kind)
}

func (f *fakeMediaChatCoordinator) RunWithMedia(_ context.Context, prompt string, request team.MediaTurnRequest) (string, error) {
	return f.mediaTurn(prompt, "run_media", request)
}

func (f *fakeMediaChatCoordinator) ContinueWithMedia(_ context.Context, prompt string, request team.MediaTurnRequest) (string, error) {
	return f.mediaTurn(prompt, "continue_media", request)
}

func TestRunChatImageTurnCountsCanonicalAdmissionRatherThanProviderSuccess(t *testing.T) {
	for _, test := range []struct {
		name                   string
		state                  team.MediaTurnAdmissionState
		failure                error
		cancelled              bool
		wantTurns, wantPending int
	}{
		{"committed success", team.MediaTurnCommitted, nil, false, 1, 0},
		{"committed provider failure", team.MediaTurnCommitted, errors.New("provider unavailable"), false, 1, 0},
		{"committed cancellation", team.MediaTurnCommitted, context.Canceled, true, 1, 0},
		{"uncommitted failure", "", errors.New("input admission failed"), false, 0, 1},
		{"uncommitted cancellation", "", context.Canceled, true, 0, 1},
		{"success without acknowledgment", "", nil, false, 0, 1},
		{"unknown commit", team.MediaTurnRecoveryRequired, errors.New("checkpoint unavailable"), false, 0, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			preserveInvocationID(t)
			path, stage := chatImageFixture(t)
			pending := &chatPendingImages{}
			t.Cleanup(func() { pending.close() })
			if _, err := pending.manage(t.Context(), conversation.ImageCommand{Operation: conversation.ImageAdd, Path: path}, stage); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			base := &fakeChatCoordinator{results: []error{test.failure, nil}, answer: "answer", cancel: cancel}
			coordinator := &fakeMediaChatCoordinator{fakeChatCoordinator: base, admissions: []team.MediaTurnAdmissionState{test.state}}
			coordinator.afterAdmission = func(request team.MediaTurnRequest) {
				if test.state == team.MediaTurnCommitted {
					requireChatMediaCode(t, request.StagedImages[0].WithBytes(t.Context(), func([]byte, media.Ref) error { return nil }), media.InputMissing)
				}
			}
			var saved []string
			deps := fakeChatTurnDeps(base, &saved)
			deps.coordinator = coordinator
			sequencer := &chatTurnSequencer{images: pending}
			outcome := runChatTurn(ctx, deps, sequencer, "image-turn", "inspect this image")
			wantError := test.failure != nil || test.state != team.MediaTurnCommitted
			if (outcome.Err != nil) != wantError || outcome.Cancelled != test.cancelled || outcome.InputAdmission == nil || sequencer.turns != test.wantTurns || len(pending.status().Images) != test.wantPending {
				t.Fatalf("image turn: err=%v cancelled=%v notice=%+v turns=%d pending=%d", outcome.Err, outcome.Cancelled, outcome.InputAdmission, sequencer.turns, len(pending.status().Images))
			}
			if test.state == team.MediaTurnCommitted {
				followup := runChatTurn(t.Context(), deps, sequencer, "plain-turn", "explain the result")
				if followup.Err != nil || !reflect.DeepEqual(base.calls, []string{"run_media", "continue"}) {
					t.Fatal("committed images were automatically attached again", base.calls, followup.Err)
				}
			}
		})
	}
}

func TestRunChatTurnExcludesRetainedImagesForQueuedPrompt(t *testing.T) {
	preserveInvocationID(t)
	path, stage := chatImageFixture(t)
	pending := &chatPendingImages{}
	t.Cleanup(func() { pending.close() })
	if _, err := pending.manage(t.Context(), conversation.ImageCommand{Operation: conversation.ImageAdd, Path: path}, stage); err != nil {
		t.Fatal(err)
	}
	base := &fakeChatCoordinator{results: []error{errors.New("precommit failure"), nil, nil}, answer: "answer"}
	coordinator := &fakeMediaChatCoordinator{fakeChatCoordinator: base, admissions: []team.MediaTurnAdmissionState{"", team.MediaTurnCommitted}}
	var saved []string
	deps := fakeChatTurnDeps(base, &saved)
	deps.coordinator = coordinator
	sequencer := &chatTurnSequencer{images: pending}
	if outcome := runChatTurn(t.Context(), deps, sequencer, "first", "first"); outcome.Err == nil {
		t.Fatal("fixture did not fail before commit")
	}
	deps.skipPending = true
	if outcome := runChatTurn(t.Context(), deps, sequencer, "queued", "queued text"); outcome.Err != nil || len(pending.status().Images) != 1 {
		t.Fatal("queued text consumed retained images", outcome.Err)
	}
	deps.skipPending = false
	if outcome := runChatTurn(t.Context(), deps, sequencer, "retry", "explicit image retry"); outcome.Err != nil || len(pending.status().Images) != 0 {
		t.Fatal("explicit retry did not consume its own canonical image turn", outcome.Err)
	}
	if !reflect.DeepEqual(base.calls, []string{"run_media", "run", "continue_media"}) {
		t.Fatal("queued and explicit submissions shared attachment ownership", base.calls)
	}
}

func TestChatImageCommandsDoNotEnterPromptHistory(t *testing.T) {
	for _, line := range []string{"/image add /private/file.png", "/image list", "/image clear", "/image remove 1"} {
		if chatHistoryAllowsLine(line) {
			t.Fatal("image control command retained its path in prompt history")
		}
	}
	for _, line := range []string{"ordinary prompt", "/help", "//image is literal text"} {
		if !chatHistoryAllowsLine(line) {
			t.Fatal("ordinary prompt disappeared from history")
		}
	}
}
