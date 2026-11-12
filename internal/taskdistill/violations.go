package taskdistill

import (
	"errors"
	"fmt"
)

var errModelContract = errors.New("model distillation contract violation")

// OutputViolation describes a rejected candidate without echoing untrusted
// response text, credentials, or unknown source values into diagnostics.
type OutputViolation struct {
	Code           string        `json:"code"`
	Path           string        `json:"path"`
	DecisionAction string        `json:"decision_action,omitempty"`
	SchemaIssues   []SchemaIssue `json:"schema_issues,omitempty"`
	cause          error
	category       error
	candidate      string
	rewrite        *knowledgeRewriteScope
}

func (v *OutputViolation) Error() string {
	message := fmt.Sprintf("%v: %s at %s", ErrInvalid, v.Code, v.Path)
	if v.DecisionAction != "" {
		message += fmt.Sprintf(" (decision action=%s)", v.DecisionAction)
	}
	for i, issue := range v.SchemaIssues {
		if i > 0 {
			message += "; at " + issue.Path
		}
		message += fmt.Sprintf(" (%s: %s)", issue.Rule, issue.Detail)
	}
	return message
}

func (v *OutputViolation) Unwrap() []error {
	causes := []error{ErrInvalid, errModelContract}
	if v.category != nil {
		causes = append(causes, v.category)
	}
	if v.cause != nil {
		causes = append(causes, v.cause)
	}
	return causes
}

func violation(code, path string, category error) error {
	return &OutputViolation{Code: code, Path: path, category: category}
}
