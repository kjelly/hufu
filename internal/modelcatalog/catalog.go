// Package modelcatalog provides the offline model metadata catalog used by
// hufu. The package has no provider or coordinator dependencies: callers can
// load a validated snapshot or explicitly request an update.
package modelcatalog

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	DefaultSourceURL = "https://models.dev/api.json"
	OriginCache      = "cache"
	OriginEmbedded   = "embedded"
)

// CatalogModel is the normalized subset of models.dev metadata consumed by
// hufu. Pointer booleans deliberately preserve the distinction between an
// explicit false and an absent catalog field.
type CatalogModel struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Family   string `json:"family,omitempty"`

	Attachment  *bool `json:"attachment,omitempty"`
	Reasoning   *bool `json:"reasoning,omitempty"`
	ToolCall    *bool `json:"tool_call,omitempty"`
	Temperature *bool `json:"temperature,omitempty"`

	Context int `json:"context,omitzero"`
	Output  int `json:"output,omitzero"`

	// Estimator is a family-derived tokenizer hint only. It is never presented
	// as a tokenizer claim from models.dev.
	Estimator           string `json:"estimator,omitempty"`
	EstimatorProvenance string `json:"estimator_provenance,omitempty"`
}

// LookupKey is the exact normalized identity of a catalog model.
type LookupKey struct {
	Provider string
	ID       string
}

// LookupResult retains the key and source for diagnostic callers.
type LookupResult struct {
	Key    LookupKey
	Model  CatalogModel
	Source string
	Found  bool
}

// Reader is the narrow catalog dependency injected into profile resolution.
type Reader interface {
	Lookup(provider, modelID string) (LookupResult, bool)
}

// Catalog is an immutable-in-use normalized snapshot. Store and Update return
// copies with independent maps; readers should not mutate Models.
type Catalog struct {
	Version string
	Models  map[LookupKey]CatalogModel
}

type catalogFile struct {
	Version string         `json:"version"`
	Models  []CatalogModel `json:"models"`
}

// NewCatalog constructs a validated catalog from normalized models.
func NewCatalog(version string, models []CatalogModel) (Catalog, error) {
	result := Catalog{Version: strings.TrimSpace(version), Models: make(map[LookupKey]CatalogModel, len(models))}
	for _, model := range models {
		normalized, err := normalizeModel(model)
		if err != nil {
			return Catalog{}, err
		}
		key := LookupKey{Provider: normalized.Provider, ID: normalized.ID}
		if _, exists := result.Models[key]; exists {
			return Catalog{}, fmt.Errorf("duplicate catalog model %s/%s", key.Provider, key.ID)
		}
		result.Models[key] = normalized
	}
	if len(result.Models) == 0 {
		return Catalog{}, errors.New("catalog requires at least one model")
	}
	return result, nil
}

// Lookup performs only exact normalized provider/model lookup. It never
// performs family, prefix, or substring matching.
func (c Catalog) Lookup(provider, modelID string) (LookupResult, bool) {
	key := NormalizeLookupKey(provider, modelID)
	model, ok := c.Models[key]
	return LookupResult{Key: key, Model: model, Found: ok}, ok
}

// NormalizeLookupKey normalizes case and whitespace and removes only a
// provider qualifier that exactly matches provider. A slash inside a model ID
// remains part of the exact ID.
func NormalizeLookupKey(provider, modelID string) LookupKey {
	provider = normalizePart(provider)
	modelID = normalizePart(modelID)
	if qualifier, remainder, ok := strings.Cut(modelID, "/"); ok && qualifier == provider {
		modelID = remainder
	}
	return LookupKey{Provider: provider, ID: modelID}
}

// Models returns models in deterministic order for diagnostics and tests.
func (c Catalog) ModelsList() []CatalogModel {
	models := make([]CatalogModel, 0, len(c.Models))
	for _, model := range c.Models {
		models = append(models, model)
	}
	slices.SortFunc(models, func(left, right CatalogModel) int {
		return cmp.Or(strings.Compare(left.Provider, right.Provider), strings.Compare(left.ID, right.ID))
	})
	return models
}

func (c Catalog) marshal() ([]byte, error) {
	return json.Marshal(catalogFile{Version: c.Version, Models: c.ModelsList()})
}

