package promotion

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kjelly/hufu/internal/skill"
	"github.com/kjelly/hufu/internal/utils"
)

var (
	skillNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	draftStepRE = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+\S`)
)

// DraftSteps returns the list-item lines of a skill draft body. The YAML
// frontmatter is excluded, so frontmatter lists never count as steps, and an
// unparsable draft has no steps. Policies have no step requirement and
// return nil. Every promotion stage (analyze, edit, apply, improve handoff
// and experiments) counts steps with this one rule.
func DraftSteps(typ Type, draft string) []string {
	if typ != TypeSkill {
		return nil
	}
	def, err := skill.ValidateSkillDraft([]byte(draft))
	if err != nil {
		return nil
	}
	var steps []string
	for _, line := range strings.Split(def.Content, "\n") {
		if draftStepRE.MatchString(line) {
			steps = append(steps, strings.TrimSpace(line))
		}
	}
	return steps
}

// ValidateDraft validates a promotion draft. Skills must be a complete
// SKILL.md whose body has at least two list-item steps (DraftSteps).
func ValidateDraft(typ Type, draft, skillName string) error {
	if strings.TrimSpace(draft) == "" {
		return fmt.Errorf("promotion draft is empty")
	}
	if utils.RedactSecrets(draft) != draft || strings.Contains(draft, "[REDACTED]") || strings.Contains(draft, "<REDACTED:") {
		return fmt.Errorf("promotion draft contains secret-like material")
	}
	switch typ {
	case TypeSkill:
		def, err := skill.ValidateSkillDraft([]byte(draft))
		if err != nil {
			return err
		}
		if len(DraftSteps(typ, draft)) < 2 {
			return fmt.Errorf("skill proposal requires at least two verifiable steps in the draft body")
		}
		if skillName == "" {
			skillName = def.Name
		}
		if def.Name != skillName {
			return fmt.Errorf("skill_name %q does not match frontmatter name %q", skillName, def.Name)
		}
		if !skillNameRE.MatchString(skillName) {
			return fmt.Errorf("invalid skill name %q", skillName)
		}
	case TypeTeamPolicy, TypeAgentPolicy:
		if strings.HasPrefix(strings.TrimSpace(draft), "---") {
			return fmt.Errorf("policy draft must not contain YAML frontmatter")
		}
		if strings.Contains(draft, "<!-- hufu-promotion:") {
			return fmt.Errorf("policy draft must not contain promotion markers")
		}
	default:
		return fmt.Errorf("unknown promotion type %q", typ)
	}
	return nil
}

func TargetPathForSkill(name string) string {
	return filepath.ToSlash(filepath.Join("skills", name, "SKILL.md"))
}
