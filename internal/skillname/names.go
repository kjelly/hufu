// Package skillname owns naming rules shared by skill authoring and loading.
package skillname

import (
	"fmt"
	"strings"
)

// reservedNames are runtime-owned protocol and control tool names. Skills must
// not shadow them: their behavior is defined by the runtime contract, not by
// model-authored instructions.
var reservedNames = map[string]struct{}{
	"agent":          {},
	"approve-plan":   {},
	"ask-user":       {},
	"context-get":    {},
	"context-query":  {},
	"create-skill":   {},
	"finish":         {},
	"load-skill":     {},
	"ltm-update":     {},
	"memory-query":   {},
	"memory-save":    {},
	"modify-plan":    {},
	"reconcile-task": {},
	"reject-plan":    {},
	"request-agent":  {},
	"save-skill":     {},
	"stm-write":      {},
	"submit-plan":    {},
	"submit-result":  {},
	"team-info":      {},
	"todo":           {},
}

// canonical normalizes separator variants accepted by older authoring paths so
// submit_result and submit-result share one identity.
func canonical(name string) string {
	var normalized strings.Builder
	separator := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if r == '-' || r == '_' {
			if !separator && normalized.Len() > 0 {
				normalized.WriteByte('-')
			}
			separator = true
			continue
		}
		normalized.WriteRune(r)
		separator = false
	}
	return strings.Trim(normalized.String(), "-")
}

// IsReserved reports whether name belongs to the runtime protocol or control
// namespace.
func IsReserved(name string) bool {
	_, reserved := reservedNames[canonical(name)]
	return reserved
}

// Validate rejects names that would shadow runtime-owned behavior.
func Validate(name string) error {
	if IsReserved(name) {
		return fmt.Errorf("skill name %q is reserved for runtime control", name)
	}
	return nil
}
