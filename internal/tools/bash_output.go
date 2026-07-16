//go:build linux || darwin
// +build linux darwin

package tools

import (
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
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

func buildBashResponse(stdout, stderr string, exitCode int) fantasy.ToolResponse {
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
	if exitCode != 0 {
		if result.Len() > 0 {
			result.WriteString("\n")
		}
		fmt.Fprintf(&result, "Exit code: %d", exitCode)
	}

	output := result.String()
	if output == "" {
		output = "(no output)"
	}

	tr := TruncateTail(output, defaultMaxLines, defaultMaxBytes)

	if exitCode != 0 {
		return fantasy.NewTextErrorResponse(tr.Content)
	}
	return fantasy.NewTextResponse(tr.Content)
}
