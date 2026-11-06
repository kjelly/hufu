package conversation

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/team"
)

type ImageOperation string

const (
	ImageAdd    ImageOperation = "add"
	ImageList   ImageOperation = "list"
	ImageRemove ImageOperation = "remove"
	ImageClear  ImageOperation = "clear"
)

type ImageCommand struct {
	Operation ImageOperation
	Path      string
	Index     int // 1-based, remove only
}

func (cmd ImageCommand) Validate() error {
	switch cmd.Operation {
	case ImageAdd:
		if cmd.Path != "" && len(cmd.Path) <= 4096 && utf8.ValidString(cmd.Path) && !strings.ContainsRune(cmd.Path, 0) && cmd.Index == 0 {
			return nil
		}
	case ImageRemove:
		if cmd.Index > 0 && cmd.Path == "" {
			return nil
		}
	case ImageList, ImageClear:
		if cmd.Path == "" && cmd.Index == 0 {
			return nil
		}
	}
	return errors.New("invalid image command; use add <path>, list, remove <index>, or clear")
}

// ParseImageCommand parses explicit slash-command syntax, never task prose.
// The remainder of add is one path, without shell expansion or evaluation.
func ParseImageCommand(args string) (ImageCommand, error) {
	args = strings.TrimSpace(args)
	if args == "" {
		return ImageCommand{Operation: ImageList}, nil
	}
	operation := strings.Fields(args)[0]
	argument := strings.TrimSpace(strings.TrimPrefix(args, operation))
	cmd := ImageCommand{Operation: ImageOperation(operation)}
	switch cmd.Operation {
	case ImageAdd:
		if strings.HasPrefix(argument, "\"") {
			value, err := strconv.Unquote(argument)
			if err != nil {
				return ImageCommand{}, errors.New("invalid quoted image path")
			}
			argument = value
		} else if strings.HasPrefix(argument, "'") {
			if len(argument) < 2 || !strings.HasSuffix(argument, "'") {
				return ImageCommand{}, errors.New("invalid quoted image path")
			}
			argument = argument[1 : len(argument)-1]
		}
		cmd.Path = argument
	case ImageRemove:
		index, err := strconv.Atoi(argument)
		if err != nil {
			return ImageCommand{}, errors.New("image removal requires a positive 1-based index")
		}
		cmd.Index = index
	default:
		if argument != "" {
			return ImageCommand{}, errors.New("this image command takes no argument")
		}
	}
	return cmd, cmd.Validate()
}

type PendingImageState string

const (
	ImagePending          PendingImageState = "pending"
	ImageSubmitting       PendingImageState = "submitting"
	ImageRecoveryRequired PendingImageState = "recovery_required"
)

// PendingImageInfo is safe presentation data; it has no original path or bytes.
type PendingImageInfo struct {
	ID, Name, MediaType string
	ByteSize            int64
	Width, Height       int
	State               PendingImageState
}

type ImageStatus struct {
	Images  []PendingImageInfo
	Removed int
}

func FormatImageStatus(status ImageStatus) string {
	var out strings.Builder
	if status.Removed > 0 {
		fmt.Fprintf(&out, "Removed %d pending images.\n", status.Removed)
	}
	if len(status.Images) == 0 {
		out.WriteString("No pending images.")
		return out.String()
	}
	for index, info := range status.Images {
		state := "unknown"
		switch info.State {
		case ImagePending, ImageSubmitting, ImageRecoveryRequired:
			state = string(info.State)
		}
		fmt.Fprintf(&out, "%d. %q [%q] %q %d×%d, %d bytes, %s\n", index+1, info.Name, info.ID, info.MediaType, info.Width, info.Height, info.ByteSize, state)
	}
	return strings.TrimSuffix(out.String(), "\n")
}

// ImageRuntimePort is optional so existing text-only runtimes stay compatible.
type ImageRuntimePort interface {
	InputRuntimePort
	ManageImageInputs(context.Context, ImageCommand) (ImageStatus, error)
	PendingImageCount() int
}

type TurnRequest struct {
	TurnID, Prompt string
	// IncludePending is fixed at command admission. A prompt queued behind an
	// earlier prompt cannot adopt that prompt's retained, uncommitted images.
	IncludePending bool
}

type InputRuntimePort interface {
	RunInputTurn(context.Context, TurnRequest, func(team.MediaTurnAdmission)) TurnResult
}

type TurnInputAdmission struct {
	TurnID    string
	Admission team.MediaTurnAdmission
}

func (TurnInputAdmission) isEvent() {}

func (c *Controller) inputObserver(turnID string) func(team.MediaTurnAdmission) {
	return func(admission team.MediaTurnAdmission) {
		c.emit(TurnInputAdmission{TurnID: turnID, Admission: admission})
	}
}

func (c *Controller) pendingImageCount() int {
	if port, ok := c.port.(ImageRuntimePort); ok {
		return port.PendingImageCount()
	}
	return 0
}
