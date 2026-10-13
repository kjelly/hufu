package embedding

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int
	}{
		{name: "empty", text: "", want: 0},
		{name: "ascii", text: "abcde", want: 2},
		{name: "cjk", text: "中文字", want: 4},
		{name: "mixed", text: "go 測試", want: 4},
		{name: "fullwidth punctuation", text: "，。", want: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := EstimateTokens(tc.text); got != tc.want {
				t.Fatalf("EstimateTokens(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}

func TestQueryPolicyShape(t *testing.T) {
	policy := QueryPolicy{Name: QueryPolicyGoalHead, PrefixTokens: 10, MaxTokens: 30, Estimator: TokenEstimatorCJKOther}
	longEnglish := "Fix the resume path. " + strings.Repeat("coordinator receipts avoid duplicate side effects ", 6)
	tests := []struct {
		name          string
		text          string
		wantText      string
		wantTruncated bool
		wantPrefix    string
	}{
		{name: "short kept", text: "  check   receipts \n now ", wantText: "check receipts now"},
		{name: "sentence boundary", text: "Fix the resume path now. Then " + strings.Repeat("x", 80), wantPrefix: "Fix the resume path now.", wantTruncated: true},
		{name: "word boundary", text: strings.Repeat("alpha ", 20), wantTruncated: true},
		{name: "cjk sentence", text: "協調者中斷後要檢查收據。" + strings.Repeat("避免重複副作用", 5), wantText: "協調者中斷後要檢查收據。", wantTruncated: true},
		{name: "long english", text: longEnglish, wantPrefix: "Fix the resume path.", wantTruncated: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := policy.Shape(tc.text)
			if got.Truncated != tc.wantTruncated {
				t.Fatalf("Truncated = %v, want %v (text %q)", got.Truncated, tc.wantTruncated, got.Text)
			}
			if tc.wantText != "" && got.Text != tc.wantText {
				t.Fatalf("Text = %q, want %q", got.Text, tc.wantText)
			}
			if !strings.HasPrefix(got.Text, tc.wantPrefix) {
				t.Fatalf("Text = %q, want prefix %q", got.Text, tc.wantPrefix)
			}
			if !utf8.ValidString(got.Text) {
				t.Fatalf("Text is not valid UTF-8: %q", got.Text)
			}
			if got.EstimatedTokens > policy.MaxTokens {
				t.Fatalf("EstimatedTokens = %d exceeds budget %d", got.EstimatedTokens, policy.MaxTokens)
			}
		})
	}
}

func TestQueryPolicyShapeCutsInsideRunesSafely(t *testing.T) {
	policy := QueryPolicy{Name: QueryPolicyGoalHead, MaxTokens: 5, Estimator: TokenEstimatorCJKOther}
	got := policy.Shape(strings.Repeat("測", 50))
	if !got.Truncated || got.Text != "測測測" || !utf8.ValidString(got.Text) {
		t.Fatalf("Shape = %+v, want three whole runes", got)
	}
}

func TestPresetFor(t *testing.T) {
	tests := []struct {
		model      string
		ok         bool
		maxInput   int
		queryMax   int
		docPrefix  string
		queryStart string
	}{
		{model: "ollama/embeddinggemma:latest", ok: true, maxInput: 2048, queryMax: 512, docPrefix: "title: none | text: ", queryStart: "task: search result"},
		{model: "embeddinggemma", ok: true, maxInput: 2048, queryMax: 512, docPrefix: "title: none | text: ", queryStart: "task: search result"},
		{model: "embeddinggemma:300m-qat-q8_0", ok: true, maxInput: 2048, queryMax: 512, docPrefix: "title: none | text: ", queryStart: "task: search result"},
		{model: "qwen3-embedding:0.6b", ok: true, maxInput: 4096, queryMax: 128, queryStart: "Instruct:"},
		{model: "qwen3-embedding:4b", ok: false},
		{model: "nomic-embed-text:latest", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			preset, ok := PresetFor(tc.model)
			if ok != tc.ok {
				t.Fatalf("PresetFor(%q) ok = %v, want %v", tc.model, ok, tc.ok)
			}
			if !ok {
				return
			}
			if preset.MaxInputTokens != tc.maxInput || preset.QueryMaxTokens != tc.queryMax ||
				preset.DocumentPrefix != tc.docPrefix || !strings.HasPrefix(preset.QueryPrefix, tc.queryStart) {
				t.Fatalf("PresetFor(%q) = %+v", tc.model, preset)
			}
		})
	}
}

func TestEncoderProfileFingerprintTracksVectorAffectingFields(t *testing.T) {
	base := testEncoderProfile()
	tests := []struct {
		name   string
		mutate func(*EncoderProfile)
	}{
		{name: "digest", mutate: func(p *EncoderProfile) { p.ArtifactDigest = strings.Repeat("b", 64) }},
		{name: "dimensions", mutate: func(p *EncoderProfile) { p.Dimensions = 512 }},
		{name: "document prefix", mutate: func(p *EncoderProfile) { p.DocumentPrefix = "" }},
		{name: "max input", mutate: func(p *EncoderProfile) { p.MaxInputTokens = 1024 }},
		{name: "quantization", mutate: func(p *EncoderProfile) { p.Quantization = "Q8_0" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			changed := base
			tc.mutate(&changed)
			if changed.Fingerprint() == base.Fingerprint() {
				t.Fatalf("fingerprint did not change for %s", tc.name)
			}
		})
	}
	if base.Fingerprint() != testEncoderProfile().Fingerprint() {
		t.Fatal("fingerprint is not deterministic")
	}
	identity := base.Identity()
	if identity.Scheme != IdentitySchemeOllamaEncoder || identity.Revision != base.Fingerprint() ||
		identity.ManifestSHA256 != base.ArtifactDigest || identity.TableSHA256 != "" || identity.ID != "ollama/embeddinggemma:latest" {
		t.Fatalf("Identity() = %+v", identity)
	}
}

func testEncoderProfile() EncoderProfile {
	return EncoderProfile{
		SchemaVersion: encoderProfileSchemaVersion, Provider: "ollama", Model: "embeddinggemma:latest",
		ArtifactDigest: strings.Repeat("a", 64), Family: "gemma3", Quantization: "BF16", Dimensions: 4,
		DocumentPrefix: "title: none | text: ", Normalization: normalizationL2, MaxInputTokens: 2048,
		AdapterVersion: ollamaAdapterVersion,
	}
}
