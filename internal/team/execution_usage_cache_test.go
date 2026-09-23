package team

import (
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestUsageFromStepsSumsCacheTokens(t *testing.T) {
	cases := []struct {
		name  string
		steps []fantasy.StepResult
		want  ExecutionUsage
	}{
		{
			name:  "no cache reported",
			steps: []fantasy.StepResult{{Response: fantasy.Response{Usage: fantasy.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}}}},
			want:  ExecutionUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12},
		},
		{
			name: "reads only",
			steps: []fantasy.StepResult{
				{Response: fantasy.Response{Usage: fantasy.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 42, CacheReadTokens: 30}}},
				{Response: fantasy.Response{Usage: fantasy.Usage{InputTokens: 5, OutputTokens: 1, TotalTokens: 46, CacheReadTokens: 40}}},
			},
			want: ExecutionUsage{InputTokens: 15, OutputTokens: 3, TotalTokens: 88, CacheReadTokens: 70},
		},
		{
			name:  "reads and writes",
			steps: []fantasy.StepResult{{Response: fantasy.Response{Usage: fantasy.Usage{InputTokens: 4, OutputTokens: 1, TotalTokens: 5, CacheReadTokens: 6, CacheCreationTokens: 8}}}},
			want:  ExecutionUsage{InputTokens: 4, OutputTokens: 1, TotalTokens: 5, CacheReadTokens: 6, CacheCreationTokens: 8},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usageFromSteps(tc.steps); got != tc.want {
				t.Fatalf("usageFromSteps = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestExecutionUsagePromptTokens(t *testing.T) {
	usage := ExecutionUsage{InputTokens: 3, OutputTokens: 100, CacheReadTokens: 4, CacheCreationTokens: 5}
	if got := usage.PromptTokens(); got != 12 {
		t.Fatalf("PromptTokens = %d, want 12", got)
	}
}

func TestExecutionUsageJSONOmitsZeroCacheFields(t *testing.T) {
	cases := []struct {
		name  string
		usage ExecutionUsage
		want  string
	}{
		{name: "zero cache keeps baseline shape", usage: ExecutionUsage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}, want: `{"input_tokens":1,"output_tokens":2,"total_tokens":3}`},
		{name: "cache fields appended", usage: ExecutionUsage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3, CacheReadTokens: 4, CacheCreationTokens: 5}, want: `{"input_tokens":1,"output_tokens":2,"total_tokens":3,"cache_read_tokens":4,"cache_creation_tokens":5}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.usage)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tc.want {
				t.Fatalf("json = %s, want %s", data, tc.want)
			}
		})
	}
}

func TestLLMLogStreamFinishCachePlacement(t *testing.T) {
	cases := []struct {
		name     string
		usage    fantasy.Usage
		reqBytes int
		want     []string
		notWant  []string
	}{
		{name: "no cache", usage: fantasy.Usage{InputTokens: 10, OutputTokens: 2}, want: []string{"tokens_out=2 ==="}, notWant: []string{"cache_read"}},
		{name: "cache after tokens_out", usage: fantasy.Usage{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 30, CacheCreationTokens: 4}, want: []string{"tokens_out=2 cache_read=30 cache_write=4 ==="}},
		{name: "cache write only", usage: fantasy.Usage{InputTokens: 1, OutputTokens: 1, CacheCreationTokens: 9}, want: []string{"cache_read=0 cache_write=9"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logged string
			llmLogStreamFinish(func(s string) { logged += s }, fantasy.FinishReasonStop, tc.usage, tc.reqBytes)
			for _, want := range tc.want {
				if !strings.Contains(logged, want) {
					t.Fatalf("log %q missing %q", logged, want)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(logged, notWant) {
					t.Fatalf("log %q unexpectedly contains %q", logged, notWant)
				}
			}
		})
	}
}
