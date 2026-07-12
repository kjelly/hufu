//go:build linux || darwin
// +build linux darwin

package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const pathConsentPolicyFile = ".hufu-path-consent.yaml"

// PathConsentPolicy is the durable, team-scoped record of path decisions.
// Paths denote directory prefixes; an entry applies to that directory and all
// of its children.
type PathConsentPolicy struct {
	Allowed []string `yaml:"allowed,omitempty"`
	Denied  []string `yaml:"denied,omitempty"`
}

// PathConsentPolicyPath returns the team-local policy file used by hufu.
func PathConsentPolicyPath(teamDir string) string {
	return filepath.Join(teamDir, pathConsentPolicyFile)
}

// LoadPathConsentPolicy loads a team's durable decisions. A missing policy is
// equivalent to an empty one. Malformed policies are errors rather than being
// silently ignored, so an unexpectedly broad permission is never assumed.
func LoadPathConsentPolicy(teamDir string) (PathConsentPolicy, error) {
	path := PathConsentPolicyPath(teamDir)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return PathConsentPolicy{}, nil
	}
	if err != nil {
		return PathConsentPolicy{}, fmt.Errorf("read path consent policy %s: %w", path, err)
	}
	var policy PathConsentPolicy
	if err := yaml.Unmarshal(data, &policy); err != nil {
		return PathConsentPolicy{}, fmt.Errorf("parse path consent policy %s: %w", path, err)
	}
	policy.Allowed = normalizeConsentPolicyPaths(policy.Allowed)
	policy.Denied = normalizeConsentPolicyPaths(policy.Denied)
	return policy, nil
}

// SavePathConsentPolicy atomically replaces a team's durable decisions.
func SavePathConsentPolicy(teamDir string, policy PathConsentPolicy) error {
	policy.Allowed = normalizeConsentPolicyPaths(policy.Allowed)
	policy.Denied = normalizeConsentPolicyPaths(policy.Denied)
	data, err := yaml.Marshal(policy)
	if err != nil {
		return fmt.Errorf("encode path consent policy: %w", err)
	}
	dir := filepath.Clean(teamDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create team directory for path consent policy: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".hufu-path-consent-*")
	if err != nil {
		return fmt.Errorf("create temporary path consent policy: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure temporary path consent policy: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write path consent policy: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close path consent policy: %w", err)
	}
	if err := os.Rename(tmpName, PathConsentPolicyPath(dir)); err != nil {
		return fmt.Errorf("replace path consent policy: %w", err)
	}
	return nil
}

// UpdatePathConsentPolicy adds or removes one directory from a team's policy.
// Adding an allow removes an identical deny (and vice versa), so the newest
// explicit command is the effective decision.
func UpdatePathConsentPolicy(teamDir, action, path string) (PathConsentPolicy, error) {
	policy, err := LoadPathConsentPolicy(teamDir)
	if err != nil {
		return PathConsentPolicy{}, err
	}
	normalized, ok := normalizeConsentPolicyPath(path)
	if !ok {
		return PathConsentPolicy{}, fmt.Errorf("invalid path %q: provide a non-root directory path", path)
	}
	switch action {
	case "allow":
		policy.Allowed = append(policy.Allowed, normalized)
		policy.Denied = removeConsentPolicyPath(policy.Denied, normalized)
	case "deny":
		policy.Denied = append(policy.Denied, normalized)
		policy.Allowed = removeConsentPolicyPath(policy.Allowed, normalized)
	case "remove":
		policy.Allowed = removeConsentPolicyPath(policy.Allowed, normalized)
		policy.Denied = removeConsentPolicyPath(policy.Denied, normalized)
	default:
		return PathConsentPolicy{}, fmt.Errorf("unknown path consent action %q", action)
	}
	if err := SavePathConsentPolicy(teamDir, policy); err != nil {
		return PathConsentPolicy{}, err
	}
	return LoadPathConsentPolicy(teamDir)
}

func (pc *PathConsent) persistLocked() error {
	if pc.persistPath == "" {
		return nil
	}
	return SavePathConsentPolicy(filepath.Dir(pc.persistPath), PathConsentPolicy{
		Allowed: stripConsentPrefixes(pc.remembered),
		Denied:  stripConsentPrefixes(pc.denied),
	})
}

func consentPrefixes(paths []string) []string {
	paths = normalizeConsentPolicyPaths(paths)
	for i := range paths {
		paths[i] += string(os.PathSeparator)
	}
	return paths
}

func stripConsentPrefixes(prefixes []string) []string {
	paths := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		paths = append(paths, strings.TrimSuffix(prefix, string(os.PathSeparator)))
	}
	return normalizeConsentPolicyPaths(paths)
}

func normalizeConsentPolicyPaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if normalized, ok := normalizeConsentPolicyPath(path); ok && !seen[normalized] {
			seen[normalized] = true
			result = append(result, normalized)
		}
	}
	sort.Strings(result)
	return result
}

func normalizeConsentPolicyPath(path string) (string, bool) {
	return normalizeConsentSuggestedPath(path)
}

func removeConsentPolicyPath(paths []string, target string) []string {
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if normalized, ok := normalizeConsentPolicyPath(path); ok && normalized != target {
			result = append(result, normalized)
		}
	}
	return result
}
