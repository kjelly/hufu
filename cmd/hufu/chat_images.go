package main

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/kjelly/hufu/internal/conversation"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/team"
)

type chatImageStage func(context.Context, string, []media.Ref) (*media.StagedImage, error)

func chatHistoryAllowsLine(line string) bool {
	command, _ := splitChatCommand(line)
	return !strings.EqualFold(command, "/image")
}

type chatPendingImage struct {
	id, name string
	image    *media.StagedImage
}

// Both front ends share this process-local state. The conversation actor owns
// mutations; the mutex additionally protects commit notifications and teardown.
type chatPendingImages struct {
	mu       sync.Mutex
	images   []chatPendingImage
	nextID   uint64
	active   string
	recovery bool
}

func (p *chatPendingImages) statusLocked() conversation.ImageStatus {
	status := conversation.ImageStatus{}
	state := conversation.ImagePending
	if p.active != "" {
		state = conversation.ImageSubmitting
	}
	if p.recovery {
		state = conversation.ImageRecoveryRequired
	}
	for _, item := range p.images {
		ref := item.image.Ref()
		status.Images = append(status.Images, conversation.PendingImageInfo{ID: item.id, Name: item.name, MediaType: ref.MediaType,
			ByteSize: ref.ByteSize, Width: ref.Width, Height: ref.Height, State: state})
	}
	return status
}

func (p *chatPendingImages) status() conversation.ImageStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.statusLocked()
}

func (p *chatPendingImages) manage(ctx context.Context, command conversation.ImageCommand, stage chatImageStage) (conversation.ImageStatus, error) {
	if err := command.Validate(); err != nil {
		return conversation.ImageStatus{}, err
	}
	p.mu.Lock()
	status, released, err := p.manageLocked(ctx, command, stage)
	p.mu.Unlock()
	for _, image := range released {
		image.Close()
	}
	return status, err
}

func (p *chatPendingImages) manageLocked(ctx context.Context, command conversation.ImageCommand, stage chatImageStage) (conversation.ImageStatus, []*media.StagedImage, error) {
	if command.Operation == conversation.ImageList {
		return p.statusLocked(), nil, nil
	}
	if p.active != "" {
		return p.statusLocked(), nil, media.Reject(media.AccessDenied, "pending images are frozen for the active turn")
	}
	if p.recovery && command.Operation != conversation.ImageClear {
		return p.statusLocked(), nil, media.Reject(media.DurabilityRequired, "image submission requires recovery before reuse")
	}
	var released []*media.StagedImage
	switch command.Operation {
	case conversation.ImageAdd:
		if stage == nil {
			return p.statusLocked(), nil, media.Reject(media.Unsupported, "image staging is unavailable")
		}
		if len(p.images) >= media.MaxCount {
			return p.statusLocked(), nil, media.Reject(media.LimitExceeded, "pending image count exceeds the hard ceiling")
		}
		refs := make([]media.Ref, len(p.images))
		for index, item := range p.images {
			refs[index] = item.image.Ref()
		}
		image, err := stage(ctx, command.Path, refs)
		if err != nil {
			return p.statusLocked(), []*media.StagedImage{image}, err
		}
		if image == nil {
			return p.statusLocked(), nil, media.Reject(media.InputMissing, "staged image is unavailable")
		}
		if err := ctx.Err(); err != nil {
			return p.statusLocked(), []*media.StagedImage{image}, err
		}
		if err := media.CheckBundle(append(refs, image.Ref()), media.Limits{}); err != nil {
			return p.statusLocked(), []*media.StagedImage{image}, err
		}
		p.nextID++
		p.images = append(p.images, chatPendingImage{id: fmt.Sprintf("pending-%d", p.nextID), name: safePendingImageName(command.Path), image: image})
	case conversation.ImageRemove:
		if command.Index > len(p.images) {
			return p.statusLocked(), nil, media.Reject(media.InputMissing, "pending image index is unavailable")
		}
		released = append(released, p.images[command.Index-1].image)
		p.images = slices.Delete(p.images, command.Index-1, command.Index)
	case conversation.ImageClear:
		for _, item := range p.images {
			released = append(released, item.image)
		}
		p.images, p.recovery = nil, false
	}
	status := p.statusLocked()
	status.Removed = len(released)
	return status, released, nil
}

func safePendingImageName(path string) string {
	name := strings.ToValidUTF8(filepath.Base(path), "�")
	var display []rune
	for _, r := range name {
		if !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) {
			display = append(display, r)
		}
		if len(display) == 128 {
			break
		}
	}
	if len(display) == 0 {
		return "image"
	}
	return string(display)
}

type chatImageSubmission struct {
	owner  *chatPendingImages
	turnID string
	images []*media.StagedImage
	mu     sync.Mutex
	notice *team.MediaTurnAdmission
}

func (p *chatPendingImages) begin(turnID string) (*chatImageSubmission, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.recovery {
		return nil, media.Reject(media.DurabilityRequired, "image submission requires recovery before reuse")
	}
	if p.active != "" || strings.TrimSpace(turnID) == "" {
		return nil, media.Reject(media.AccessDenied, "another turn owns pending image admission")
	}
	if len(p.images) == 0 {
		return nil, nil
	}
	submission := &chatImageSubmission{owner: p, turnID: turnID}
	for _, item := range p.images {
		submission.images = append(submission.images, item.image)
	}
	p.active = turnID
	return submission, nil
}

func (s *chatImageSubmission) admit(notice team.MediaTurnAdmission) team.MediaTurnAdmission {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.notice != nil {
		return *s.notice
	}
	if notice.TurnID != s.turnID || notice.ImageCount != len(s.images) || notice.SnapshotID == "" ||
		(notice.State != team.MediaTurnCommitted && notice.State != team.MediaTurnNotCommitted && notice.State != team.MediaTurnRecoveryRequired) {
		notice = team.MediaTurnAdmission{State: team.MediaTurnRecoveryRequired, TurnID: s.turnID, ImageCount: len(s.images)}
	}
	s.notice = &notice
	p := s.owner
	p.mu.Lock()
	var released []*media.StagedImage
	if p.active == s.turnID {
		switch notice.State {
		case team.MediaTurnCommitted:
			for _, item := range p.images {
				released = append(released, item.image)
			}
			p.images = nil
		case team.MediaTurnRecoveryRequired:
			p.recovery = true
		}
	}
	p.mu.Unlock()
	for _, image := range released {
		image.Close()
	}
	return notice
}

func (s *chatImageSubmission) finish() *team.MediaTurnAdmission {
	s.owner.mu.Lock()
	if s.owner.active == s.turnID {
		s.owner.active = ""
	}
	s.owner.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.notice == nil {
		return &team.MediaTurnAdmission{State: team.MediaTurnNotCommitted, TurnID: s.turnID, ImageCount: len(s.images)}
	}
	copy := *s.notice
	return &copy
}

// close runs after the turn/controller drains. It also safely waits for any
// remaining staging borrower, and is idempotent for repeated shutdown paths.
func (p *chatPendingImages) close() int {
	p.mu.Lock()
	images := p.images
	p.images, p.active, p.recovery = nil, "", false
	p.mu.Unlock()
	for _, item := range images {
		item.image.Close()
	}
	return len(images)
}
