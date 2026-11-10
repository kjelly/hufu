package media

import (
	"cmp"
	"math"
	"slices"
	"strings"
)

type Target struct {
	Backend              string `json:"backend" yaml:"backend"`
	ProviderKey          string `json:"provider_key" yaml:"provider-key"`
	Model                string `json:"model" yaml:"model"`
	ImageTokenUpperBound int64  `json:"image_token_upper_bound" yaml:"image-token-upper-bound"`
	IdentityHash         string `json:"identity_hash,omitempty" yaml:"-"`
}

// Policy is trusted configuration, never a model-supplied grant. A nil policy
// at the operator boundary disables media. A nil team policy adds no limits.
type Policy struct {
	Enabled        bool     `json:"enabled" yaml:"enabled"`
	Image          Limits   `json:"image" yaml:"image"`
	AllowedTargets []Target `json:"allowed_targets" yaml:"allowed-targets"`
}

func (p *Policy) Clone() *Policy {
	if p == nil {
		return nil
	}
	copy := *p
	copy.AllowedTargets = slices.Clone(p.AllowedTargets)
	return &copy
}

func (p Policy) Resolve() (Policy, error) {
	l, err := p.Image.Resolve()
	if err != nil {
		return Policy{}, err
	}
	p.Image = l
	p.AllowedTargets = slices.Clone(p.AllowedTargets)
	seen := make(map[[3]string]bool)
	for _, t := range p.AllowedTargets {
		key := [3]string{t.Backend, t.ProviderKey, t.Model}
		if !validTargetName(t.Backend) || (t.ProviderKey != "" && !validTargetName(t.ProviderKey)) || !validTargetName(t.Model) || t.ImageTokenUpperBound < 0 || t.ImageTokenUpperBound > math.MaxInt64/MaxCount || seen[key] {
			return Policy{}, Reject(EgressDenied, "invalid or duplicate exact media target")
		}
		seen[key] = true
	}
	slices.SortFunc(p.AllowedTargets, func(a, b Target) int {
		if c := cmp.Compare(a.Backend, b.Backend); c != 0 {
			return c
		}
		if c := cmp.Compare(a.ProviderKey, b.ProviderKey); c != 0 {
			return c
		}
		return cmp.Compare(a.Model, b.Model)
	})
	return p, nil
}

func validTargetName(s string) bool {
	return s != "" && len(s) <= 256 && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "*?\x00\r\n\t")
}

// Intersect cannot enable an operator-disabled policy or add a destination.
// Token estimates take the larger bound; lowering an estimate would weaken
// capacity admission. Empty-but-present target arrays deny every destination.
func Intersect(operator, team *Policy) (*Policy, error) {
	if operator == nil {
		if team != nil {
			if _, err := team.Resolve(); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	base, err := operator.Resolve()
	if err != nil {
		return nil, err
	}
	if team == nil {
		if !base.Enabled {
			return nil, nil
		}
		return &base, nil
	}
	restriction, err := team.Resolve()
	if err != nil {
		return nil, err
	}
	if !base.Enabled || !restriction.Enabled {
		return nil, nil
	}
	a, b := base.Image, restriction.Image
	base.Image = Limits{MaxCount: min(a.MaxCount, b.MaxCount), MaxBytesPerImage: min(a.MaxBytesPerImage, b.MaxBytesPerImage),
		MaxBytesPerBundle: min(a.MaxBytesPerBundle, b.MaxBytesPerBundle), MaxPixels: min(a.MaxPixels, b.MaxPixels),
		MaxSide: min(a.MaxSide, b.MaxSide), MaxBranchBytes: min(a.MaxBranchBytes, b.MaxBranchBytes), MaxBranchBlobs: min(a.MaxBranchBlobs, b.MaxBranchBlobs)}
	if team.AllowedTargets != nil {
		var targets []Target
		for _, t := range restriction.AllowedTargets {
			for _, allowed := range base.AllowedTargets {
				if t.Backend == allowed.Backend && t.ProviderKey == allowed.ProviderKey && t.Model == allowed.Model {
					allowed.ImageTokenUpperBound = max(t.ImageTokenUpperBound, allowed.ImageTokenUpperBound)
					targets = append(targets, allowed)
				}
			}
		}
		base.AllowedTargets = targets
	}
	return &base, nil
}

func (p *Policy) Allow(backend, provider, model string) (Target, error) {
	if p != nil && p.Enabled {
		for _, t := range p.AllowedTargets {
			if t.Backend == backend && t.ProviderKey == provider && t.Model == model {
				return t, nil
			}
		}
	}
	return Target{}, Reject(EgressDenied, "resolved target lacks an image grant")
}
