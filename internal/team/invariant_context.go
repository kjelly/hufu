package team

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	contextstore "github.com/kjelly/hufu/internal/context"
)

const repositoryInvariantSource = "repository_invariant"

func canonicalInvariantContent(definition InvariantDefinition) string {
	var content strings.Builder
	fmt.Fprintf(&content, "Invariant ID: %s\n", definition.ID)
	fmt.Fprintf(&content, "Severity: %s\n", definition.Severity)
	content.WriteString("Applies to:\n")
	for _, appliesTo := range definition.AppliesTo {
		fmt.Fprintf(&content, "- %s\n", appliesTo)
	}
	content.WriteString("Statement:\n")
	content.WriteString(definition.Statement)
	return content.String()
}

func fullContentHash(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

func validFullContentHash(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func materializeInvariantContextItem(c *Coordinator, definition InvariantDefinition) (contextstore.ContextItem, error) {
	if c == nil || c.session == nil {
		return contextstore.ContextItem{}, fmt.Errorf("materialize invariant %q: coordinator session is unavailable", definition.ID)
	}
	teamName := normalizedName(c.session.Config.Name)
	if teamName == "" {
		return contextstore.ContextItem{}, fmt.Errorf("materialize invariant %q: team identity is empty", definition.ID)
	}
	content := canonicalInvariantContent(definition)
	return contextstore.ContextItem{
		ID:          "invariant:" + teamName + ":" + definition.ID,
		Kind:        contextstore.ContextInvariant,
		Content:     content,
		ContentHash: fullContentHash(content),
		Scope: contextstore.Scope{
			ProjectID: c.contextScopeProjectID(),
			TeamID:    c.session.Config.Name,
		},
		Authority:  contextstore.AuthorityRepository,
		TrustLevel: contextstore.TrustTrusted,
		Priority:   contextstore.PriorityCritical,
		MustKeep:   true,
		Source: contextstore.SourceRef{
			Type: "team_invariant_manifest",
			Path: "invariants.yaml",
			Ref:  definition.ID,
		},
		Tags:      slices.Clone(definition.AppliesTo),
		Metadata:  map[string]string{"invariant.id": definition.ID, "invariant.severity": string(definition.Severity), "activation.phases": string(PhaseVerify)},
		Lifecycle: contextstore.LifecycleConfirmed,
	}, nil
}

func invariantAppliesToTouchedPaths(definition InvariantDefinition, touchedPaths []string) bool {
	if len(touchedPaths) == 0 {
		return true
	}
	for _, appliesTo := range definition.AppliesTo {
		if appliesTo == "*" {
			return true
		}
		for _, touchedPath := range touchedPaths {
			if strings.HasSuffix(appliesTo, "/") {
				if strings.HasPrefix(touchedPath, appliesTo) {
					return true
				}
				continue
			}
			if touchedPath == appliesTo {
				return true
			}
		}
	}
	return false
}

func normalizeRequestTouchedPaths(paths []string) ([]string, error) {
	normalized := make(map[string]struct{}, len(paths))
	for index, rawPath := range paths {
		path, err := NormalizeTouchedPath(rawPath)
		if err != nil {
			return nil, fmt.Errorf("touched_paths[%d]: %w", index, err)
		}
		normalized[path] = struct{}{}
	}
	result := make([]string, 0, len(normalized))
	for path := range normalized {
		result = append(result, path)
	}
	slices.Sort(result)
	return result, nil
}

func (c *Coordinator) invariantVerificationModeForRequest(request ContextRequest) InvariantVerificationMode {
	if c == nil || strings.TrimSpace(request.TaskID) == "" {
		return ""
	}
	if item := c.todoItemByID(request.TaskID); item != nil {
		return item.InvariantVerification
	}
	return ""
}
