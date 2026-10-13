// Package semantic holds the operator configuration for canonical semantic
// memory retrieval. It is separate from the legacy embedding-model setting:
// that one names the chromem index model, this one controls whether and how
// the coordinator and workers embed canonical context items.
package semantic

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/embedding"
)

const (
	// DefaultModel is the default encoder, also the legacy embedding-model
	// default (spec decision: embeddinggemma ran ~5x faster than qwen3 on CPU).
	DefaultModel    = "ollama/embeddinggemma:latest"
	DefaultEndpoint = "http://127.0.0.1:11434"

	defaultQueryTimeout   = 3 * time.Second
	defaultLoadTimeout    = 30 * time.Second
	defaultIndexTimeout   = 5 * time.Minute
	defaultKeepAlive      = 10 * time.Minute
	defaultMaxConcurrent  = 1
	defaultIndexBatchSize = 8
	maxMaxConcurrent      = 16
	maxIndexBatchSize     = 64
)

// Mode is the rollout state. Off sends no request and adds no prompt text;
// shadow embeds and traces but never changes selection; active fuses the
// semantic lane into ranking; auto decides per request: it runs active when
// the lexical lane cannot read enough of the query (RoutesSemantic) and the
// lexical-only retrieval of mode off otherwise.
type Mode string

const (
	ModeOff    Mode = "off"
	ModeShadow Mode = "shadow"
	ModeActive Mode = "active"
	ModeAuto   Mode = "auto"
)

// Fusion names how a request that uses the semantic lane fuses it with the
// lexical lane. The values mirror the context package's FusionMode, which
// this package cannot import.
type Fusion string

const (
	FusionLegacy          Fusion = "legacy"
	FusionRRFNormalized   Fusion = "rrf_normalized"
	FusionScoreNormalized Fusion = "score_normalized"

	// DefaultFusion is score_normalized: legacy fusion carries raw BM25
	// scores that outrank every vector-only candidate, so the semantic lane
	// changed no selection in the 2026-10-13 replays.
	DefaultFusion = FusionScoreNormalized
)

// AutoRouteMinShare is the share of a query's estimated tokens that the
// lexical lane drops (non-ASCII letters and digits; FTS keeps ASCII terms
// only) at which auto mode uses the semantic lane. Calibrated on hufu history:
// all 200 English review goals score below 0.15 (198 below 0.01), and 49 of 54
// Chinese research goals score 0.15 or more.
const AutoRouteMinShare = 0.15

