package utils

import (
	"strings"
	"testing"
)

func TestPairedAllocationLimitsRemainNumericOnly(t *testing.T) {
	resetLearnedSecrets(t)
	for _, key := range []string{"per_stage_tokens", "pipeline_tokens"} {
		for _, value := range []string{"12000", `"sensitive_value"`, "true"} {
			raw := []byte(`{"` + key + `":` + value + `}`)
			redacted, err := RedactJSON(raw)
			if err != nil {
				t.Fatal(err)
			}
			if value == "12000" {
				if !strings.Contains(string(redacted), "12000") {
					t.Fatalf("numeric budget redacted: %s", redacted)
				}
			} else if !strings.Contains(string(redacted), "[REDACTED]") {
				t.Fatalf("nonnumeric credential-shaped value escaped redaction: %s", redacted)
			}
		}
	}
}
