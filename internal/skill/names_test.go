package skill

import "testing"

func TestReservedSkillNamesNormalizeRuntimeToolSeparators(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"submit_result",
		"submit-result",
		"Submit__Result",
		" finish ",
		"save_skill",
		"request-agent",
	} {
		if !IsReservedSkillName(name) {
			t.Errorf("IsReservedSkillName(%q) = false, want true", name)
		}
		if err := ValidateSkillName(name); err == nil {
			t.Errorf("ValidateSkillName(%q) succeeded, want reserved-name error", name)
		}
	}
}

func TestValidateSkillNameAllowsDomainSkills(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"code-review", "deploy-service", "submit-review-result"} {
		if err := ValidateSkillName(name); err != nil {
			t.Errorf("ValidateSkillName(%q) error = %v", name, err)
		}
	}
}

func TestValidateSkillDraftRejectsRuntimeControlName(t *testing.T) {
	t.Parallel()

	raw := []byte("---\nname: submit_result\ndescription: typed task result\n---\n\n# Submit result\n")
	if _, err := ValidateSkillDraft(raw); err == nil {
		t.Fatal("ValidateSkillDraft() succeeded for runtime-owned submit_result")
	}
}
