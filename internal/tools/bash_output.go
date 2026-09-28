//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/utils"
)

// timeoutResponseMessage reports a command timeout together with whatever the
// command printed before it was killed, so the model can see the state it was
// stuck in instead of a bare "timed out".
func timeoutResponseMessage(timeout time.Duration, stdout, stderr string) string {
	msg := fmt.Sprintf("command timed out after %s", timeout)
	combined := strings.TrimSpace(stdout)
	if s := strings.TrimSpace(stderr); s != "" {
		if combined != "" {
			combined += "\n"
		}
		combined += "STDERR:\n" + s
	}
	if combined == "" {
		return msg
	}
	tr := TruncateTail(combined, defaultMaxLines, defaultMaxBytes)
	return msg + ". Output before the kill:\n" + tr.Content
}

// bashCommandResponse returns the response for a finished shell command. An
// offloadable call first hands the complete output to the attempt's
// offloader; otherwise, or when it declines, the legacy bound applies.
func bashCommandResponse(ctx context.Context, offloadable bool, stdout, stderr string, exitCode int) fantasy.ToolResponse {
	output := assembleBashOutput(stdout, stderr, exitCode)
	if offloadable {
		if response, ok := OffloadToolOutput(ctx, ToolOutputCapture{
			ToolName: "bash", Content: output, IsError: exitCode != 0,
			LegacyWouldTruncate: bashOutputWouldTruncate(output),
		}); ok {
			return response
		}
	}
	return bashExitResponse(boundBashOutput(output), exitCode)
}

func buildBashResponse(stdout, stderr string, exitCode int) fantasy.ToolResponse {
	return bashExitResponse(boundBashOutput(assembleBashOutput(stdout, stderr, exitCode)), exitCode)
}

// assembleBashOutput builds the complete, redacted model-facing text of a
// finished command: stdout, a STDERR section, and the exit-code footer, which
// is always the final line.
func assembleBashOutput(stdout, stderr string, exitCode int) string {
	stdout = utils.RedactSecrets(stdout)
	stderr = utils.RedactSecrets(stderr)
	var result strings.Builder
	if stdout != "" {
		result.WriteString(stdout)
	}
	if stderr != "" {
		if result.Len() > 0 {
			result.WriteString("\n")
		}
		result.WriteString("STDERR:\n")
		result.WriteString(stderr)
	}
	if result.Len() > 0 {
		result.WriteString("\n")
	}
	fmt.Fprintf(&result, "Exit code: %d", exitCode)
	return result.String()
}

// boundBashOutput applies the legacy display bound. When the bound changes
// anything it says so on the first line, so the model never mistakes a tail
// for the whole output.
func boundBashOutput(output string) string {
	tr := TruncateTail(output, defaultMaxLines, defaultMaxBytes)
	if tr.Content == output {
		return output
	}
	return bashTruncationNotice(output, tr) + "\n" + tr.Content
}

// bashOutputWouldTruncate reports whether boundBashOutput would change output.
func bashOutputWouldTruncate(output string) bool {
	return TruncateTail(output, defaultMaxLines, defaultMaxBytes).Content != output
}

func bashTruncationNotice(output string, tr TruncationResult) string {
	var clauses strings.Builder
	if tr.Omitted {
		clauses.WriteString("; earlier output was omitted")
	}
	if tr.LongLinesCut {
		fmt.Fprintf(&clauses, "; lines over %d characters were cut", defaultMaxLineLen)
	}
	return fmt.Sprintf("[output truncated by hufu: kept %d of %d lines (%d of %d bytes)%s]",
		strings.Count(tr.Content, "\n")+1, strings.Count(output, "\n")+1, len(tr.Content), len(output), clauses.String())
}

func bashExitResponse(content string, exitCode int) fantasy.ToolResponse {
	if exitCode != 0 {
		return fantasy.NewTextErrorResponse(content)
	}
	return fantasy.NewTextResponse(content)
}
