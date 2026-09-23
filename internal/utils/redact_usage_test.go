package utils

import (
	"strings"
	"testing"
)

func TestRedactJSONLDataPreservesUsageCounters(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		want    []string
		notWant []string
	}{
		{
			name: "numeric usage counters survive",
			line: `{"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"progress_tokens":5,"cache_read_tokens":7,"cache_creation_tokens":3}}`,
			want: []string{`"input_tokens":10`, `"output_tokens":20`, `"total_tokens":30`, `"progress_tokens":5`, `"cache_read_tokens":7`, `"cache_creation_tokens":3`},
		},
		{
			name:    "string values under usage keys stay redacted",
			line:    `{"usage":{"input_tokens":"sk-live-abcdefghijklmnop"}}`,
			notWant: []string{"sk-live-abcdefghijklmnop"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(RedactJSONLData([]byte(tc.line + "\n")))
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("redacted %s missing %s", got, want)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(got, notWant) {
					t.Fatalf("redacted %s still contains %s", got, notWant)
				}
			}
		})
	}
}
