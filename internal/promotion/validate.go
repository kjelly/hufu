package promotion

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kjelly/hufu/internal/skill"
	"github.com/kjelly/hufu/internal/utils"
)

var skillNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func ValidateDraft(typ Type, draft, skillName string, steps []string) error {
	if strings.TrimSpace(draft) == "" {
		return fmt.Errorf("promotion draft is empty")
	}
	if utils.RedactSecrets(draft) != draft || strings.Contains(draft, "[REDACTED]") || strings.Contains(draft, "<REDACTED:") {
		return fmt.Errorf("promotion draft contains secret-like material")
	}
	switch typ {
	case TypeSkill:
		if len(steps) < 2 {
			return fmt.Errorf("skill proposal requires at least two verifiable steps")
		}
		def, err := skill.ValidateSkillDraft([]byte(draft))
		if err != nil {
			return err
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
