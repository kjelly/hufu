package conversation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

func TestParseImageCommandValidatesExplicitOperations(t *testing.T) {
	for _, test := range []struct {
		input string
		want  ImageCommand
		valid bool
	}{
		{"", ImageCommand{Operation: ImageList}, true},
		{"list", ImageCommand{Operation: ImageList}, true},
		{"clear", ImageCommand{Operation: ImageClear}, true},
		{"remove 2", ImageCommand{Operation: ImageRemove, Index: 2}, true},
		{"add /private/image with spaces.png", ImageCommand{Operation: ImageAdd, Path: "/private/image with spaces.png"}, true},
		{`add "/private/image with spaces.png"`, ImageCommand{Operation: ImageAdd, Path: "/private/image with spaces.png"}, true},
		{"add '/private/image with spaces.png'", ImageCommand{Operation: ImageAdd, Path: "/private/image with spaces.png"}, true},
		{"remove 0", ImageCommand{}, false},
		{"remove -1", ImageCommand{}, false},
		{"list /private/image.png", ImageCommand{}, false},
		{"clear /private/image.png", ImageCommand{}, false},
		{`add "/private/unclosed`, ImageCommand{}, false},
		{"add", ImageCommand{}, false},
		{"add /private/\x00image.png", ImageCommand{}, false},
		{"add /private/" + strings.Repeat("x", 4096), ImageCommand{}, false},
		{"ADD /private/image.png", ImageCommand{}, false},
	} {
		t.Run(test.input, func(t *testing.T) {
			got, err := ParseImageCommand(test.input)
			if !test.valid {
				if err == nil || strings.Contains(err.Error(), "/private") {
					t.Fatal("invalid command was accepted or exposed its source path", err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatal("image command changed operation, index or literal path", got, err)
			}
		})
	}
}

type imagePort struct {
	*fakePort
	inputs        chan TurnRequest
	manageStarted chan struct{}
	allowManage   chan struct{}
	imageCount    int
}

func (p *imagePort) RunInputTurn(ctx context.Context, request TurnRequest, _ func(team.MediaTurnAdmission)) TurnResult {
	select {
	case p.inputs <- request:
	case <-ctx.Done():
		return TurnResult{Err: ctx.Err(), Cancelled: true}
	}
	return p.RunTurn(ctx, request.TurnID, request.Prompt)
}

func (p *imagePort) PendingImageCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.imageCount
}

func (p *imagePort) ManageImageInputs(ctx context.Context, command ImageCommand) (ImageStatus, error) {
	if p.manageStarted != nil {
		close(p.manageStarted)
		select {
		case <-p.allowManage:
		case <-ctx.Done():
			return ImageStatus{}, ctx.Err()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if command.Operation == ImageAdd {
		p.imageCount++
	}
	return ImageStatus{Images: []PendingImageInfo{{ID: "pending", Name: "image.png", State: ImagePending}}}, nil
}

func TestControllerImagesBindPendingToFirstPromptAfterControl(t *testing.T) {
	port := &imagePort{fakePort: newFakePort(true), inputs: make(chan TurnRequest, 4), manageStarted: make(chan struct{}), allowManage: make(chan struct{})}
	controller := New(port, Config{})
	ctx, cancel := context.WithCancel(t.Context())
	controller.Start(ctx)
	t.Cleanup(func() { controller.Close(time.Second); cancel() })
	add := Command{ID: "add", Kind: CommandImages, Image: ImageCommand{Operation: ImageAdd, Path: "/private/image.png"}}
	if ack := controller.Submit(add); ack.Status != AckAccepted {
		t.Fatal("image control rejected", ack)
	}
	select {
	case <-port.manageStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("image control did not start")
	}
	first := controller.Submit(Command{ID: "first", Kind: CommandSubmitPrompt, Text: "first"})
	second := controller.Submit(Command{ID: "second", Kind: CommandSubmitPrompt, Text: "second"})
	if first.Status != AckQueued || second.Status != AckQueued {
		t.Fatal("prompts did not wait behind image staging", first, second)
	}
	if ack := controller.Submit(Command{ID: "clear", Kind: CommandImages, Image: ImageCommand{Operation: ImageClear}}); ack.Status != AckRejected {
		t.Fatal("pending mutation raced queued input", ack)
	}
	close(port.allowManage)
	events := collect(t, controller, 1)
	control, ok := events[0].(ControlDone)
	if !ok || control.Err != nil || control.Images == nil || len(control.Images.Images) != 1 {
		t.Fatal("image control result was lost", events)
	}
	readInput := func() TurnRequest {
		select {
		case request := <-port.inputs:
			return request
		case <-time.After(2 * time.Second):
			t.Fatal("input turn did not start")
			return TurnRequest{}
		}
	}
	if request := readInput(); request.TurnID != first.TurnID || !request.IncludePending {
		t.Fatal("first prompt did not own images staged by its preceding control", request)
	}
	if ack := controller.Submit(Command{ID: "cancel", Kind: CommandCancelTurn}); ack.Status != AckAccepted {
		t.Fatal(ack)
	}
	waitSettlement := func(turnID string) {
		deadline, stop := context.WithTimeout(t.Context(), 3*time.Second)
		defer stop()
		for {
			select {
			case event, open := <-controller.Events():
				if !open {
					t.Fatal("conversation closed before settlement")
				}
				if settled, ok := event.(TurnSettled); ok && settled.Result.TurnID == turnID {
					return
				}
			case <-deadline.Done():
				t.Fatal("conversation turn did not settle")
			}
		}
	}
	waitSettlement(first.TurnID)
	if request := readInput(); request.TurnID != second.TurnID || request.IncludePending {
		t.Fatal("queued prompt inherited its predecessor's retained inputs", request)
	}
	port.release <- struct{}{}
	waitSettlement(second.TurnID)
	third := controller.Submit(Command{ID: "third", Kind: CommandSubmitPrompt, Text: "explicit retry"})
	if third.Status != AckAccepted {
		t.Fatal("settled controller did not admit an explicit retry", third)
	}
	if request := readInput(); request.TurnID != third.TurnID || !request.IncludePending {
		t.Fatal("explicit idle submission could not select retained pending input", request)
	}
	port.release <- struct{}{}
	if ack := controller.Submit(add); ack != (Ack{CommandID: "add", Status: AckAccepted}) {
		t.Fatal("duplicate command changed its original acknowledgment", ack)
	}
	if port.PendingImageCount() != 1 {
		t.Fatal("duplicate image command repeated staging")
	}
}
