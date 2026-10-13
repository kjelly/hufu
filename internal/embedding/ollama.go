package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Typed Ollama adapter failures. Context cancellation and deadlines are
// returned unchanged so callers never mistake them for a fallback.
var (
	ErrOllamaUnavailable     = errors.New("ollama endpoint unavailable")
	ErrModelNotFound         = errors.New("embedding model is not present in ollama")
	ErrEmbeddingInputTooLong = errors.New("embedding input exceeds the model context")
	ErrEmbeddingResponse     = errors.New("invalid ollama embedding response")
	ErrModelProfileMismatch  = errors.New("ollama model does not match the pinned encoder profile")
)

const (
	maxOllamaResponseBytes = 64 << 20
	maxOllamaBatch         = 256
)

// OllamaClient speaks the Ollama native API. The endpoint is the server root
// ("http://127.0.0.1:11434"); a trailing "/api" is accepted.
type OllamaClient struct {
	base *url.URL
	http *http.Client
}

// NewOllamaClient validates the endpoint. Requests are bounded by the caller's
// context, so the default client has no timeout of its own.
func NewOllamaClient(endpoint string, client *http.Client) (*OllamaClient, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("invalid ollama endpoint %q", RedactEndpoint(endpoint))
	}
	parsed.Path = strings.TrimSuffix(strings.TrimSuffix(parsed.Path, "/"), "/api")
	parsed.RawQuery, parsed.Fragment = "", ""
	if client == nil {
		client = &http.Client{}
	}
	return &OllamaClient{base: parsed, http: client}, nil
}

// RedactEndpoint drops user info, query and fragment from an endpoint so it is
// safe to show in errors and status output.
func RedactEndpoint(endpoint string) string {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Host == "" {
		return "<invalid>"
	}
	parsed.User, parsed.RawQuery, parsed.Fragment = nil, "", ""
	return parsed.String()
}

func (c *OllamaClient) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode ollama %s request: %w", path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	target := *c.base
	target.Path = strings.TrimSuffix(target.Path, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return fmt.Errorf("build ollama %s request: %w", path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("ollama %s: %w", path, ctxErr)
		}
		return fmt.Errorf("%w: %s", ErrOllamaUnavailable, path)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxOllamaResponseBytes+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("ollama %s: %w", path, ctxErr)
		}
		return fmt.Errorf("%w: read %s response", ErrOllamaUnavailable, path)
	}
	if len(data) > maxOllamaResponseBytes {
		return fmt.Errorf("%w: %s response is too large", ErrEmbeddingResponse, path)
	}
	if resp.StatusCode != http.StatusOK {
		return classifyOllamaStatus(path, resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%w: decode %s response", ErrEmbeddingResponse, path)
	}
	return nil
}

// classifyOllamaStatus maps a non-200 reply onto a typed error. Only fixed
// classifications are reported, never the server's free-form message.
func classifyOllamaStatus(path string, status int, body []byte) error {
	var reply struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &reply)
	message := strings.ToLower(reply.Error)
	switch {
	case strings.Contains(message, "exceeds the context length") || strings.Contains(message, "input length"):
		return fmt.Errorf("%w: %s returned %d", ErrEmbeddingInputTooLong, path, status)
	case status == http.StatusNotFound || strings.Contains(message, "not found"):
		return fmt.Errorf("%w: %s returned %d", ErrModelNotFound, path, status)
	case status >= 500:
		return fmt.Errorf("%w: %s returned %d", ErrOllamaUnavailable, path, status)
	default:
		return fmt.Errorf("%w: %s returned %d", ErrEmbeddingResponse, path, status)
	}
}

// OllamaModelInfo is what /api/tags and /api/show report about a local model.
type OllamaModelInfo struct {
	Name            string
	Digest          string
	Family          string
	Quantization    string
	ContextLength   int
	EmbeddingLength int
}

