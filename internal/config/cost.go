package config

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/cost"
)

type CostConfig struct {
	Prices map[string]cost.PriceConfig `yaml:"prices"`
}

func (c *CostConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("cost must be a mapping")
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if key := node.Content[index].Value; key != "prices" {
			return fmt.Errorf("cost: unknown key %q (supported: prices)", key)
		}
	}
	type plain CostConfig
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return fmt.Errorf("cost: %w", err)
	}
	if _, err := cost.NewCatalog(decoded.Prices); err != nil {
		return err
	}
	*c = CostConfig(decoded)
	return nil
}

func (c CostConfig) Catalog() (cost.Catalog, error) {
	return cost.NewCatalog(c.Prices)
}

func (c *CostConfig) merge(next CostConfig) {
	if len(next.Prices) == 0 {
		return
	}
	if c.Prices == nil {
		c.Prices = make(map[string]cost.PriceConfig)
	}
	for target, price := range next.Prices {
		c.Prices[target] = price
	}
}
