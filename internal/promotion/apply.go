package promotion

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/skill"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/utils"
)

type ApplyResult struct {
	Proposal       Proposal `json:"proposal"`
	AlreadyApplied bool     `json:"already_applied"`
}

func (s Service) Apply(ctx context.Context, id, project, teamID string, registry *team.TeamRegistry) (ApplyResult, error) {
	p, err := s.Get(ctx, id, project, teamID)
	if err != nil {
		return ApplyResult{}, err
	}
	if p.Status == StatusApplied {
		return ApplyResult{Proposal: p, AlreadyApplied: true}, nil
	}
	if p.Status != StatusApproved {
		return ApplyResult{}, fmt.Errorf("proposal %s is %s; approval is required before apply", id, p.Status)
	}
	if registry == nil {
		return ApplyResult{}, fmt.Errorf("team registry is required")
	}
	if err = registry.Discover(); err != nil {
		return ApplyResult{}, err
	}
	teamDir, err := registry.Resolve(p.TeamID)
	if err != nil {
		return ApplyResult{}, err
	}
	vars, err := team.ResolveTeamTemplateVars(teamDir, nil)
	if err != nil {
		return ApplyResult{}, s.applyFailed(ctx, p, err)
	}
	target, err := secureTarget(teamDir, p.TargetPath)
	if err != nil {
		return ApplyResult{}, s.applyFailed(ctx, p, err)
	}
	if err = s.validateEvidence(ctx, p); err != nil {
		stale, transitionErr := s.Repo.TransitionPromotion(ctx, p.ID, p.ProjectID, p.TeamID, StatusStale, "", lifecycleEvent("memory_promotion_stale", p, "evidence", "operator"))
		if transitionErr != nil {
			return ApplyResult{}, transitionErr
		}
		return ApplyResult{Proposal: stale}, fmt.Errorf("promotion evidence is stale; run analyze again: %w", err)
	}
	if contextstore.HashPromotionContent(p.Draft) != p.DraftHash || utils.RedactSecrets(p.Draft) != p.Draft {
		return ApplyResult{}, s.applyFailed(ctx, p, fmt.Errorf("draft hash mismatch or secret-like material"))
	}
	if err = ValidateDraft(p.Type, p.Draft, skillNameForPath(p.TargetPath), policySteps(p.Draft)); err != nil {
		return ApplyResult{}, s.applyFailed(ctx, p, err)
	}
	current, readErr := os.ReadFile(target)
	if readErr != nil && !os.IsNotExist(readErr) {
		return ApplyResult{}, s.applyFailed(ctx, p, readErr)
	}
	expected := p.Draft
	if p.Type != TypeSkill {
		expected = policyBlock(p)
	}
	if readErr == nil && alreadyWritten(p, string(current), expected) {
		p.TargetBaseHash = contextstore.HashPromotionContent(string(current))
		applied, e := s.Repo.TransitionPromotion(ctx, p.ID, p.ProjectID, p.TeamID, StatusApplied, "", lifecycleEvent("memory_promotion_applied", p, p.DraftHash, "operator"))
		return ApplyResult{Proposal: applied, AlreadyApplied: true}, e
	}
	currentHash := ""
	if readErr == nil {
		currentHash = contextstore.HashPromotionContent(string(current))
	}
	if currentHash != p.TargetBaseHash {
		stale, e := s.Repo.TransitionPromotion(ctx, p.ID, p.ProjectID, p.TeamID, StatusStale, "", lifecycleEvent("memory_promotion_stale", p, "target", "operator"))
		if e != nil {
			return ApplyResult{}, e
		}
		return ApplyResult{Proposal: stale}, fmt.Errorf("promotion target changed; run analyze again")
	}
	var next []byte
	var mode os.FileMode = 0o644
	var before agentIdentity
	if p.Type == TypeSkill {
		if readErr == nil {
			return ApplyResult{}, s.applyFailed(ctx, p, fmt.Errorf("skill target already exists"))
		}
		next = []byte(p.Draft)
	} else {
		if readErr != nil {
			return ApplyResult{}, s.applyFailed(ctx, p, fmt.Errorf("policy target does not exist"))
		}
		info, e := os.Stat(target)
		if e != nil {
			return ApplyResult{}, s.applyFailed(ctx, p, e)
		}
		mode = info.Mode().Perm()
		sep := "\n\n"
		if strings.HasSuffix(string(current), "\n") {
			sep = "\n"
		}
		next = []byte(string(current) + sep + expected + "\n")
	}
	if p.Type != TypeSkill {
		def, e := team.ValidateAgentFileWithVars(target, vars)
		if e != nil || def == nil {
			return ApplyResult{}, s.applyFailed(ctx, p, fmt.Errorf("invalid policy target before apply"))
		}
		before = agentIdentity{Name: def.Name, Role: def.Role}
		combined, e := team.ValidateAgentContentWithVars(next, target, vars)
		if e != nil || combined == nil || combined.Name != before.Name || combined.Role != before.Role {
			return ApplyResult{}, s.applyFailed(ctx, p, fmt.Errorf("policy draft produces an invalid agent file"))
		}
	}
	if p.Type == TypeSkill {
		err = team.AtomicCreateFile(target, next, mode)
	} else {
		err = team.AtomicWriteFile(target, next, mode)
	}
	if err != nil {
		return ApplyResult{}, s.applyFailed(ctx, p, err)
	}
	if err = validateWrittenTarget(p, target, teamDir, current, mode, before, vars); err != nil {
		return ApplyResult{}, s.applyFailed(ctx, p, err)
	}
	p.TargetBaseHash = contextstore.HashPromotionContent(string(next))
	applied, err := s.Repo.TransitionPromotion(ctx, p.ID, p.ProjectID, p.TeamID, StatusApplied, "", lifecycleEvent("memory_promotion_applied", p, p.DraftHash, "operator"))
	if err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Proposal: applied}, nil
}

