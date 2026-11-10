package team

import (
	"fmt"
	"reflect"

	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/media"
)

type MediaTaskBinding = media.TaskBinding

type MediaPolicySnapshot struct {
	Version int          `json:"version"`
	Policy  media.Policy `json:"policy"`
	Hash    string       `json:"hash"`
}

func (p *MediaPolicySnapshot) clone() *MediaPolicySnapshot {
	if p == nil {
		return nil
	}
	copy := *p
	copy.Policy = *p.Policy.Clone()
	return &copy
}

func (p MediaPolicySnapshot) digest() (string, error) { p.Hash = ""; return media.Digest(p) }

// Media must be resolved from the complete frozen target catalog before the
// enclosing policy hash is sealed. Text-only snapshots retain their old hash.
func (c *Coordinator) mediaExecutionPolicyConfigurationHash(snapshot *ExecutionPolicySnapshot) (string, error) {
	var err error
	snapshot.Media, err = c.resolveMediaExecutionPolicy(snapshot)
	if err != nil {
		return "", err
	}
	return executionPolicyConfigurationHash(snapshot)
}

func (c *Coordinator) resolveMediaExecutionPolicy(executionPolicy *ExecutionPolicySnapshot) (*MediaPolicySnapshot, error) {
	policy, err := media.Intersect(c.session.MediaPolicy, c.session.Config.Media)
	if err != nil || policy == nil {
		return nil, err
	}
	if executionPolicy.Version < executionPolicySnapshotVersion {
		return nil, media.Reject(media.EgressDenied, "legacy execution policy cannot enable media")
	}
	for index, target := range policy.AllowedTargets {
		for _, backend := range executionPolicy.Backends {
			if target.Backend == backend.Backend && target.ProviderKey == backend.ProviderKey {
				policy.AllowedTargets[index].IdentityHash = backend.IdentityHash
			}
		}
	}
	snapshot := &MediaPolicySnapshot{Version: media.Version, Policy: *policy}
	snapshot.Hash, err = snapshot.digest()
	if err != nil {
		return nil, err
	}
	if err := validateMediaPolicySnapshot(snapshot, executionPolicy); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func validateMediaPolicySnapshot(p *MediaPolicySnapshot, executionPolicy *ExecutionPolicySnapshot) error {
	if p == nil {
		return nil
	}
	if p.Version != media.Version || !p.Policy.Enabled || executionPolicy.Version < executionPolicySnapshotVersion {
		return media.Reject(media.EgressDenied, "invalid frozen media policy")
	}
	resolved, err := p.Policy.Resolve()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(resolved, p.Policy) {
		return media.Reject(media.EgressDenied, "media policy is not canonical")
	}
	for _, target := range p.Policy.AllowedTargets {
		knownBackend, knownModel := false, false
		for _, backend := range executionPolicy.Backends {
			if target.Backend == backend.Backend && target.ProviderKey == backend.ProviderKey && target.IdentityHash != "" && target.IdentityHash == backend.IdentityHash {
				knownBackend = true
			}
		}
		for _, route := range executionPolicy.ModelRoutes {
			selector, err := execution.ParseExecutionSelector(route.Model)
			if err != nil {
				continue
			}
			if route.Backend == target.Backend && route.ProviderKey == target.ProviderKey && selector.Model == target.Model {
				knownModel = true
			}
		}
		if !knownBackend || !knownModel {
			return media.Reject(media.EgressDenied, "media target is not a frozen backend/provider/model")
		}
	}
	hash, err := p.digest()
	if err != nil {
		return err
	}
	if hash != p.Hash {
		return media.Reject(media.IntegrityMismatch, "media policy hash mismatch")
	}
	return nil
}

func (c *Coordinator) requiredMediaPolicy() (*MediaPolicySnapshot, error) {
	if c == nil || c.executionPolicy == nil || c.executionPolicy.snapshot == nil || c.executionPolicy.snapshot.Media == nil {
		return nil, media.Reject(media.EgressDenied, "media is disabled by execution policy")
	}
	p := c.executionPolicy.snapshot.Media
	if err := validateMediaPolicySnapshot(p, c.executionPolicy.snapshot); err != nil {
		return nil, fmt.Errorf("media policy: %w", err)
	}
	return p.clone(), nil
}
