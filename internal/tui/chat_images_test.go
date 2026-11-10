package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/conversation"
	"github.com/kjelly/hufu/internal/team"
)

func pendingImageStatus() conversation.ImageStatus {
	return conversation.ImageStatus{Images: []conversation.PendingImageInfo{{ID: "pending-1", Name: "screen.png", MediaType: "image/png", Width: 2, Height: 3, ByteSize: 80, State: conversation.ImagePending}}}
}

func TestChatImageCommandsUseTypedControls(t *testing.T) {
	for _, test := range []struct {
		text      string
		operation conversation.ImageOperation
		path      string
		index     int
		valid     bool
	}{
		{`/image add "/private/image with spaces.png"`, conversation.ImageAdd, "/private/image with spaces.png", 0, true},
		{"/image", conversation.ImageList, "", 0, true},
		{"/image list", conversation.ImageList, "", 0, true},
		{"/image remove 2", conversation.ImageRemove, "", 2, true},
		{"/image clear", conversation.ImageClear, "", 0, true},
		{"/image remove 0", "", "", 0, false},
		{"/image add", "", "", 0, false},
	} {
		t.Run(test.text, func(t *testing.T) {
			model, runtime := newTestChat(t, 100, 30)
			model = chatSubmit(t, model, test.text)
			if !test.valid {
				if len(runtime.commands) != 0 || model.notice == "" {
					t.Fatal("invalid image control reached the runtime")
				}
				return
			}
			if len(runtime.commands) != 1 {
				t.Fatal("image control became a model prompt")
			}
			command := runtime.commands[0]
			if command.Kind != conversation.CommandImages || command.Image.Operation != test.operation || command.Image.Path != test.path || command.Image.Index != test.index || command.Text != "" {
				t.Fatal("image control changed its explicit input")
			}
		})
	}
}

func TestChatImageViewSeparatesAdmissionFromGoalOutcome(t *testing.T) {
	model, _ := newTestChat(t, 100, 30)
	status := pendingImageStatus()
	model = chatEvent(t, model, conversation.ControlDone{Kind: conversation.CommandImages, ImageOperation: conversation.ImageList, Images: &status})
	if model.overlayTitle != "Pending images" || !strings.Contains(model.overlayBody, "screen.png") || !strings.Contains(model.overlayBody, "2×3") || !strings.Contains(model.statusText(), "images:1") {
		t.Fatal("pending listing lost safe image metadata")
	}
	model = chatEvent(t, model, conversation.TurnStarted{TurnID: "image-turn", IncludePending: true})
	if !strings.Contains(model.statusText(), "images:1 submitting") {
		t.Fatal("view did not freeze attachments with their turn")
	}
	model = chatEvent(t, model, conversation.TurnInputAdmission{TurnID: "image-turn", Admission: team.MediaTurnAdmission{State: team.MediaTurnCommitted, TurnID: "image-turn", SnapshotID: "snapshot", ImageCount: 1}})
	if len(model.pendingImages.Images) != 0 || model.lastResult != nil || model.phase != phaseRunning {
		t.Fatal("image admission manufactured run completion or retained pending input")
	}
	failed := &team.RunResult{Outcome: team.RunOutcomeFailed, GoalSatisfied: false}
	empty := conversation.ImageStatus{}
	model = chatEvent(t, model, conversation.TurnSettled{Result: conversation.TurnResult{TurnID: "image-turn", Err: errors.New("provider failed after admission"), Result: failed, Images: &empty}})
	if model.lastResult != failed || model.lastResult.GoalSatisfied {
		t.Fatal("committed images erased a failed canonical outcome")
	}
	before := len(model.transcript.items)
	model = chatEvent(t, model, conversation.TurnInputAdmission{TurnID: "image-turn", Admission: team.MediaTurnAdmission{State: team.MediaTurnNotCommitted, ImageCount: 1}})
	if len(model.transcript.items) != before || len(model.pendingImages.Images) != 0 {
		t.Fatal("late admission resurrected a settled turn's attachments")
	}
}

func TestChatImageViewPreservesExcludedPendingAndFailedControls(t *testing.T) {
	for _, control := range []conversation.CommandKind{conversation.CommandResetContext, conversation.CommandSwitchTeam} {
		t.Run(string(control), func(t *testing.T) {
			model, _ := newTestChat(t, 100, 30)
			status := pendingImageStatus()
			model.setPendingImages(status)
			model = chatEvent(t, model, conversation.TurnStarted{TurnID: "queued", IncludePending: false})
			if model.pendingImages.Images[0].State != conversation.ImagePending {
				t.Fatal("queued text claimed its predecessor's retained images")
			}
			model = chatEvent(t, model, conversation.TurnSettled{Result: conversation.TurnResult{TurnID: "queued", Images: &status}})
			model = chatEvent(t, model, conversation.ControlDone{Kind: control, Target: "beta", Err: errors.New("control failed")})
			if len(model.pendingImages.Images) != 1 {
				t.Fatal("failed control discarded valid pending input")
			}
			model = chatEvent(t, model, conversation.ControlDone{Kind: control, Target: "beta", ClearedImages: 1})
			if len(model.pendingImages.Images) != 0 {
				t.Fatal("successful control carried pending input into a new context")
			}
		})
	}
}