func parseCatalogFile(data []byte) (Catalog, error) {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return Catalog{}, err
	}
	var file catalogFile
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&file); err != nil {
		return Catalog{}, fmt.Errorf("decode catalog snapshot: %w", err)
	}
	if strings.TrimSpace(file.Version) == "" {
		return Catalog{}, errors.New("catalog snapshot version is missing")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Catalog{}, errors.New("catalog snapshot contains trailing JSON")
		}
		return Catalog{}, fmt.Errorf("catalog snapshot has invalid trailing JSON: %w", err)
	}
	return NewCatalog(file.Version, file.Models)
}

// ParseJSON parses either a normalized hufu snapshot or the provider-grouped
// models.dev response and returns a normalized catalog.
func ParseJSON(data []byte) (Catalog, error) {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return Catalog{}, err
	}
	var raw any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return Catalog{}, fmt.Errorf("decode models.dev response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Catalog{}, errors.New("models.dev response contains trailing JSON")
		}
		return Catalog{}, fmt.Errorf("models.dev response has invalid trailing JSON: %w", err)
	}
	return normalizeValue(raw)
}

func normalizeValue(raw any) (Catalog, error) {
	root, ok := raw.(map[string]any)
	if !ok {
		return Catalog{}, errors.New("models.dev response must be a JSON object")
	}
	version, _ := root["version"].(string)
	models := make([]CatalogModel, 0)
	if rawModels, ok := root["models"]; ok {
		if err := appendModelCollection(&models, "", rawModels); err != nil {
			return Catalog{}, err
		}
	} else if rawData, ok := root["data"]; ok {
		if err := appendModelCollection(&models, "", rawData); err != nil {
			return Catalog{}, err
		}
	} else {
		for provider, rawProvider := range root {
			if provider == "$schema" || provider == "version" || provider == "updated_at" {
				continue
			}
			providerObject, ok := rawProvider.(map[string]any)
			if !ok {
				return Catalog{}, fmt.Errorf("provider %q is not an object", provider)
			}
			collection, ok := providerObject["models"]
			if !ok {
				continue
			}
			if err := appendModelCollection(&models, provider, collection); err != nil {
				return Catalog{}, err
			}
		}
	}
	if version == "" {
		version = "models.dev"
	}
	return NewCatalog(version, models)
}

func appendModelCollection(models *[]CatalogModel, provider string, raw any) error {
	switch collection := raw.(type) {
	case map[string]any:
		for id, value := range collection {
			object, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("model %q is not an object", id)
			}
			model, err := normalizeObject(provider, id, object)
			if err != nil {
				return err
			}
			*models = append(*models, model)
		}
	case []any:
		for _, value := range collection {
			object, ok := value.(map[string]any)
			if !ok {
				return errors.New("model collection contains a non-object")
			}
			model, err := normalizeObject(provider, "", object)
			if err != nil {
				return err
			}
			*models = append(*models, model)
		}
	default:
		return errors.New("model collection is neither an object nor a list")
	}
	return nil
}

func normalizeObject(provider, mapID string, object map[string]any) (CatalogModel, error) {
	model := CatalogModel{Provider: provider}
	if value, ok := object["provider"].(string); ok && strings.TrimSpace(value) != "" {
		model.Provider = value
	}
	model.ID = mapID
	if value, ok := object["id"].(string); ok && strings.TrimSpace(value) != "" {
		model.ID = value
	}
	if value, ok := object["name"].(string); ok {
		model.Name = value
	}
	if value, ok := object["family"].(string); ok {
		model.Family = value
	}
	var err error
	model.Attachment, err = optionalBool(object, "attachment")
	if err != nil {
		return CatalogModel{}, err
	}
	model.Reasoning, err = optionalBool(object, "reasoning")
	if err != nil {
		return CatalogModel{}, err
	}
	model.ToolCall, err = optionalBool(object, "tool_call")
	if err != nil {
		return CatalogModel{}, err
	}
	model.Temperature, err = optionalBool(object, "temperature")
	if err != nil {
		return CatalogModel{}, err
	}
	model.Context, err = firstPositiveInt(object, "context_length", "max_context_window", "max_input_tokens")
	if err != nil {
		return CatalogModel{}, err
	}
	model.Output, err = firstPositiveInt(object, "max_output_tokens", "max_completion_tokens")
	if err != nil {
		return CatalogModel{}, err
	}
	if limit, ok := object["limit"].(map[string]any); ok {
		model.Context, err = overridePositiveInt(limit, model.Context, "context")
		if err != nil {
			return CatalogModel{}, err
		}
		model.Output, err = overridePositiveInt(limit, model.Output, "output")
		if err != nil {
			return CatalogModel{}, err
		}
	} else if object["limit"] != nil {
		return CatalogModel{}, errors.New("model limit is not an object")
	}
	if model.Family != "" {
		model.Estimator = familyEstimator(model.Family)
		if model.Estimator != "" {
			model.EstimatorProvenance = "catalog_family_derived"
		}
	}
	return normalizeModel(model)
}

