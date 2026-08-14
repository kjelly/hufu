package promotion

import (
	"context"
	"fmt"
	"os"
	"strings"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/utils"
)

type Service struct{ Repo Repository }

func (s Service) Get(ctx context.Context, id, project, team string) (Proposal, error) {
	return s.Repo.GetPromotion(ctx, id, project, team)
}
func (s Service) List(ctx context.Context, project, team string) ([]Proposal, error) {
	return s.Repo.ListPromotions(ctx, project, team)
}
func (s Service) Edit(ctx context.Context, id, project, team, file string) (Proposal, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return Proposal{}, err
	}
	p, err := s.Get(ctx, id, project, team)
	if err != nil {
		return p, err
	}
	if err = ValidateDraft(p.Type, string(b), skillNameForPath(p.TargetPath), policySteps(string(b))); err != nil {
		return p, err
	}
	hash := contextstore.HashPromotionContent(string(b))
	if p.DraftHash == hash && p.Draft == string(b) {
		return p, nil
	}
	p.DraftHash = hash
	revision := fmt.Sprintf("%d:%s", p.UpdatedAt.UnixNano(), hash)
	return s.Repo.UpdatePromotionDraft(ctx, id, project, team, string(b), hash, lifecycleEvent("memory_promotion_edited", p, revision, "operator"))
}
func (s Service) Approve(ctx context.Context, id, project, team string) (Proposal, error) {
	p, err := s.Get(ctx, id, project, team)
	if err != nil {
		return p, err
	}
	return s.Repo.TransitionPromotion(ctx, id, project, team, StatusApproved, "", lifecycleEvent("memory_promotion_approved", p, "approve", "operator"))
}
func (s Service) Reject(ctx context.Context, id, project, team, reason string) (Proposal, error) {
	if reason == "" {
		return Proposal{}, fmt.Errorf("rejection reason is required")
	}
	p, err := s.Get(ctx, id, project, team)
	if err != nil {
		return p, err
	}
	reason = utils.RedactSecrets(reason)
	return s.Repo.TransitionPromotion(ctx, id, project, team, StatusRejected, reason, lifecycleEvent("memory_promotion_rejected", p, "reject", "operator"))
}

func skillNameForPath(path string) string {
	base := path
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	if len(base) >= 9 && base[len(base)-9:] == "/SKILL.md" {
		base = base[:len(base)-9]
	}
	for i := len(base) - 1; i >= 0; i-- {
		if base[i] == '/' {
			return base[i+1:]
		}
	}
	return base
}

// Edited skill drafts no longer carry generator metadata, so require two list steps in the body.
func policySteps(draft string) []string {
	var steps []string
	for _, line := range strings.Split(draft, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "1. ") || strings.HasPrefix(line, "2. ") {
			steps = append(steps, line)
		}
	}
	return steps
}
