package config

import "github.com/kjelly/hufu/internal/media"

// ResolveMediaPolicy intersects operator config layers before the team can
// further restrict them. Invalid restrictions never fall back to a wider grant.
func (c *Config) ResolveMediaPolicy() (*media.Policy, error) {
	if c == nil {
		return nil, nil
	}
	if c.mediaPolicyError != nil {
		return nil, c.mediaPolicyError
	}
	layers := c.mediaPolicies
	if len(layers) == 0 && c.Media != nil {
		layers = []*media.Policy{c.Media}
	}
	var result *media.Policy
	for index, p := range layers {
		if index == 0 {
			resolved, err := p.Resolve()
			if err != nil {
				return nil, err
			}
			result = &resolved
			continue
		}
		var err error
		result, err = media.Intersect(result, p)
		if err != nil {
			return nil, err
		}
	}
	if result != nil && !result.Enabled {
		return nil, nil
	}
	return result, nil
}
