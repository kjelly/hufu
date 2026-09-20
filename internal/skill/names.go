package skill

import "github.com/kjelly/hufu/internal/skillname"

// IsReservedSkillName reports whether name belongs to the runtime protocol or
// control namespace.
func IsReservedSkillName(name string) bool {
	return skillname.IsReserved(name)
}

// ValidateSkillName rejects names that would shadow runtime-owned behavior.
func ValidateSkillName(name string) error {
	return skillname.Validate(name)
}
