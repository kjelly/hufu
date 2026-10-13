package embedding

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	encoderProfileSchemaVersion = 1
	ollamaAdapterVersion        = "ollama-embed-v1"
	normalizationL2             = "l2-v1"

	// QueryPolicyGoalHead embeds only the request goal and cuts it to the
	// profile's query token budget (spec D3).
	QueryPolicyGoalHead = "goal-head-v1"
	// TokenEstimatorCJKOther is the conservative rune-based token estimate
	// calibrated on hufu task descriptions: CJK runes count 1.3 tokens and
	// all other runes 0.4.
	TokenEstimatorCJKOther = "cjk1.3-other0.4-v1"
)

// EncoderProfile is everything that determines the bytes of a stored
// document vector. Any change produces a new fingerprint and therefore a new
// semantic generation; query-only settings live in QueryPolicy instead so a
// query budget change does not force re-embedding every document.
type EncoderProfile struct {
	SchemaVersion  int    `json:"schema_version"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	ArtifactDigest string `json:"artifact_digest"`
	Family         string `json:"family"`
	Quantization   string `json:"quantization"`
	Dimensions     int    `json:"dimensions"`
	DocumentPrefix string `json:"document_prefix"`
	Normalization  string `json:"normalization"`
	MaxInputTokens int    `json:"max_input_tokens"`
	AdapterVersion string `json:"adapter_version"`
}

// Validate checks the profile is complete enough to identify vectors.
func (p EncoderProfile) Validate() error {
	switch {
	case p.SchemaVersion != encoderProfileSchemaVersion:
		return fmt.Errorf("unsupported encoder profile schema version %d", p.SchemaVersion)
	case p.Provider != "ollama":
		return fmt.Errorf("unsupported encoder provider %q", p.Provider)
	case strings.TrimSpace(p.Model) == "":
		return errors.New("encoder profile model is required")
	case !validSHA256(p.ArtifactDigest):
		return errors.New("encoder profile artifact digest must be a full lowercase SHA-256")
	case p.Dimensions <= 0 || p.Dimensions > maxDimensions:
		return fmt.Errorf("invalid encoder profile dimensions %d", p.Dimensions)
	case p.MaxInputTokens <= 0:
		return errors.New("encoder profile max input tokens must be positive")
	case p.Normalization != normalizationL2 || p.AdapterVersion != ollamaAdapterVersion:
		return errors.New("encoder profile normalization or adapter version is unsupported")
	}
	return nil
}

// Fingerprint is the SHA-256 of the canonical JSON encoding. Field order is
// fixed by the struct, so equal profiles always hash equally.
func (p EncoderProfile) Fingerprint() string {
	encoded, _ := json.Marshal(p)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// Identity maps the profile onto the projection's ModelIdentity.
func (p EncoderProfile) Identity() ModelIdentity {
	return ModelIdentity{
		Scheme: IdentitySchemeOllamaEncoder, ID: "ollama/" + p.Model, Revision: p.Fingerprint(),
		Dimensions: p.Dimensions, ManifestSHA256: p.ArtifactDigest,
	}
}

// QueryPolicy shapes query text before it is embedded. It never changes
// stored document vectors.
type QueryPolicy struct {
	Name         string `json:"name"`
	Prefix       string `json:"prefix"`
	PrefixTokens int    `json:"prefix_tokens"`
	MaxTokens    int    `json:"max_tokens"`
	Estimator    string `json:"estimator"`
}

// Validate checks the policy leaves room for query text.
func (q QueryPolicy) Validate() error {
	if q.Name != QueryPolicyGoalHead || q.Estimator != TokenEstimatorCJKOther {
		return fmt.Errorf("unsupported query policy %q with estimator %q", q.Name, q.Estimator)
	}
	if q.PrefixTokens < 0 || q.MaxTokens <= q.PrefixTokens {
		return fmt.Errorf("query budget %d leaves no room after a %d-token prefix", q.MaxTokens, q.PrefixTokens)
	}
	return nil
}

// Fingerprint identifies the query side of a retrieval for traces.
func (q QueryPolicy) Fingerprint() string {
	encoded, _ := json.Marshal(q)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// ShapedQuery is the text actually embedded for a query.
type ShapedQuery struct {
	Text            string
	Truncated       bool
	EstimatedTokens int
}

// Shape normalizes whitespace and, when the estimate exceeds the budget, cuts
// the text at the last sentence end, else word break, else rune inside the
// budget. The prefix is not included in Text.
func (q QueryPolicy) Shape(text string) ShapedQuery {
	normalized := strings.Join(strings.Fields(text), " ")
	budget := q.MaxTokens - q.PrefixTokens
	if estimate := EstimateTokens(normalized); estimate <= budget {
		return ShapedQuery{Text: normalized, EstimatedTokens: q.PrefixTokens + estimate}
	}
	var cost float64
	end := 0
	for i, r := range normalized {
		next := cost + runeTokenCost(r)
		if math.Ceil(next) > float64(budget) {
			break
		}
		cost = next
		end = i + utf8.RuneLen(r)
	}
	cut := cutAtBoundary(normalized[:end])
	return ShapedQuery{Text: cut, Truncated: true, EstimatedTokens: q.PrefixTokens + EstimateTokens(cut)}
}

// cutAtBoundary prefers a sentence end, then a space, in the second half of
// head; otherwise it keeps head as is (already a rune boundary).
func cutAtBoundary(head string) string {
	half := len(head) / 2
	for i := len(head); i > half; {
		r, size := utf8.DecodeLastRuneInString(head[:i])
		switch r {
		case '。', '！', '？', '!', '?':
			return strings.TrimSpace(head[:i])
		case '.':
			if i == len(head) || head[i] == ' ' {
				return strings.TrimSpace(head[:i])
			}
		}
		i -= size
	}
	if space := strings.LastIndexByte(head, ' '); space > half {
		return strings.TrimSpace(head[:space])
	}
	return strings.TrimSpace(head)
}

// EstimateTokens is the TokenEstimatorCJKOther estimate, rounded up.
func EstimateTokens(text string) int {
	var cost float64
	for _, r := range text {
		cost += runeTokenCost(r)
	}
	return int(math.Ceil(cost))
}

func runeTokenCost(r rune) float64 {
	if isWideRune(r) {
		return 1.3
	}
	return 0.4
}

// isWideRune covers CJK ideographs, CJK punctuation and full-width forms, the
// ranges the estimator was calibrated with.
func isWideRune(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		(r >= 0x3000 && r <= 0x303f) || (r >= 0xff00 && r <= 0xffef) ||
		unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}

// Preset holds the per-model prompt format and the token budgets measured
// for it (spec §0.3, D3, D4). Budgets may be overridden by configuration.
type Preset struct {
	Family            string
	DocumentPrefix    string
	QueryPrefix       string
	QueryPrefixTokens int
	MaxInputTokens    int
	QueryMaxTokens    int
}

const qwen3EmbeddingInstruction = "Instruct: Retrieve relevant verified technical decisions, failure analyses, " +
	"operational constraints and reusable lessons from an AI agent's memory.\nQuery:"

// PresetFor returns the preset for an Ollama model name such as
// "embeddinggemma:latest". Only measured models are supported.
func PresetFor(model string) (Preset, bool) {
	name, tag, _ := strings.Cut(NormalizeOllamaModelName(model), ":")
	switch {
	case name == "embeddinggemma":
		return Preset{
			Family: "embeddinggemma", DocumentPrefix: "title: none | text: ",
			QueryPrefix: "task: search result | query: ", QueryPrefixTokens: 10,
			MaxInputTokens: 2048, QueryMaxTokens: 512,
		}, true
	case name == "qwen3-embedding" && strings.HasPrefix(tag, "0.6b"):
		return Preset{
			Family: "qwen3-embedding", QueryPrefix: qwen3EmbeddingInstruction, QueryPrefixTokens: 27,
			MaxInputTokens: 4096, QueryMaxTokens: 128,
		}, true
	}
	return Preset{}, false
}

// NormalizeOllamaModelName strips an "ollama/" provider prefix and adds the
// implicit ":latest" tag Ollama applies to untagged names.
func NormalizeOllamaModelName(model string) string {
	model = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(model), "ollama/"))
	if model != "" && !strings.Contains(model, ":") {
		model += ":latest"
	}
	return model
}
