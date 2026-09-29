package cost

import (
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

type UnknownPricePolicy string

const (
	UnknownPriceAllow UnknownPricePolicy = "allow"
	UnknownPriceDeny  UnknownPricePolicy = "deny"
)

// PolicyConfig is the authored team.yaml cost block. Pointer ownership lives
// in the team manifest so an omitted block remains distinguishable from an
// explicitly configured empty policy.
type PolicyConfig struct {
	MaxRunUSD          string             `yaml:"max-run-usd,omitempty"`
	WarningRunUSD      string             `yaml:"warning-run-usd,omitempty"`
	UnknownPricePolicy UnknownPricePolicy `yaml:"unknown-price-policy,omitempty"`
}

func (p *PolicyConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("cost policy must be a mapping")
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		key, value := node.Content[index].Value, node.Content[index+1]
		switch key {
		case "max-run-usd", "warning-run-usd":
			if value.Tag != "!!str" {
				return fmt.Errorf("cost policy %s must be a quoted decimal string", key)
			}
		case "unknown-price-policy":
		default:
			return fmt.Errorf("cost policy: unknown key %q", key)
		}
	}
	type plain PolicyConfig
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return fmt.Errorf("cost policy: %w", err)
	}
	if _, err := ResolveRunPolicy((*PolicyConfig)(&decoded)); err != nil {
		return err
	}
	*p = PolicyConfig(decoded)
	return nil
}

type RunPolicy struct {
	Configured         bool
	MaxRunMicros       *int64
	WarningRunMicros   *int64
	UnknownPricePolicy UnknownPricePolicy
}

func ResolveRunPolicy(config *PolicyConfig) (RunPolicy, error) {
	policy := RunPolicy{Configured: config != nil, UnknownPricePolicy: UnknownPriceAllow}
	if config == nil {
		return policy, nil
	}
	if config.MaxRunUSD != "" {
		value, err := ParseUSDMicros(config.MaxRunUSD)
		if err != nil || value <= 0 {
			return RunPolicy{}, fmt.Errorf("cost policy max-run-usd must be a positive decimal string")
		}
		policy.MaxRunMicros = new(value)
	}
	if config.WarningRunUSD != "" {
		value, err := ParseUSDMicros(config.WarningRunUSD)
		if err != nil || value <= 0 {
			return RunPolicy{}, fmt.Errorf("cost policy warning-run-usd must be a positive decimal string")
		}
		policy.WarningRunMicros = new(value)
	}
	if config.UnknownPricePolicy != "" {
		policy.UnknownPricePolicy = config.UnknownPricePolicy
	} else if policy.MaxRunMicros != nil {
		policy.UnknownPricePolicy = UnknownPriceDeny
	}
	if policy.UnknownPricePolicy != UnknownPriceAllow && policy.UnknownPricePolicy != UnknownPriceDeny {
		return RunPolicy{}, fmt.Errorf("cost policy unknown-price-policy %q is unsupported", policy.UnknownPricePolicy)
	}
	if policy.MaxRunMicros != nil && config.UnknownPricePolicy == UnknownPriceAllow {
		return RunPolicy{}, fmt.Errorf("cost policy max-run-usd requires unknown-price-policy deny")
	}
	if policy.WarningRunMicros != nil && policy.MaxRunMicros != nil && *policy.WarningRunMicros > *policy.MaxRunMicros {
		return RunPolicy{}, fmt.Errorf("cost policy warning-run-usd must not exceed max-run-usd")
	}
	return policy, nil
}

type PolicySnapshot struct {
	MaxRunMicros       *int64             `json:"max_run_micros,omitempty"`
	WarningRunMicros   *int64             `json:"warning_run_micros,omitempty"`
	UnknownPricePolicy UnknownPricePolicy `json:"unknown_price_policy"`
	Prices             []PriceSnapshot    `json:"prices,omitempty"`
	PolicyHash         string             `json:"policy_hash"`
}

