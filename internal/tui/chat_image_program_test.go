package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/kjelly/hufu/internal/conversation"
)

type chatProgramOutput struct {
	ctx    context.Context
	chunks chan string
}

func (out chatProgramOutput) Write(data []byte) (int, error) {
	select {
	case out.chunks <- string(data):
		return len(data), nil
	case <-out.ctx.Done():
		return 0, out.ctx.Err()
	}
}

func TestChatImageProgramHandlesCommandsValidationAndResize(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("NO_SPINNER", "1")
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	commands := make(chan conversation.Command, 8)
	output := chatProgramOutput{ctx: ctx, chunks: make(chan string, 64)}
	empty := conversation.ImageStatus{}
	model := NewChatModel(ChatOptions{Presentation: Options{Theme: ThemeDark, NoColor: true}, PendingImages: &empty,
		TeamInfo: TeamInfo{TeamName: "images"},
		Actions: ChatActions{Submit: func(command conversation.Command) conversation.Ack {
			select {
			case commands <- command:
				return conversation.Ack{CommandID: command.ID, Status: conversation.AckAccepted}
			case <-ctx.Done():
				return conversation.Ack{CommandID: command.ID, Status: conversation.AckRejected}
			}
		}},
	})
	program := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(output), tea.WithoutSignals(), tea.WithWindowSize(100, 30))
	type completed struct {
		model tea.Model
		err   error
	}
	done := make(chan completed, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		final, err := program.Run()
		done <- completed{model: final, err: err}
	}()
	defer func() {
		// Unblock the owned output writer before stopping renderer consumers.
		cancel()
		program.Kill()
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			t.Error("chat program did not drain during cleanup")
		}
	}()
	var rendered strings.Builder
	waitText := func(want string) {
		for !strings.Contains(ansi.Strip(rendered.String()), want) {
			select {
			case chunk := <-output.chunks:
				rendered.WriteString(chunk)
			case <-ctx.Done():
				t.Fatalf("chat program did not render %q", want)
			}
		}
	}
	send := func(text string) {
		program.Send(tea.PasteMsg{Content: text})
		program.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	}
	nextCommand := func(operation conversation.ImageOperation) conversation.Command {
		select {
		case command := <-commands:
			if command.Kind != conversation.CommandImages || command.Image.Operation != operation {
				t.Fatal("image input became a model prompt")
			}
			return command
		case <-ctx.Done():
			t.Fatal("image command was not submitted")
			return conversation.Command{}
		}
	}
	waitText("Message the team")
	send("/image add /private/input.png")
	add := nextCommand(conversation.ImageAdd)
	status := pendingImageStatus()
	program.Send(ChatEventMsg{Event: conversation.ControlDone{CommandID: add.ID, Kind: add.Kind, ImageOperation: conversation.ImageAdd, Images: &status}})
	waitText("Pending images: 1; removed: 0.")
	send("/image remove 0")
	waitText("invalid image command")
	select {
	case <-commands:
		t.Fatal("invalid removal reached runtime")
	default:
	}
	send("/image list")
	list := nextCommand(conversation.ImageList)
	program.Send(ChatEventMsg{Event: conversation.ControlDone{CommandID: list.ID, Kind: list.Kind, ImageOperation: conversation.ImageList, Images: &status}})
	waitText("screen.png")
	program.Send(tea.WindowSizeMsg{Width: 80, Height: 24})
	program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	send("/image clear")
	clear := nextCommand(conversation.ImageClear)
	cleared := conversation.ImageStatus{Removed: 1}
	program.Send(ChatEventMsg{Event: conversation.ControlDone{CommandID: clear.ID, Kind: clear.Kind, ImageOperation: conversation.ImageClear, Images: &cleared}})
	waitText("removed: 1.")
	program.Quit()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		final := result.model.(ChatModel)
		if final.width != 80 || final.height != 24 || final.pendingImages == nil || len(final.pendingImages.Images) != 0 {
			t.Fatal("image command program lost resize or final cleanup")
		}
	case <-ctx.Done():
		t.Fatal("chat program did not exit")
	}
}
