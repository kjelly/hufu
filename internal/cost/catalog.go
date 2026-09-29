package cost

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/execution"
)

const (
	priceHashDomain             = "hufu-cost-price-v1\x00"
	catalogHashDomain           = "hufu-cost-catalog-v1\x00"
	unresolvedCatalogHashDomain = "hufu-cost-unresolved-catalog-v1\x00"
)

// PriceConfig is the authored hufu.yaml representation of one execution
// target's price. Monetary fields remain strings until validated.
type PriceConfig struct {
	BillingMode               BillingMode `yaml:"billing-mode"`
	InputUSDPerMillion        string      `yaml:"input-usd-per-million"`
	CacheReadUSDPerMillion    string      `yaml:"cache-read-usd-per-million"`
	CacheWriteUSDPerMillion   string      `yaml:"cache-write-usd-per-million"`
	OutputUSDPerMillion       string      `yaml:"output-usd-per-million"`
	OpaqueMaxUSDPerInvocation string      `yaml:"opaque-max-usd-per-invocation"`
}

// UnmarshalYAML rejects misspelled fields and YAML numeric scalars. Quoted
// decimal strings are required so binary floating point never enters money
// parsing.
func (p *PriceConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("cost price must be a mapping")
	}
	moneyKeys := map[string]bool{
		"input-usd-per-million": true, "cache-read-usd-per-million": true,
		"cache-write-usd-per-million": true, "output-usd-per-million": true,
		"opaque-max-usd-per-invocation": true,
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		key, value := node.Content[index].Value, node.Content[index+1]
		switch key {
		case "billing-mode", "input-usd-per-million", "cache-read-usd-per-million",
			"cache-write-usd-per-million", "output-usd-per-million", "opaque-max-usd-per-invocation":
		default:
			return fmt.Errorf("cost price: unknown key %q", key)
		}
		if moneyKeys[key] && value.Tag != "!!str" {
			return fmt.Errorf("cost price %s must be a quoted decimal string", key)
		}
	}
	type plain PriceConfig
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return fmt.Errorf("cost price: %w", err)
	}
	*p = PriceConfig(decoded)
	return nil
}

type Catalog struct {
	prices map[string]PriceSnapshot
	hash   string
}

func NewCatalog(configs map[string]PriceConfig) (Catalog, error) {
	prices := make(map[string]PriceSnapshot, len(configs))
	for _, target := range slices.Sorted(maps.Keys(configs)) {
		snapshot, err := compilePrice(target, configs[target])
		if err != nil {
			return Catalog{}, err
		}
		prices[target] = snapshot
	}
	ordered := make([]PriceSnapshot, 0, len(prices))
	for _, target := range slices.Sorted(maps.Keys(prices)) {
		ordered = append(ordered, prices[target])
	}
	hash, err := hashJSON(catalogHashDomain, struct {
		SchemaVersion int             `json:"schema_version"`
		Prices        []PriceSnapshot `json:"prices"`
	}{SchemaVersion: 1, Prices: ordered})
	if err != nil {
		return Catalog{}, err
	}
	for target, snapshot := range prices {
		snapshot.CatalogHash = hash
		prices[target] = snapshot
	}
	return Catalog{prices: prices, hash: hash}, nil
}