// Config is the semantic-retrieval block of hufu.yaml or team.yaml. Zero
// values inherit; Merge applies a later layer field by field.
type Config struct {
	Mode                Mode          `yaml:"mode,omitempty" json:"mode,omitempty"`
	Fusion              Fusion        `yaml:"fusion,omitempty" json:"fusion,omitempty"`
	Model               string        `yaml:"model,omitempty" json:"model,omitempty"`
	Endpoint            string        `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	AllowRemoteEndpoint *bool         `yaml:"allow-remote-endpoint,omitempty" json:"allow_remote_endpoint,omitempty"`
	MaxInputTokens      int           `yaml:"max-input-tokens,omitempty" json:"max_input_tokens,omitempty"`
	QueryMaxTokens      int           `yaml:"query-max-tokens,omitempty" json:"query_max_tokens,omitempty"`
	QueryTimeout        time.Duration `yaml:"query-timeout,omitempty" json:"query_timeout,omitzero"`
	LoadTimeout         time.Duration `yaml:"load-timeout,omitempty" json:"load_timeout,omitzero"`
	IndexTimeout        time.Duration `yaml:"index-timeout,omitempty" json:"index_timeout,omitzero"`
	KeepAlive           time.Duration `yaml:"keep-alive,omitempty" json:"keep_alive,omitzero"`
	MaxConcurrent       int           `yaml:"max-concurrent,omitempty" json:"max_concurrent,omitempty"`
	IndexBatchSize      int           `yaml:"index-batch-size,omitempty" json:"index_batch_size,omitempty"`
}

// Merge returns base with every field override sets.
func Merge(base, override Config) Config {
	merged := base.Clone()
	if override.Mode != "" {
		merged.Mode = override.Mode
	}
	if override.Fusion != "" {
		merged.Fusion = override.Fusion
	}
	if override.Model != "" {
		merged.Model = override.Model
	}
	if override.Endpoint != "" {
		merged.Endpoint = override.Endpoint
	}
	if override.AllowRemoteEndpoint != nil {
		allow := *override.AllowRemoteEndpoint
		merged.AllowRemoteEndpoint = &allow
	}
	for _, field := range []struct{ dst, src *int }{
		{&merged.MaxInputTokens, &override.MaxInputTokens}, {&merged.QueryMaxTokens, &override.QueryMaxTokens},
		{&merged.MaxConcurrent, &override.MaxConcurrent}, {&merged.IndexBatchSize, &override.IndexBatchSize},
	} {
		if *field.src != 0 {
			*field.dst = *field.src
		}
	}
	for _, field := range []struct{ dst, src *time.Duration }{
		{&merged.QueryTimeout, &override.QueryTimeout}, {&merged.LoadTimeout, &override.LoadTimeout},
		{&merged.IndexTimeout, &override.IndexTimeout}, {&merged.KeepAlive, &override.KeepAlive},
	} {
		if *field.src != 0 {
			*field.dst = *field.src
		}
	}
	return merged
}

// Clone deep-copies the pointer field.
func (c Config) Clone() Config {
	if c.AllowRemoteEndpoint != nil {
		allow := *c.AllowRemoteEndpoint
		c.AllowRemoteEndpoint = &allow
	}
	return c
}

// Validate checks the values that are present; Resolve fills the rest.
func (c Config) Validate() error {
	_, err := c.Resolve()
	return err
}

// Settings is the fully resolved configuration.
type Settings struct {
	Mode                Mode          `json:"mode"`
	Fusion              Fusion        `json:"fusion"`
	Model               string        `json:"model"`
	Endpoint            string        `json:"endpoint"`
	AllowRemoteEndpoint bool          `json:"allow_remote_endpoint"`
	MaxInputTokens      int           `json:"max_input_tokens"`
	QueryMaxTokens      int           `json:"query_max_tokens"`
	QueryTimeout        time.Duration `json:"query_timeout"`
	LoadTimeout         time.Duration `json:"load_timeout"`
	IndexTimeout        time.Duration `json:"index_timeout"`
	KeepAlive           time.Duration `json:"keep_alive"`
	MaxConcurrent       int           `json:"max_concurrent"`
	IndexBatchSize      int           `json:"index_batch_size"`
}

// Resolve validates the configuration and fills defaults. Token budgets of
// zero come from the model preset.
func (c Config) Resolve() (Settings, error) {
	s := Settings{
		Mode: c.Mode, Fusion: c.Fusion, Model: embedding.NormalizeOllamaModelName(c.Model), Endpoint: strings.TrimSpace(c.Endpoint),
		MaxInputTokens: c.MaxInputTokens, QueryMaxTokens: c.QueryMaxTokens,
		QueryTimeout: c.QueryTimeout, LoadTimeout: c.LoadTimeout, IndexTimeout: c.IndexTimeout, KeepAlive: c.KeepAlive,
		MaxConcurrent: c.MaxConcurrent, IndexBatchSize: c.IndexBatchSize,
	}
	if c.AllowRemoteEndpoint != nil {
		s.AllowRemoteEndpoint = *c.AllowRemoteEndpoint
	}
	switch s.Mode {
	case "":
		s.Mode = ModeOff
	case ModeOff, ModeShadow, ModeActive, ModeAuto:
	default:
		return Settings{}, fmt.Errorf("semantic-retrieval.mode must be off, shadow, active, or auto, got %q", c.Mode)
	}
	switch s.Fusion {
	case "":
		s.Fusion = DefaultFusion
	case FusionLegacy, FusionRRFNormalized, FusionScoreNormalized:
	default:
		return Settings{}, fmt.Errorf("semantic-retrieval.fusion must be legacy, rrf_normalized, or score_normalized, got %q", c.Fusion)
	}
	if s.Model == "" {
		s.Model = embedding.NormalizeOllamaModelName(DefaultModel)
	}
	if strings.HasPrefix(strings.TrimSpace(c.Model), "/") || strings.Contains(strings.TrimPrefix(strings.TrimSpace(c.Model), "ollama/"), "/") {
		return Settings{}, fmt.Errorf("semantic-retrieval.model must be an ollama model name, got %q", c.Model)
	}
	preset, ok := embedding.PresetFor(s.Model)
	if !ok {
		return Settings{}, fmt.Errorf("semantic-retrieval.model %q is not supported (supported: embeddinggemma, qwen3-embedding:0.6b)", s.Model)
	}
	if s.Endpoint == "" {
		s.Endpoint = DefaultEndpoint
	}
	loopback, err := endpointIsLoopback(s.Endpoint)
	if err != nil {
		return Settings{}, err
	}
	if !loopback && !s.AllowRemoteEndpoint {
		return Settings{}, fmt.Errorf("semantic-retrieval.endpoint %s is not loopback; set allow-remote-endpoint: true to send memory text to it", embedding.RedactEndpoint(s.Endpoint))
	}
	for name, value := range map[string]int{
		"max-input-tokens": s.MaxInputTokens, "query-max-tokens": s.QueryMaxTokens,
		"max-concurrent": s.MaxConcurrent, "index-batch-size": s.IndexBatchSize,
	} {
		if value < 0 {
			return Settings{}, fmt.Errorf("semantic-retrieval.%s must not be negative", name)
		}
	}
	for name, value := range map[string]time.Duration{
		"query-timeout": s.QueryTimeout, "load-timeout": s.LoadTimeout, "index-timeout": s.IndexTimeout, "keep-alive": s.KeepAlive,
	} {
		if value < 0 {
			return Settings{}, fmt.Errorf("semantic-retrieval.%s must not be negative", name)
		}
	}
	if s.MaxInputTokens == 0 {
		s.MaxInputTokens = preset.MaxInputTokens
	}
	if s.QueryMaxTokens == 0 {
		s.QueryMaxTokens = preset.QueryMaxTokens
	}
	if s.QueryMaxTokens > s.MaxInputTokens {
		return Settings{}, fmt.Errorf("semantic-retrieval.query-max-tokens %d exceeds max-input-tokens %d", s.QueryMaxTokens, s.MaxInputTokens)
	}
	if s.QueryMaxTokens <= preset.QueryPrefixTokens {
		return Settings{}, fmt.Errorf("semantic-retrieval.query-max-tokens %d leaves no room after the %d-token query prefix", s.QueryMaxTokens, preset.QueryPrefixTokens)
	}
	defaults := []struct {
		dst *time.Duration
		def time.Duration
	}{{&s.QueryTimeout, defaultQueryTimeout}, {&s.LoadTimeout, defaultLoadTimeout}, {&s.IndexTimeout, defaultIndexTimeout}, {&s.KeepAlive, defaultKeepAlive}}
	for _, d := range defaults {
		if *d.dst == 0 {
			*d.dst = d.def
		}
	}
	if s.MaxConcurrent == 0 {
		s.MaxConcurrent = defaultMaxConcurrent
	}
	if s.IndexBatchSize == 0 {
		s.IndexBatchSize = defaultIndexBatchSize
	}
	if s.MaxConcurrent > maxMaxConcurrent || s.IndexBatchSize > maxIndexBatchSize {
		return Settings{}, fmt.Errorf("semantic-retrieval.max-concurrent must be at most %d and index-batch-size at most %d", maxMaxConcurrent, maxIndexBatchSize)
	}
	return s, nil
}

// Enabled reports whether any embedding request may be sent.
func (s Settings) Enabled() bool { return s.Mode != ModeOff && s.Mode != "" }

// ChangesSelection reports whether the semantic lane may change which
// memories are selected (active, or auto when a request routes to it).
func (s Settings) ChangesSelection() bool { return s.Mode == ModeActive || s.Mode == ModeAuto }

// RoutesSemantic is auto mode's per-request decision: whether the share of
// text the lexical lane drops reaches AutoRouteMinShare. It only reads the
// script of each character and never interprets the words.
func RoutesSemantic(text string) bool {
	return embedding.LexicalDropShare(text) >= AutoRouteMinShare
}

// Hash pins the effective settings in the execution policy snapshot, so a
// resumed session cannot silently switch encoder, endpoint or mode.
func (s Settings) Hash() string {
	pinned := s
	pinned.Endpoint = embedding.RedactEndpoint(s.Endpoint)
	encoded, _ := json.Marshal(pinned)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// EndpointIsLoopback reports whether the endpoint host is a literal loopback
// address or localhost. It never resolves DNS.
func (s Settings) EndpointIsLoopback() bool {
	loopback, err := endpointIsLoopback(s.Endpoint)
	return err == nil && loopback
}

func endpointIsLoopback(endpoint string) (bool, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return false, fmt.Errorf("semantic-retrieval.endpoint %q must be an http or https URL", embedding.RedactEndpoint(endpoint))
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true, nil
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback(), nil
}
