package main

import (
	"context"
	"errors"
	"sync"

	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/team"
)

type chatMediaCoordinator interface {
	RunWithMedia(context.Context, string, team.MediaTurnRequest) (string, error)
	ContinueWithMedia(context.Context, string, team.MediaTurnRequest) (string, error)
}

func executeChatTurn(ctx context.Context, deps chatTurnDeps, seq *chatTurnSequencer, turnID, prompt string) (answer string, admission *team.MediaTurnAdmission, err error) {
	var submission *chatImageSubmission
	if !deps.skipPending && seq.images != nil {
		submission, err = seq.images.begin(turnID)
		if err != nil {
			notice := &team.MediaTurnAdmission{State: team.MediaTurnNotCommitted, TurnID: turnID, ImageCount: len(seq.images.status().Images)}
			if classified, ok := errors.AsType[*media.Error](err); ok && classified.Code == media.DurabilityRequired {
				notice.State = team.MediaTurnRecoveryRequired
			}
			return "", notice, err
		}
	}
	if submission == nil {
		if seq.first() {
			answer, err = deps.coordinator.Run(ctx, prompt)
		} else {
			answer, err = deps.coordinator.ContinueWithPrompt(ctx, prompt)
		}
		return answer, nil, err
	}
	defer func() {
		admission = submission.finish()
		if admission.State == team.MediaTurnRecoveryRequired || err == nil && admission.State != team.MediaTurnCommitted {
			err = errors.Join(err, media.Reject(media.DurabilityRequired, "image turn has no confirmed canonical admission"))
		}
	}()
	coordinator, ok := deps.coordinator.(chatMediaCoordinator)
	if !ok {
		return "", nil, media.Reject(media.Unsupported, "image turns are unavailable in this session")
	}
	var notified sync.Once
	request := team.MediaTurnRequest{TurnID: turnID, StagedImages: submission.images, OnAdmission: func(notice team.MediaTurnAdmission) {
		notified.Do(func() {
			accepted := submission.admit(notice)
			if deps.onInputAdmission != nil {
				deps.onInputAdmission(accepted)
			}
		})
	}}
	if seq.first() {
		answer, err = coordinator.RunWithMedia(ctx, prompt, request)
	} else {
		answer, err = coordinator.ContinueWithMedia(ctx, prompt, request)
	}
	return answer, nil, err
}
