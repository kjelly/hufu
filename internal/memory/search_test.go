package memory

import (
	"context"
	"strings"
	"testing"
)

func TestAutoQueryNilStore(t *testing.T) {
	result, err := AutoQuery(context.Background(), nil, "test query", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty result for nil store, got: %s", result)
	}
}

func TestAutoQueryEmptyPrompt(t *testing.T) {
	result, err := AutoQuery(context.Background(), nil, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty result for empty prompt, got: %s", result)
	}
}

func TestAutoQueryResultFormat(t *testing.T) {
	if !strings.Contains("## Relevant Memory", "Relevant Memory") {
		t.Error("expected header to contain 'Relevant Memory'")
	}
}
