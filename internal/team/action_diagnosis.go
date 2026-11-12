package team

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/utils"
)

// ActionFailureDiagnosis is provider-authored explanatory evidence, not a
// recovery decision. The runtime binds its task, attempt and receipt identity.
// CauseStatus describes the provider's claim, not independent runtime approval.
type ActionFailureDiagnosis struct {
	Version     int                    `json:"version" yaml:"version"`
	Code        string                 `json:"code" yaml:"code"`
	Summary     string                 `json:"summary" yaml:"summary"`
	CauseStatus string                 `json:"cause_status" yaml:"cause_status"`
	Cause       string                 `json:"cause" yaml:"cause"`
	Evidence    []ActionDiagnosticFact `json:"evidence,omitempty" yaml:"evidence,omitempty"`
	Suggestions []string               `json:"suggestions,omitempty" yaml:"suggestions,omitempty"`
}

type ActionDiagnosticFact struct {
	Name  string `json:"name" yaml:"name"`
	Value string `json:"value" yaml:"value"`
}

// NormalizedActionDiagnosis validates bounds before copying and redacting.
// Invalid observations are omitted; they never change the process outcome.
func NormalizedActionDiagnosis(d *ActionFailureDiagnosis) *ActionFailureDiagnosis {
	if d == nil || d.Version != 1 || !diagnosticIdentifier(d.Code) || len(d.Summary) == 0 || len(d.Summary) > 500 || len(d.Cause) == 0 || len(d.Cause) > 2000 || len(d.Evidence) > 16 || len(d.Suggestions) > 8 {
		return nil
	}
	switch d.CauseStatus {
	case "confirmed", "hypothesis", "unresolved":
	default:
		return nil
	}
	clean := func(s string) string {
		return strings.Join(strings.Fields(utils.SanitizeTerminalText(utils.RedactSecrets(s))), " ")
	}
	result := *d
	result.Summary, result.Cause = clean(d.Summary), clean(d.Cause)
	if result.Summary == "" || result.Cause == "" {
		return nil
	}
	result.Evidence = slices.Clone(d.Evidence)
	for i, fact := range result.Evidence {
		if !diagnosticIdentifier(fact.Name) || len(fact.Value) > 500 {
			return nil
		}
		if utils.IsRedactedJSONKey(fact.Name) {
			result.Evidence[i].Value = "[REDACTED]"
		} else {
			result.Evidence[i].Value = clean(fact.Value)
		}
	}
	result.Suggestions = slices.Clone(d.Suggestions)
	for i, s := range result.Suggestions {
		if len(s) > 500 {
			return nil
		}
		result.Suggestions[i] = clean(s)
	}
	return &result
}

func diagnosticIdentifier(s string) bool {
	if len(s) == 0 || len(s) > 100 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func actionDiagnosisFromError(err error) *ActionFailureDiagnosis {
	if provider, ok := errors.AsType[interface {
		error
		ActionFailureDiagnosis() *ActionFailureDiagnosis
	}](err); ok {
		return NormalizedActionDiagnosis(provider.ActionFailureDiagnosis())
	}
	return nil
}

// MarshalJSON keeps direct receipt/session/event serialization redacted too.
func (d ActionFailureDiagnosis) MarshalJSON() ([]byte, error) {
	type wire ActionFailureDiagnosis
	normalized := NormalizedActionDiagnosis(&d)
	if normalized == nil {
		return []byte("null"), nil
	}
	return json.Marshal((*wire)(normalized))
}

func renderActionDiagnosis(d *ActionFailureDiagnosis) string {
	d = NormalizedActionDiagnosis(d)
	if d == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "provider_diagnosis: %s [%s, provider-reported %s]\ncause: %s\n", d.Summary, d.Code, d.CauseStatus, d.Cause)
	for _, fact := range d.Evidence {
		fmt.Fprintf(&b, "evidence: %s=%s\n", fact.Name, fact.Value)
	}
	for _, suggestion := range d.Suggestions {
		fmt.Fprintf(&b, "provider_suggestion: %s\n", suggestion)
	}
	return b.String()
}

// Retain process diagnostics independently of the provider's concise message.
// A secondary failure must not disappear when an envelope explains the first.
func actionProcessEvidenceFromError(err error) []string {
	if process, ok := errors.AsType[*ActionCommandError](err); ok && process.ProcessDiagnostic != "" {
		return []string{process.ProcessDiagnostic}
	}
	return nil
}
func actionExitCodeFromError(err error) *int {
	if process, ok := errors.AsType[*ActionCommandError](err); ok && process.ExitCode != nil {
		return new(*process.ExitCode)
	}
	return nil
}
