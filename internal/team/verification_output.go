package team

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/utils"
)

var errVerificationAssertionPath = errors.New("could not resolve its configured path")

const (
	verificationEvidenceBytes     = 2000
	verificationRedactionPrefix   = 2 * verificationEvidenceBytes
	executableNotFoundDiagnostic  = "executable file not found"
	verificationDiagnosticOverlap = len(executableNotFoundDiagnostic) - 1
)

// Command evidence retains a bounded prefix and checks the entire stream for
// executable and weak verifier diagnostics, including split pipe writes.
type verificationOutput struct {
	prefix                bytes.Buffer
	limit                 int
	truncated             bool
	environmentFailure    bool
	weakWarning           bool
	diagnosticTail        string
	lineStartedBeforeTail bool
	grepCount             *verificationZeroOutput
}

func (w *verificationOutput) Write(p []byte) (int, error) {
	retained := min(len(p), w.limit-w.prefix.Len())
	_, _ = w.prefix.Write(p[:retained])
	w.truncated = w.truncated || retained < len(p)
	if w.grepCount != nil {
		w.grepCount.observe(p)
	}
	if !w.environmentFailure || !w.weakWarning {
		diagnostic := w.diagnosticTail + string(p)
		if !w.environmentFailure {
			w.environmentFailure = hasEnvironmentFailureSignal(diagnostic)
		}
		if !w.weakWarning {
			lineView := diagnostic
			if w.lineStartedBeforeTail {
				// An "error:" within a line is not the same as its prefix.
				lineView = "_" + lineView
			}
			w.weakWarning, _ = CheckWeakVerifierWarning(lineView, "")
		}
		if !w.environmentFailure || !w.weakWarning {
			w.retainDiagnosticTail(diagnostic)
		}
	}
	return len(p), nil
}

// grep -c can report a polarity error only when all of stdout trims to "0".
// Keep this independent of the evidence cutoff and carry split UTF-8 spaces.
type verificationZeroOutput struct {
	tail      string
	seenZero  bool
	seenOther bool
}

func (z *verificationZeroOutput) observe(p []byte) {
	if z.seenOther {
		return
	}
	text := z.tail + string(p)
	for len(text) > 0 {
		if !utf8.FullRuneInString(text) {
			break
		}
		r, size := utf8.DecodeRuneInString(text)
		if !unicode.IsSpace(r) {
			if r != '0' || z.seenZero {
				z.seenOther = true
				return
			}
			z.seenZero = true
		}
		text = text[size:]
	}
	z.tail = strings.Clone(text)
}

func (z *verificationZeroOutput) onlyZero() bool {
	return z != nil && z.seenZero && !z.seenOther && z.tail == ""
}

func (w *verificationOutput) retainDiagnosticTail(diagnostic string) {
	start := max(0, len(diagnostic)-verificationDiagnosticOverlap)
	for start > 0 && !utf8.RuneStart(diagnostic[start]) {
		start--
	}
	discarded := diagnostic[:start]
	if newline := strings.LastIndexByte(discarded, '\n'); newline >= 0 {
		w.lineStartedBeforeTail = strings.TrimSpace(discarded[newline+1:]) != ""
	} else if strings.TrimSpace(discarded) != "" {
		w.lineStartedBeforeTail = true
	}
	w.diagnosticTail = strings.Clone(diagnostic[start:])
}

func (w *verificationOutput) evidence(env []string) string {
	output := utils.RedactSubprocessOutputPrefix(w.prefix.String(), env, verificationRedactionPrefix)
	output = strings.TrimSpace(output)
	if w.prefix.Len() > verificationRedactionPrefix || w.truncated {
		output += "…"
	}
	return utils.TruncateString(output, verificationEvidenceBytes)
}

// Compare the complete private value. Bound and mask the diagnostic at the
// value boundary before formatting, so a failed comparison cannot serialize
// an arbitrary command output back into its public evidence or error.
func checkPrivateJSONAssertion(data any, assertion JSONAssertion, index int) error {
	actual, err := resolveJSONAssertionValue(data, assertion.Path)
	if err != nil {
		return fmt.Errorf("JSON assertion %d %w", index+1, errVerificationAssertionPath)
	}
	if equalJSONValues(actual, assertion.Equals) {
		return nil
	}
	env := os.Environ()
	return fmt.Errorf("JSON assertion %d did not match: expected %s, got %s", index+1,
		verificationJSONValueEvidence(assertion.Equals, env), verificationJSONValueEvidence(actual, env))
}

func verificationJSONValueEvidence(value any, env []string) string {
	const prefixBytes = 160
	var raw string
	switch value := value.(type) {
	case string:
		raw = value
	case json.Number:
		raw = string(value)
	default:
		if !isJSONScalar(value) {
			return fmt.Sprintf("<%T>", value)
		}
		raw = fmt.Sprint(value)
	}
	captured := raw[:min(len(raw), utils.SubprocessOutputRedactionWindow(prefixBytes, env))]
	detail := utils.RedactSubprocessOutputPrefix(captured, env, prefixBytes)
	if len(raw) > prefixBytes {
		detail += "…"
	}
	return fmt.Sprintf("%q (%T)", utils.TruncateString(detail, prefixBytes), value)
}
