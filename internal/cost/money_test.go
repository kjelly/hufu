package cost

import (
	"math"
	"testing"
)

func TestParseAndFormatUSDMicros(t *testing.T) {
	tests := []struct {
		value string
		want  int64
	}{
		{value: "0", want: 0},
		{value: "0.20", want: 200_000},
		{value: "2.000001", want: 2_000_001},
		{value: "9223372036854.775807", want: math.MaxInt64},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, err := ParseUSDMicros(tt.value)
			if err != nil || got != tt.want {
				t.Fatalf("ParseUSDMicros(%q) = %d, %v; want %d", tt.value, got, err, tt.want)
			}
			formatted, err := FormatUSDMicros(got)
			if err != nil {
				t.Fatal(err)
			}
			if reparsed, err := ParseUSDMicros(formatted); err != nil || reparsed != got {
				t.Fatalf("canonical round trip %q = %d, %v; want %d", formatted, reparsed, err, got)
			}
		})
	}
}

func TestParseUSDMicrosRejectsNonCanonicalAndOverflow(t *testing.T) {
	for _, value := range []string{"", "2.0 ", " 2.0", "-1", "+1", "1e-3", "$2", ".1", "1.", "00", "01", "0.0000001", "9223372036854.775808"} {
		t.Run(value, func(t *testing.T) {
			if _, err := ParseUSDMicros(value); err == nil {
				t.Fatalf("ParseUSDMicros(%q) succeeded", value)
			}
		})
	}
	if _, err := FormatUSDMicros(-1); err == nil {
		t.Fatal("FormatUSDMicros accepted a negative value")
	}
}

func TestTokenRateMicrosRoundsUpAndChecksOverflow(t *testing.T) {
	tests := []struct {
		name   string
		tokens int64
		rate   int64
		want   int64
	}{
		{name: "zero", tokens: 0, rate: math.MaxInt64, want: 0},
		{name: "sub micro rounds up", tokens: 1, rate: 1, want: 1},
		{name: "exact million", tokens: 1_000_000, rate: 2_000_000, want: 2_000_000},
		{name: "fractional micro", tokens: 500_001, rate: 2, want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := TokenRateMicros(tt.tokens, tt.rate)
			if err != nil || got != tt.want {
				t.Fatalf("TokenRateMicros(%d, %d) = %d, %v; want %d", tt.tokens, tt.rate, got, err, tt.want)
			}
		})
	}
	for _, input := range [][2]int64{{-1, 1}, {1, -1}, {math.MaxInt64, math.MaxInt64}} {
		if _, err := TokenRateMicros(input[0], input[1]); err == nil {
			t.Fatalf("TokenRateMicros(%d, %d) succeeded", input[0], input[1])
		}
	}
}
