package team

import (
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestProviderFailureDetailPreservesProviderSourceAfterLocalTool(t *testing.T) {
	c := &Coordinator{}
	c.SetCurrentTool("grep")
	providerErr := &fantasy.ProviderError{
		Title:        "service unavailable",
		Message:      "Service is disabled",
		StatusCode:   503,
		ResponseBody: []byte("<html>503 Service Unavailable</html>"),
	}
	err := annotateProviderModelFailure(providerErr, "ollama/qwen3:8b")
	detail := c.FailureDetail(err, FailureSourceError)

	for _, want := range []string{
		"source=provider/model",
		"provider=ollama",
		"model=ollama/qwen3:8b",
		"status_code=503",
		"Service is disabled",
		"503 Service Unavailable",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("provider detail %q missing %q", detail, want)
		}
	}
	if strings.Contains(detail, "last_tool=grep") {
		t.Fatalf("provider failure was attributed to stale local tool: %q", detail)
	}
}
