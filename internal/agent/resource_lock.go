package agent

import (
	"fmt"
	"strings"
)

// RequiredResourceKind enumerates the categories of content a team can
// declare as a required, hash-locked resource. See
// docs/architecture/strict-verification.md §8.2 for the target design this
// mirrors, and spec.md ("Generic Required Resource Lock") for the runtime
// that resolves and locks these declarations.
type RequiredResourceKind string

const (
	ResourceSkill        RequiredResourceKind = "skill"
	ResourcePrompt       RequiredResourceKind = "prompt"
	ResourceProjectRules RequiredResourceKind = "project_rules"
	ResourceSchema       RequiredResourceKind = "schema"
)

func (k RequiredResourceKind) valid() bool {
	switch k {
	case ResourceSkill, ResourcePrompt, ResourceProjectRules, ResourceSchema:
		return true
	default:
		return false
	}
}

// RequiredResourceSpec is one team-authored declaration that a resource
// must be resolved, hash-locked, and injected before any provider/model
// call starts. Path is the authoring-time locator; it is not durable
// identity — the runtime resolves it to a canonical path and SHA-256 digest
// before trusting it (internal/team.ResolveRequiredResource).
type RequiredResourceSpec struct {
	Name       string               `yaml:"name" json:"name"`
	Kind       RequiredResourceKind `yaml:"kind" json:"kind"`
	Path       string               `yaml:"path" json:"path"`
	SHA256     string               `yaml:"sha256,omitempty" json:"sha256,omitempty"`
	InjectInto []string             `yaml:"inject-into,omitempty" json:"inject_into,omitempty"`
	Required   bool                 `yaml:"required" json:"required"`
}

// Validate checks one declaration at team-load time so a malformed entry
// fails before any run starts, not mid-run at first dispatch.
func (r RequiredResourceSpec) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("name must not be empty")
	}
	if !r.Kind.valid() {
		return fmt.Errorf("kind %q is not one of skill, prompt, project_rules, schema", r.Kind)
	}
	if strings.TrimSpace(r.Path) == "" {
		return fmt.Errorf("path must not be empty")
	}
	if sha := strings.TrimSpace(r.SHA256); sha != "" {
		if len(sha) != 64 {
			return fmt.Errorf("sha256 must be 64 hex characters, got %d", len(sha))
		}
		for _, c := range sha {
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return fmt.Errorf("sha256 must be hex, got %q", r.SHA256)
			}
		}
	}
	return nil
}

// ValidateRequiredResources validates a full declared list, additionally
// rejecting duplicate names: bind/inject looks resources up by name, so a
// duplicate would make injection ambiguous.
func ValidateRequiredResources(specs []RequiredResourceSpec) error {
	seen := make(map[string]bool, len(specs))
	for i, spec := range specs {
		if err := spec.Validate(); err != nil {
			return fmt.Errorf("required-resources[%d]: %w", i, err)
		}
		if seen[spec.Name] {
			return fmt.Errorf("required-resources[%d]: duplicate name %q", i, spec.Name)
		}
		seen[spec.Name] = true
	}
	return nil
}
