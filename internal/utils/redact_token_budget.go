package utils

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

var scopedTokenBudgetKey = regexp.MustCompile(`^max_([a-z][a-z0-9]*(?:_[a-z][a-z0-9]*)*)_(?:input|output)_tokens$`)
var sensitiveBudgetScope = regexp.MustCompile(`auth|access|refresh|bearer|api|key|secret|passwd|password|credential|token`)

func isPublicTokenBudgetKey(key string) bool {
	if len(key) > 96 {
		return false
	}
	match := scopedTokenBudgetKey.FindStringSubmatch(strings.ToLower(key))
	return len(match) == 2 && !sensitiveBudgetScope.MatchString(match[1])
}

// IsPublicTokenBudget recognizes bounded, nonnegative integer counts under a
// scoped max_<scope>_input_tokens or max_<scope>_output_tokens metadata key.
// Credential-like scopes and every noninteger JSON type remain sensitive.
// The unscoped max_output_tokens telemetry DTO keeps its existing contract.
func IsPublicTokenBudget(key string, value any) bool {
	number, ok := value.(json.Number)
	if !ok || !isPublicTokenBudgetKey(key) {
		return false
	}
	count, err := strconv.ParseUint(number.String(), 10, 31)
	return err == nil && count <= 1<<31-1
}