func normalizeModel(model CatalogModel) (CatalogModel, error) {
	model.Provider = normalizePart(model.Provider)
	model.ID = normalizePart(model.ID)
	if model.Provider == "" || model.ID == "" {
		return CatalogModel{}, errors.New("catalog model requires provider and id")
	}
	model.Name = strings.TrimSpace(model.Name)
	model.Family = strings.TrimSpace(model.Family)
	if model.Estimator == "" && model.Family != "" {
		model.Estimator = familyEstimator(model.Family)
	}
	if model.Estimator != "" && model.EstimatorProvenance == "" {
		model.EstimatorProvenance = "catalog_family_derived"
	}
	if model.Context < 0 || model.Output < 0 {
		return CatalogModel{}, fmt.Errorf("catalog model %s/%s has a negative limit", model.Provider, model.ID)
	}
	return model, nil
}

func normalizePart(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

func optionalBool(object map[string]any, key string) (*bool, error) {
	value, present := object[key]
	if !present || value == nil {
		return nil, nil
	}
	boolean, ok := value.(bool)
	if !ok {
		return nil, fmt.Errorf("catalog field %q is not a boolean", key)
	}
	return new(boolean), nil
}

func firstPositiveInt(object map[string]any, keys ...string) (int, error) {
	for _, key := range keys {
		if value, present := object[key]; present {
			return parsePositiveInt(value, key)
		}
	}
	return 0, nil
}

func overridePositiveInt(object map[string]any, current int, key string) (int, error) {
	value, present := object[key]
	if !present {
		return current, nil
	}
	if value == nil {
		return 0, nil
	}
	return parseOptionalPositiveInt(value, "limit."+key)
}

func parsePositiveInt(value any, key string) (int, error) {
	return parseInt(value, key, false)
}

func parseOptionalPositiveInt(value any, key string) (int, error) {
	return parseInt(value, key, true)
}

func parseInt(value any, key string, allowZero bool) (int, error) {
	positive := func(parsed int64) (int, error) {
		if parsed < 0 || (!allowZero && parsed == 0) || int64(int(parsed)) != parsed {
			if allowZero {
				return 0, fmt.Errorf("catalog field %q is not a non-negative integer", key)
			}
			return 0, fmt.Errorf("catalog field %q is not positive", key)
		}
		return int(parsed), nil
	}
	positiveFloat := func(parsed float64) (int, error) {
		if parsed < 0 || (!allowZero && parsed == 0) || parsed != float64(int64(parsed)) || int64(int(parsed)) != int64(parsed) {
			if allowZero {
				return 0, fmt.Errorf("catalog field %q is not a non-negative integer", key)
			}
			return 0, fmt.Errorf("catalog field %q is not a positive integer", key)
		}
		return int(parsed), nil
	}

	switch typed := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseInt(string(typed), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("catalog field %q is not an integer", key)
		}
		return positive(parsed)
	case float64:
		return positiveFloat(typed)
	case int:
		if typed < 0 || (!allowZero && typed == 0) {
			if allowZero {
				return 0, fmt.Errorf("catalog field %q is not a non-negative integer", key)
			}
			return 0, fmt.Errorf("catalog field %q is not positive", key)
		}
		return typed, nil
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err != nil {
			if allowZero {
				return 0, fmt.Errorf("catalog field %q is not a non-negative integer", key)
			}
			return 0, fmt.Errorf("catalog field %q is not a positive integer", key)
		}
		return positive(parsed)
	default:
		return 0, fmt.Errorf("catalog field %q is not an integer", key)
	}
}

func familyEstimator(family string) string {
	family = strings.ToLower(strings.TrimSpace(family))
	for _, candidate := range []string{"claude", "gemma", "gpt", "llama", "qwen", "mistral", "mixtral"} {
		if strings.Contains(family, candidate) {
			return candidate
		}
	}
	return ""
}

func DefaultPath() string {
	directory, err := os.UserCacheDir()
	if err != nil || directory == "" {
		return filepath.Join(".", "hufu", "models.json")
	}
	return filepath.Join(directory, "hufu", "models.json")
}
