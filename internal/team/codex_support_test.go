package team

import "testing"

const unsupportedCodexIntegrationReason = "Hufu Codex App Server integration is temporarily unsupported"

func skipUnsupportedCodexIntegration(t *testing.T) {
	t.Helper()
	t.Skip(unsupportedCodexIntegrationReason)
}
