package team

import (
	"bytes"
	"errors"
	"testing"
)

func TestCanonicalDecisionJSONVectors(t *testing.T) {
	tests := []struct {
		name       string
		input      any
		wantHex    string
		wantDigest string
	}{
		{
			name:       "unicode and key order",
			input:      map[string]any{"b": true, "a": "繁體中文<&>\u2028\n"},
			wantHex:    "7b2261223a22e7b981e9ab94e4b8ade696873c263ee280a85c6e222c2262223a747275657d",
			wantDigest: "3820045ed19dd3d4d9c430881f0e891a275ed24612178a5b4cd6727a3645a783",
		},
		{
			name:       "null and max integer",
			input:      map[string]any{"a": nil, "z": []any{uint64(0), uint64(1), maxCanonicalJSONInteger}},
			wantHex:    "7b2261223a6e756c6c2c227a223a5b302c312c393030373139393235343734303939315d7d",
			wantDigest: "281b25cc4b401e4e31b93eed69d55c37b38b0a0224f4816bb96c6e720a156d17",
		},
		{
			name:       "control escapes",
			input:      map[string]any{"s": "\"\\\b\t\n\f\r\x01"},
			wantHex:    "7b2273223a225c225c5c5c625c745c6e5c665c725c7530303031227d",
			wantDigest: "56c130210722fc056a74dab96eab41c92afd3e254cdf98fd5e7f48339423cbaf",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := CanonicalDecisionJSON(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if gotHex := fmtHex(got); gotHex != test.wantHex {
				t.Fatalf("canonical hex = %s, want %s", gotHex, test.wantHex)
			}
			digest, err := DecisionContractDigest("hufu/test/v1", test.input)
			if err != nil {
				t.Fatal(err)
			}
			if digest != test.wantDigest {
				t.Fatalf("digest = %s, want %s", digest, test.wantDigest)
			}
		})
	}
}

func TestDecisionIdentityVectors(t *testing.T) {
	teamID, err := DecisionTeamID(" fixture-team ")
	if err != nil {
		t.Fatal(err)
	}
	if want := "team_fb375d896910b18ee57646004c0190ff67c58a93ac276948b4ad7eb99e9a8d8f"; teamID != want {
		t.Fatalf("team ID = %s, want %s", teamID, want)
	}
	logicalID := "ldr_01010101010101010101010101010101"
	taskID, err := PrimaryDecisionTaskID("main", logicalID)
	if err != nil {
		t.Fatal(err)
	}
	if want := "__hufu_pd_022e76e7152401093a8164f3dad9cdeca829a8fcb9053ddae7c51e7818a38006"; taskID != want {
		t.Fatalf("task ID = %s, want %s", taskID, want)
	}
	decisionID, err := PrimaryDecisionID("main", logicalID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := "pd_9c8da055a527c9bd3e41c024a87bc7b87e64954447092a41465a3db07bdcf02b"; decisionID != want {
		t.Fatalf("decision ID = %s, want %s", decisionID, want)
	}
}

func TestNewLogicalRunIDFailsClosedOnEntropyError(t *testing.T) {
	if _, err := newLogicalRunID(errorReader{}); err == nil {
		t.Fatal("expected entropy failure")
	}
	got, err := newLogicalRunID(bytes.NewReader(bytes.Repeat([]byte{0xab}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if got != "ldr_abababababababababababababababab" {
		t.Fatalf("logical ID = %q", got)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func fmtHex(value []byte) string {
	const digits = "0123456789abcdef"
	result := make([]byte, len(value)*2)
	for index, b := range value {
		result[index*2] = digits[b>>4]
		result[index*2+1] = digits[b&0xf]
	}
	return string(result)
}