// ModelInfo looks the model up without pulling it. A model absent from
// /api/tags returns ErrModelNotFound; hufu never runs `ollama pull`.
func (c *OllamaClient) ModelInfo(ctx context.Context, model string) (OllamaModelInfo, error) {
	name := NormalizeOllamaModelName(model)
	var tags struct {
		Models []struct {
			Name    string `json:"name"`
			Model   string `json:"model"`
			Digest  string `json:"digest"`
			Details struct {
				Family            string `json:"family"`
				QuantizationLevel string `json:"quantization_level"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/tags", nil, &tags); err != nil {
		return OllamaModelInfo{}, err
	}
	info := OllamaModelInfo{}
	for _, m := range tags.Models {
		if NormalizeOllamaModelName(m.Name) == name || NormalizeOllamaModelName(m.Model) == name {
			info = OllamaModelInfo{Name: name, Digest: strings.ToLower(m.Digest), Family: m.Details.Family, Quantization: m.Details.QuantizationLevel}
			break
		}
	}
	if info.Name == "" {
		return OllamaModelInfo{}, fmt.Errorf("%w: %s", ErrModelNotFound, name)
	}
	if !validSHA256(info.Digest) {
		return OllamaModelInfo{}, fmt.Errorf("%w: %s has no full digest", ErrEmbeddingResponse, name)
	}
	var show struct {
		ModelInfo map[string]any `json:"model_info"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/show", map[string]string{"model": name}, &show); err != nil {
		return OllamaModelInfo{}, err
	}
	arch, _ := show.ModelInfo["general.architecture"].(string)
	info.ContextLength = jsonInt(show.ModelInfo[arch+".context_length"])
	info.EmbeddingLength = jsonInt(show.ModelInfo[arch+".embedding_length"])
	if arch == "" || info.ContextLength <= 0 || info.EmbeddingLength <= 0 {
		return OllamaModelInfo{}, fmt.Errorf("%w: %s lacks context or embedding length", ErrEmbeddingResponse, name)
	}
	return info, nil
}

func jsonInt(value any) int {
	number, ok := value.(float64)
	if !ok || number <= 0 || number > math.MaxInt32 || number != math.Trunc(number) {
		return 0
	}
	return int(number)
}

// ProfileSettings are the operator-controlled inputs to profile resolution.
type ProfileSettings struct {
	Model string
	// MaxInputTokens and QueryMaxTokens override the preset when positive.
	MaxInputTokens int
	QueryMaxTokens int
}

// ResolveProfile pins the local model digest and checks the preset against
// what Ollama reports (spec D4: the input limit may not exceed the model).
func ResolveProfile(ctx context.Context, client *OllamaClient, settings ProfileSettings) (EncoderProfile, QueryPolicy, error) {
	name := NormalizeOllamaModelName(settings.Model)
	preset, ok := PresetFor(name)
	if !ok {
		return EncoderProfile{}, QueryPolicy{}, fmt.Errorf("embedding model %q has no measured preset", name)
	}
	info, err := client.ModelInfo(ctx, name)
	if err != nil {
		return EncoderProfile{}, QueryPolicy{}, err
	}
	maxInput := preset.MaxInputTokens
	if settings.MaxInputTokens > 0 {
		maxInput = settings.MaxInputTokens
	}
	if maxInput > info.ContextLength {
		return EncoderProfile{}, QueryPolicy{}, fmt.Errorf("max input tokens %d exceed %s context length %d", maxInput, name, info.ContextLength)
	}
	queryMax := preset.QueryMaxTokens
	if settings.QueryMaxTokens > 0 {
		queryMax = settings.QueryMaxTokens
	}
	if queryMax > maxInput {
		return EncoderProfile{}, QueryPolicy{}, fmt.Errorf("query max tokens %d exceed max input tokens %d", queryMax, maxInput)
	}
	profile := EncoderProfile{
		SchemaVersion: encoderProfileSchemaVersion, Provider: "ollama", Model: name,
		ArtifactDigest: info.Digest, Family: info.Family, Quantization: info.Quantization,
		Dimensions: info.EmbeddingLength, DocumentPrefix: preset.DocumentPrefix,
		Normalization: normalizationL2, MaxInputTokens: maxInput, AdapterVersion: ollamaAdapterVersion,
	}
	query := QueryPolicy{
		Name: QueryPolicyGoalHead, Prefix: preset.QueryPrefix, PrefixTokens: preset.QueryPrefixTokens,
		MaxTokens: queryMax, Estimator: TokenEstimatorCJKOther,
	}
	if err := profile.Validate(); err != nil {
		return EncoderProfile{}, QueryPolicy{}, err
	}
	if err := query.Validate(); err != nil {
		return EncoderProfile{}, QueryPolicy{}, err
	}
	return profile, query, nil
}

// OllamaEmbedder embeds through /api/embed with a pinned profile. It is safe
// for concurrent use; concurrency limits belong to the caller.
type OllamaEmbedder struct {
	client    *OllamaClient
	profile   EncoderProfile
	query     QueryPolicy
	keepAlive string
}

// NewOllamaEmbedder binds a resolved profile. keepAlive is passed to Ollama
// on every request so a run does not lose the loaded model mid-way.
func NewOllamaEmbedder(client *OllamaClient, profile EncoderProfile, query QueryPolicy, keepAlive time.Duration) (*OllamaEmbedder, error) {
	if client == nil {
		return nil, errors.New("ollama client is required")
	}
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	if err := query.Validate(); err != nil {
		return nil, err
	}
	if keepAlive <= 0 {
		keepAlive = 10 * time.Minute
	}
	return &OllamaEmbedder{client: client, profile: profile, query: query, keepAlive: keepAlive.String()}, nil
}

func (e *OllamaEmbedder) Identity() ModelIdentity { return e.profile.Identity() }

// Profile returns the pinned document profile.
func (e *OllamaEmbedder) Profile() EncoderProfile { return e.profile }

// QueryPolicy returns the query shaping policy.
func (e *OllamaEmbedder) QueryPolicy() QueryPolicy { return e.query }

type embedRequest struct {
	Model     string         `json:"model"`
	Input     []string       `json:"input"`
	Truncate  bool           `json:"truncate"`
	KeepAlive string         `json:"keep_alive"`
	Options   map[string]int `json:"options"`
}

type embedResponse struct {
	Embeddings      [][]float64 `json:"embeddings"`
	PromptEvalCount int         `json:"prompt_eval_count"`
}

func (e *OllamaEmbedder) request(inputs []string) embedRequest {
	return embedRequest{
		Model: e.profile.Model, Input: inputs, Truncate: false, KeepAlive: e.keepAlive,
		Options: map[string]int{"num_ctx": e.profile.MaxInputTokens},
	}
}

// QueryEmbedding is a query vector plus the shaping that produced it.
type QueryEmbedding struct {
	Vector       []float32
	Shaped       ShapedQuery
	PromptTokens int
}

func (e *OllamaEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	result, err := e.EmbedQueryDetailed(ctx, text)
	return result.Vector, err
}

// EmbedQueryDetailed shapes the query with the policy (spec D3) and reports
// the actual prompt token count so estimator drift can be traced.
func (e *OllamaEmbedder) EmbedQueryDetailed(ctx context.Context, text string) (QueryEmbedding, error) {
	shaped := e.query.Shape(text)
	if shaped.Text == "" {
		return QueryEmbedding{}, ErrNoEmbeddableTokens
	}
	vectors, tokens, err := e.embed(ctx, []string{e.query.Prefix + shaped.Text})
	if err != nil {
		return QueryEmbedding{}, err
	}
	return QueryEmbedding{Vector: vectors[0], Shaped: shaped, PromptTokens: tokens}, nil
}

func (e *OllamaEmbedder) EmbedDocument(ctx context.Context, text string) ([]float32, error) {
	vectors, err := e.EmbedDocuments(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

// EmbedDocuments embeds verbatim document text with the profile's document
// prefix. Documents are never truncated; an oversize input fails the batch
// with ErrEmbeddingInputTooLong.
func (e *OllamaEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	if len(texts) > maxOllamaBatch {
		return nil, fmt.Errorf("embedding batch has %d documents, limit is %d", len(texts), maxOllamaBatch)
	}
	inputs := make([]string, len(texts))
	for i, text := range texts {
		if strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("embed document %d: %w", i, ErrNoEmbeddableTokens)
		}
		inputs[i] = e.profile.DocumentPrefix + text
	}
	vectors, _, err := e.embed(ctx, inputs)
	return vectors, err
}

func (e *OllamaEmbedder) embed(ctx context.Context, inputs []string) ([][]float32, int, error) {
	for i, input := range inputs {
		if len(input) > maxInputBytes {
			return nil, 0, fmt.Errorf("%w: input %d is larger than %d bytes", ErrEmbeddingInputTooLong, i, maxInputBytes)
		}
	}
	var resp embedResponse
	if err := e.client.do(ctx, http.MethodPost, "/api/embed", e.request(inputs), &resp); err != nil {
		return nil, 0, err
	}
	if len(resp.Embeddings) != len(inputs) {
		return nil, 0, fmt.Errorf("%w: got %d embeddings for %d inputs", ErrEmbeddingResponse, len(resp.Embeddings), len(inputs))
	}
	vectors := make([][]float32, len(inputs))
	for i, raw := range resp.Embeddings {
		if len(raw) != e.profile.Dimensions {
			return nil, 0, fmt.Errorf("%w: embedding %d has %d dimensions, want %d", ErrEmbeddingResponse, i, len(raw), e.profile.Dimensions)
		}
		vector := make([]float32, len(raw))
		for j, value := range raw {
			vector[j] = float32(value)
		}
		if err := normalizeL2(vector); err != nil {
			return nil, 0, fmt.Errorf("embedding %d: %w", i, err)
		}
		vectors[i] = vector
	}
	return vectors, resp.PromptEvalCount, nil
}

// Preload loads the model without computing a vector (an empty input array),
// with the same num_ctx as later requests so Ollama does not reload.
func (e *OllamaEmbedder) Preload(ctx context.Context) error {
	var resp embedResponse
	return e.client.do(ctx, http.MethodPost, "/api/embed", e.request([]string{}), &resp)
}

// VerifyLoaded checks /api/ps reports the pinned digest at the profile's
// context length. A mismatch means another client loaded a different model
// or context under the same name.
func (e *OllamaEmbedder) VerifyLoaded(ctx context.Context) error {
	var ps struct {
		Models []struct {
			Name          string `json:"name"`
			Model         string `json:"model"`
			Digest        string `json:"digest"`
			ContextLength int    `json:"context_length"`
		} `json:"models"`
	}
	if err := e.client.do(ctx, http.MethodGet, "/api/ps", nil, &ps); err != nil {
		return err
	}
	for _, m := range ps.Models {
		if NormalizeOllamaModelName(m.Name) != e.profile.Model && NormalizeOllamaModelName(m.Model) != e.profile.Model {
			continue
		}
		if strings.ToLower(m.Digest) != e.profile.ArtifactDigest {
			return fmt.Errorf("%w: loaded digest differs", ErrModelProfileMismatch)
		}
		if m.ContextLength != 0 && m.ContextLength != e.profile.MaxInputTokens {
			return fmt.Errorf("%w: loaded context length %d, want %d", ErrModelProfileMismatch, m.ContextLength, e.profile.MaxInputTokens)
		}
		return nil
	}
	return fmt.Errorf("%w: %s is not loaded", ErrModelProfileMismatch, e.profile.Model)
}

// CheckDigest confirms the tag still points at the pinned digest.
func (e *OllamaEmbedder) CheckDigest(ctx context.Context) error {
	info, err := e.client.ModelInfo(ctx, e.profile.Model)
	if err != nil {
		return err
	}
	if info.Digest != e.profile.ArtifactDigest {
		return fmt.Errorf("%w: %s now points at a different digest", ErrModelProfileMismatch, e.profile.Model)
	}
	return nil
}

var _ Embedder = (*OllamaEmbedder)(nil)
