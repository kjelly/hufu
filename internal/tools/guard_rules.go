package tools

import (
	"fmt"
	"regexp"
	"strings"
)

// deterministicGuardRulePrefix identifies guard rules that are enforced
// locally, before any model-based guard reviewer.  The rule language is
// intentionally small and generic so teams can protect tool inputs without
// coupling Hufu to a particular application or command.
const deterministicGuardRulePrefix = "deny_tool_input_regex:"

// EvaluateDeterministicGuardRules evaluates the machine-readable subset of
// AgentDef.Guard rules. It returns a denial reason, if any. Invalid rules are
// errors and must fail closed; silently ignoring a malformed safety rule would
// turn a configuration typo into an authorization bypass.
func EvaluateDeterministicGuardRules(toolName, input string, rules []string) (string, error) {
	for _, raw := range rules {
		rule := strings.TrimSpace(raw)
		if rule == "" || !strings.HasPrefix(rule, deterministicGuardRulePrefix) {
			continue
		}
		pattern := strings.TrimSpace(strings.TrimPrefix(rule, deterministicGuardRulePrefix))
		if pattern == "" {
			return "", fmt.Errorf("empty deterministic guard rule for tool %q", toolName)
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return "", fmt.Errorf("invalid deterministic guard rule for tool %q: %w", toolName, err)
		}
		if re.MatchString(input) {
			return fmt.Sprintf("tool %q input matched deterministic guard rule", toolName), nil
		}
	}
	return "", nil
}