func (s Service) validateEvidence(ctx context.Context, p Proposal) error {
	for _, snap := range p.Sources {
		item, err := s.Repo.Get(ctx, snap.ContextItemID)
		if err != nil {
			return err
		}
		if item.Scope.ProjectID != p.ProjectID || item.Scope.TeamID != p.TeamID || item.Lifecycle != contextstore.LifecycleConfirmed || item.SupersededBy != "" || item.ContentHash != snap.ContentHash {
			return fmt.Errorf("source %s changed", snap.ContextItemID)
		}
		if item.Metadata == nil || (item.Metadata["memory_lifetime"] != "persistent" && item.Metadata["memory_tier"] != "persistent") {
			return fmt.Errorf("source %s is no longer persistent", snap.ContextItemID)
		}
		if item.ExpiresAt != nil && !item.ExpiresAt.After(time.Now().UTC()) {
			return fmt.Errorf("source %s expired", snap.ContextItemID)
		}
		agg, err := s.Repo.ExperienceAggregate(ctx, snap.ContextItemID, p.PolicyVersion)
		if err != nil || agg.Revision != snap.AggregateRevision {
			return fmt.Errorf("source %s aggregate changed", snap.ContextItemID)
		}
	}
	return nil
}

func secureTarget(teamDir, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("promotion target must be relative")
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("promotion target escapes team directory")
	}
	realTeam, err := filepath.EvalSymlinks(teamDir)
	if err != nil {
		return "", err
	}
	target := filepath.Join(realTeam, clean)
	probe := target
	for {
		_, err = os.Lstat(probe)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", fmt.Errorf("cannot resolve target parent")
		}
		probe = parent
	}
	realProbe, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(realTeam, realProbe)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("promotion target resolves outside team directory")
	}
	if _, err = os.Lstat(target); err == nil {
		realTarget, e := filepath.EvalSymlinks(target)
		if e != nil {
			return "", e
		}
		relative, e = filepath.Rel(realTeam, realTarget)
		if e != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("promotion target symlink escapes team directory")
		}
		target = realTarget
	}
	return target, nil
}
func policyBlock(p Proposal) string {
	return fmt.Sprintf("<!-- hufu-promotion:%s:start -->\n%s\n<!-- hufu-promotion:%s:end -->", p.ID, strings.TrimSpace(p.Draft), p.ID)
}
func alreadyWritten(p Proposal, current, expected string) bool {
	if p.Type == TypeSkill {
		return current == p.Draft
	}
	return strings.Contains(current, expected)
}
func validateWrittenTarget(p Proposal, target, teamDir string, original []byte, mode os.FileMode, before agentIdentity, vars map[string]string) error {
	if p.Type == TypeSkill {
		def, err := skill.ValidateSkillDraft([]byte(p.Draft))
		if err != nil {
			return rollbackTarget(target, original, mode, err)
		}
		found := skill.DiscoverSkills([]string{filepath.Join(teamDir, "skills")}, false)
		for _, v := range found {
			if v.Name == def.Name && filepath.Clean(v.Path) == filepath.Clean(target) {
				return nil
			}
		}
		return rollbackTarget(target, original, mode, fmt.Errorf("written skill is not discoverable"))
	}
	after, err := team.ValidateAgentFileWithVars(target, vars)
	if err != nil || after == nil {
		return rollbackTarget(target, original, mode, fmt.Errorf("written policy target is invalid"))
	}
	if before.Name != after.Name || before.Role != after.Role {
		return rollbackTarget(target, original, mode, fmt.Errorf("policy apply changed agent identity"))
	}
	return nil
}

type agentIdentity struct{ Name, Role string }

func rollbackTarget(path string, original []byte, mode os.FileMode, cause error) error {
	var err error
	if original == nil {
		err = os.Remove(path)
	} else {
		err = team.AtomicWriteFile(path, original, mode)
	}
	if err != nil {
		return fmt.Errorf("%v; rollback failed: %w", cause, err)
	}
	return cause
}
func (s Service) applyFailed(ctx context.Context, p Proposal, cause error) error {
	event := lifecycleEvent("memory_promotion_apply_failed", p, contextstore.HashPromotionContent(cause.Error()), "operator")
	if err := s.Repo.EnqueuePromotionEvent(ctx, event); err != nil {
		return fmt.Errorf("%v; record apply failure: %w", cause, err)
	}
	return cause
}