func compilePrice(target string, config PriceConfig) (PriceSnapshot, error) {
	selector, err := execution.ParseExecutionSelector(target)
	if err != nil || selector.Backend == "" || selector.Backend+"/"+selector.Model != target {
		return PriceSnapshot{}, fmt.Errorf("cost price target %q must be canonical and backend-qualified", target)
	}
	snapshot := PriceSnapshot{ExecutionTarget: target, BillingMode: config.BillingMode}
	fields := []struct {
		name  string
		value string
		dest  **int64
	}{
		{name: "input-usd-per-million", value: config.InputUSDPerMillion, dest: &snapshot.InputMicrosPerMillion},
		{name: "cache-read-usd-per-million", value: config.CacheReadUSDPerMillion, dest: &snapshot.CacheReadMicrosPerMillion},
		{name: "cache-write-usd-per-million", value: config.CacheWriteUSDPerMillion, dest: &snapshot.CacheWriteMicrosPerMillion},
		{name: "output-usd-per-million", value: config.OutputUSDPerMillion, dest: &snapshot.OutputMicrosPerMillion},
		{name: "opaque-max-usd-per-invocation", value: config.OpaqueMaxUSDPerInvocation, dest: &snapshot.OpaqueMaxMicrosPerInvocation},
	}
	for _, field := range fields {
		if field.value == "" {
			continue
		}
		micros, parseErr := ParseUSDMicros(field.value)
		if parseErr != nil {
			return PriceSnapshot{}, fmt.Errorf("cost price %q %s: %w", target, field.name, parseErr)
		}
		*field.dest = new(micros)
	}
	hasInput := snapshot.InputMicrosPerMillion != nil
	hasOutput := snapshot.OutputMicrosPerMillion != nil
	hasTokenRates := hasInput || hasOutput || snapshot.CacheReadMicrosPerMillion != nil || snapshot.CacheWriteMicrosPerMillion != nil
	switch snapshot.BillingMode {
	case BillingMetered:
		if hasInput != hasOutput {
			return PriceSnapshot{}, fmt.Errorf("cost price %q: metered token pricing requires both input and output rates", target)
		}
		if (snapshot.CacheReadMicrosPerMillion != nil || snapshot.CacheWriteMicrosPerMillion != nil) && !hasInput {
			return PriceSnapshot{}, fmt.Errorf("cost price %q: cache rates require input and output rates", target)
		}
		if !hasInput && snapshot.OpaqueMaxMicrosPerInvocation == nil {
			return PriceSnapshot{}, fmt.Errorf("cost price %q: metered pricing requires token rates or an opaque maximum", target)
		}
		if hasInput {
			if snapshot.CacheReadMicrosPerMillion == nil {
				snapshot.CacheReadMicrosPerMillion = new(*snapshot.InputMicrosPerMillion)
			}
			if snapshot.CacheWriteMicrosPerMillion == nil {
				snapshot.CacheWriteMicrosPerMillion = new(*snapshot.InputMicrosPerMillion)
			}
		}
	case BillingLocal, BillingSubscription:
		if hasTokenRates || snapshot.OpaqueMaxMicrosPerInvocation != nil {
			return PriceSnapshot{}, fmt.Errorf("cost price %q: %s billing must not define monetary rates", target, snapshot.BillingMode)
		}
	case BillingUnknown:
		return PriceSnapshot{}, fmt.Errorf("cost price %q: unknown billing mode is runtime-derived and cannot be configured", target)
	default:
		return PriceSnapshot{}, fmt.Errorf("cost price %q: unsupported billing-mode %q", target, snapshot.BillingMode)
	}
	snapshot.ID, err = priceSnapshotID(snapshot)
	if err != nil {
		return PriceSnapshot{}, err
	}
	return snapshot, nil
}

func priceSnapshotID(snapshot PriceSnapshot) (string, error) {
	return hashJSON(priceHashDomain, struct {
		ExecutionTarget              string      `json:"execution_target"`
		BillingMode                  BillingMode `json:"billing_mode"`
		InputMicrosPerMillion        *int64      `json:"input_micros_per_million,omitempty"`
		CacheReadMicrosPerMillion    *int64      `json:"cache_read_micros_per_million,omitempty"`
		CacheWriteMicrosPerMillion   *int64      `json:"cache_write_micros_per_million,omitempty"`
		OutputMicrosPerMillion       *int64      `json:"output_micros_per_million,omitempty"`
		OpaqueMaxMicrosPerInvocation *int64      `json:"opaque_max_micros_per_invocation,omitempty"`
	}{
		ExecutionTarget: snapshot.ExecutionTarget, BillingMode: snapshot.BillingMode,
		InputMicrosPerMillion: snapshot.InputMicrosPerMillion, CacheReadMicrosPerMillion: snapshot.CacheReadMicrosPerMillion,
		CacheWriteMicrosPerMillion: snapshot.CacheWriteMicrosPerMillion, OutputMicrosPerMillion: snapshot.OutputMicrosPerMillion,
		OpaqueMaxMicrosPerInvocation: snapshot.OpaqueMaxMicrosPerInvocation,
	})
}

