package tools

import (
	"strings"
	"testing"
)

func TestSecretRegistryRedactsTextAndJSON(t *testing.T) {
	r := NewSecretRegistry()
	if err := r.Register(SecretRef{Name: "api-key", Source: "test", ExactValue: "secret-value"}); err != nil {
		t.Fatal(err)
	}
	if got := r.RedactText("token=secret-value"); strings.Contains(got, "secret-value") || !strings.Contains(got, "REDACTED:api-key") {
		t.Fatalf("redacted text = %q", got)
	}
	data, err := r.RedactJSON([]byte(`{"token":"secret-value","nested":["secret-value"]}`))
	if err != nil || strings.Contains(string(data), "secret-value") {
		t.Fatalf("redacted JSON = %s, err %v", data, err)
	}
	if refs := r.References(); len(refs) != 1 || refs[0].ExactValue != "" {
		t.Fatalf("secret references leaked values: %#v", refs)
	}
}

func TestSecretRegistryFailsClosedForInvalidJSON(t *testing.T) {
	r := NewSecretRegistry()
	if _, err := r.RedactJSON([]byte("not-json")); err == nil {
		t.Fatal("invalid JSON was redacted without an error")
	}
}
