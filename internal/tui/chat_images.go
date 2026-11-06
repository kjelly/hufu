package tui

import (
	"fmt"
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/kjelly/hufu/internal/conversation"
	"github.com/kjelly/hufu/internal/team"
)

func (m *ChatModel) setPendingImages(status conversation.ImageStatus) {
	copy := status
	copy.Images = slices.Clone(status.Images)
	m.pendingImages = &copy
}

func (m *ChatModel) markPendingImagesSubmitting(include bool) {
	if !include || m.pendingImages == nil {
		return
	}
	status := *m.pendingImages
	status.Images = slices.Clone(status.Images)
	for index := range status.Images {
		if status.Images[index].State == conversation.ImagePending {
			status.Images[index].State = conversation.ImageSubmitting
		}
	}
	m.setPendingImages(status)
}

func (m ChatModel) imageCommand(argument string) (ChatModel, tea.Cmd) {
	command, err := conversation.ParseImageCommand(argument)
	if err != nil {
		m.notice = err.Error()
		return m, nil
	}
	var id string
	m, id = m.nextCommandID()
	return m, m.submitCmd(conversation.Command{ID: id, Kind: conversation.CommandImages, Image: command}, "")
}

func (m *ChatModel) clearPendingImagesAfterControl(count int) {
	if m.pendingImages != nil || count > 0 {
		m.setPendingImages(conversation.ImageStatus{})
	}
	if count > 0 {
		m.transcript.add(chatItem{kind: itemNotice, body: fmt.Sprintf("Removed %d pending images.", count)})
	}
}

func (m ChatModel) handleImageAdmission(event conversation.TurnInputAdmission) ChatModel {
	if m.stages[event.TurnID] >= turnSettled || m.lastImageAdmissionTurn == event.TurnID {
		return m
	}
	m.lastImageAdmissionTurn = event.TurnID
	notice := event.Admission
	text := "Image admission requires recovery; pending images will not be reused automatically."
	switch notice.State {
	case team.MediaTurnCommitted:
		m.setPendingImages(conversation.ImageStatus{})
		text = fmt.Sprintf("Committed %d images to this turn.", notice.ImageCount)
	case team.MediaTurnNotCommitted:
		text = fmt.Sprintf("Retained %d uncommitted images for an explicit retry.", notice.ImageCount)
	case team.MediaTurnRecoveryRequired:
		if m.pendingImages != nil {
			status := *m.pendingImages
			status.Images = slices.Clone(status.Images)
			for index := range status.Images {
				status.Images[index].State = conversation.ImageRecoveryRequired
			}
			m.setPendingImages(status)
		}
	}
	m.transcript.add(chatItem{kind: itemNotice, turnID: event.TurnID, body: text})
	return m
}

func (m ChatModel) imageStatusText() string {
	text := fmt.Sprintf("images:%d", len(m.pendingImages.Images))
	for _, item := range m.pendingImages.Images {
		if item.State == conversation.ImageRecoveryRequired {
			return text + " recovery"
		}
		if item.State == conversation.ImageSubmitting {
			return text + " submitting"
		}
	}
	return text
}