// NewUnknownPriceSnapshot returns the deterministic runtime representation of
// a canonical target that has no authored catalog entry. Its catalog hash is
// explicitly an unresolved-target digest, not the identity of an authored
// catalog.
func NewUnknownPriceSnapshot(target string) (PriceSnapshot, error) {
	selector, err := execution.ParseExecutionSelector(target)
	if err != nil || selector.Backend == "" || selector.Backend+"/"+selector.Model != target {
		return PriceSnapshot{}, fmt.Errorf("unknown cost target %q must be canonical and backend-qualified", target)
	}
	snapshot := PriceSnapshot{ExecutionTarget: target, BillingMode: BillingUnknown}
	snapshot.ID, err = priceSnapshotID(snapshot)
	if err != nil {
		return PriceSnapshot{}, err
	}
	snapshot.CatalogHash, err = hashJSON(unresolvedCatalogHashDomain, struct {
		ExecutionTarget string `json:"execution_target"`
	}{ExecutionTarget: target})
	if err != nil {
		return PriceSnapshot{}, err
	}
	return snapshot, nil
}

// ValidatePriceSnapshot verifies a resolved snapshot without consulting live
// configuration. It accepts runtime-derived unknown snapshots but authored
// catalogs still reject BillingUnknown in compilePrice.
func ValidatePriceSnapshot(snapshot PriceSnapshot) error {
	if strings.TrimSpace(snapshot.ID) == "" || strings.TrimSpace(snapshot.ExecutionTarget) == "" || strings.TrimSpace(snapshot.CatalogHash) == "" {
		return fmt.Errorf("cost price snapshot is incomplete")
	}
	selector, err := execution.ParseExecutionSelector(snapshot.ExecutionTarget)
	if err != nil || selector.Backend == "" || selector.Backend+"/"+selector.Model != snapshot.ExecutionTarget {
		return fmt.Errorf("cost price snapshot target %q is not canonical", snapshot.ExecutionTarget)
	}
	switch snapshot.BillingMode {
	case BillingMetered:
		if hasTokenPrice(snapshot) {
			for _, value := range []*int64{snapshot.InputMicrosPerMillion, snapshot.CacheReadMicrosPerMillion, snapshot.CacheWriteMicrosPerMillion, snapshot.OutputMicrosPerMillion} {
				if value == nil || *value < 0 {
					return fmt.Errorf("cost price snapshot has invalid token rate")
				}
			}
		} else if snapshot.OpaqueMaxMicrosPerInvocation == nil || *snapshot.OpaqueMaxMicrosPerInvocation < 0 {
			return fmt.Errorf("cost price snapshot metered price is unbounded")
		}
	case BillingLocal, BillingSubscription, BillingUnknown:
		if hasAnyPriceField(snapshot) {
			return fmt.Errorf("cost price snapshot %s mode must not define rates", snapshot.BillingMode)
		}
	default:
		return fmt.Errorf("cost price snapshot has unsupported billing mode %q", snapshot.BillingMode)
	}
	wantID, err := priceSnapshotID(snapshot)
	if err != nil {
		return err
	}
	if snapshot.ID != wantID {
		return fmt.Errorf("cost price snapshot ID does not match its contents")
	}
	return nil
}

func hasAnyPriceField(snapshot PriceSnapshot) bool {
	return snapshot.InputMicrosPerMillion != nil || snapshot.CacheReadMicrosPerMillion != nil || snapshot.CacheWriteMicrosPerMillion != nil || snapshot.OutputMicrosPerMillion != nil || snapshot.OpaqueMaxMicrosPerInvocation != nil
}

func hashJSON(domain string, value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal cost hash input: %w", err)
	}
	digest := sha256.Sum256(append([]byte(domain), encoded...))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func (c Catalog) Hash() string { return c.hash }

func (c Catalog) Len() int { return len(c.prices) }

func (c Catalog) Resolve(target string) (PriceSnapshot, bool) {
	snapshot, ok := c.prices[target]
	return clonePriceSnapshot(snapshot), ok
}

func (c Catalog) Snapshots() []PriceSnapshot {
	result := make([]PriceSnapshot, 0, len(c.prices))
	for _, target := range slices.Sorted(maps.Keys(c.prices)) {
		result = append(result, clonePriceSnapshot(c.prices[target]))
	}
	return result
}

func clonePriceSnapshot(snapshot PriceSnapshot) PriceSnapshot {
	for _, pair := range []struct{ source **int64 }{
		{source: &snapshot.InputMicrosPerMillion}, {source: &snapshot.CacheReadMicrosPerMillion},
		{source: &snapshot.CacheWriteMicrosPerMillion}, {source: &snapshot.OutputMicrosPerMillion},
		{source: &snapshot.OpaqueMaxMicrosPerInvocation},
	} {
		if *pair.source != nil {
			*pair.source = new(**pair.source)
		}
	}
	return snapshot
}