func NewPolicySnapshot(policy RunPolicy, prices []PriceSnapshot) (*PolicySnapshot, error) {
	if !policy.Configured && len(prices) == 0 {
		return nil, nil
	}
	snapshot := &PolicySnapshot{
		MaxRunMicros: cloneInt64(policy.MaxRunMicros), WarningRunMicros: cloneInt64(policy.WarningRunMicros),
		UnknownPricePolicy: policy.UnknownPricePolicy, Prices: make([]PriceSnapshot, len(prices)),
	}
	if snapshot.UnknownPricePolicy == "" {
		snapshot.UnknownPricePolicy = UnknownPriceAllow
	}
	for index, price := range prices {
		snapshot.Prices[index] = clonePriceSnapshot(price)
	}
	slices.SortFunc(snapshot.Prices, func(left, right PriceSnapshot) int {
		return strings.Compare(left.ExecutionTarget, right.ExecutionTarget)
	})
	hash, err := policySnapshotHash(snapshot)
	if err != nil {
		return nil, err
	}
	snapshot.PolicyHash = hash
	if err := ValidatePolicySnapshot(snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func ValidatePolicySnapshot(snapshot *PolicySnapshot) error {
	if snapshot == nil {
		return nil
	}
	if snapshot.UnknownPricePolicy != UnknownPriceAllow && snapshot.UnknownPricePolicy != UnknownPriceDeny {
		return fmt.Errorf("cost policy snapshot has unsupported unknown-price policy %q", snapshot.UnknownPricePolicy)
	}
	if snapshot.MaxRunMicros != nil && *snapshot.MaxRunMicros <= 0 {
		return fmt.Errorf("cost policy snapshot max must be positive")
	}
	if snapshot.MaxRunMicros != nil && snapshot.UnknownPricePolicy != UnknownPriceDeny {
		return fmt.Errorf("cost policy snapshot hard max requires unknown-price deny")
	}
	if snapshot.WarningRunMicros != nil && *snapshot.WarningRunMicros <= 0 {
		return fmt.Errorf("cost policy snapshot warning must be positive")
	}
	if snapshot.MaxRunMicros != nil && snapshot.WarningRunMicros != nil && *snapshot.WarningRunMicros > *snapshot.MaxRunMicros {
		return fmt.Errorf("cost policy snapshot warning exceeds max")
	}
	for index, price := range snapshot.Prices {
		if strings.TrimSpace(price.ID) == "" || strings.TrimSpace(price.ExecutionTarget) == "" || strings.TrimSpace(price.CatalogHash) == "" {
			return fmt.Errorf("cost policy snapshot price is incomplete")
		}
		if index > 0 && snapshot.Prices[index-1].ExecutionTarget >= price.ExecutionTarget {
			return fmt.Errorf("cost policy snapshot prices are not in strict target order")
		}
	}
	want, err := policySnapshotHash(snapshot)
	if err != nil {
		return err
	}
	if snapshot.PolicyHash != want {
		return fmt.Errorf("cost policy snapshot hash does not match its contents")
	}
	return nil
}

func ClonePolicySnapshot(snapshot *PolicySnapshot) *PolicySnapshot {
	if snapshot == nil {
		return nil
	}
	clone := *snapshot
	clone.MaxRunMicros = cloneInt64(snapshot.MaxRunMicros)
	clone.WarningRunMicros = cloneInt64(snapshot.WarningRunMicros)
	clone.Prices = make([]PriceSnapshot, len(snapshot.Prices))
	for index, price := range snapshot.Prices {
		clone.Prices[index] = clonePriceSnapshot(price)
	}
	return &clone
}

func policySnapshotHash(snapshot *PolicySnapshot) (string, error) {
	clone := ClonePolicySnapshot(snapshot)
	clone.PolicyHash = ""
	return hashJSON("hufu-cost-policy-v1\x00", clone)
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	return new(*value)
}
