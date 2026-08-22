package team

import (
	"fmt"
	"sort"
	"strings"
)

// taskResultArtifactDeclaration is one canonical artifact projection emitted
// by a task result. A named structured output may be an artifact declaration
// even when its value is malformed; retaining that declaration lets the
// resolver fail closed instead of treating a non-artifact output as absent.
type taskResultArtifactDeclaration struct {
	label string
	name  string
	ref   *ArtifactRef
}

// taskResultArtifactDeclarations enumerates every supported TaskResult
// artifact projection in deterministic order. The result is intentionally
// scoped to one task result: callers must select the intended producer before
// invoking this resolver and it never searches other tasks.
func taskResultArtifactDeclarations(result *TaskResult) []taskResultArtifactDeclaration {
	if result == nil {
		return nil
	}
	declarations := make([]taskResultArtifactDeclaration, 0, len(result.Artifacts)+len(result.Outputs)+1)
	for index := range result.Artifacts {
		ref := result.Artifacts[index]
		declarations = append(declarations, taskResultArtifactDeclaration{
			label: fmt.Sprintf("artifacts[%d]", index), ref: &ref,
		})
	}
	if result.RawOutputRef != nil {
		ref := *result.RawOutputRef
		declarations = append(declarations, taskResultArtifactDeclaration{label: "raw_output_ref", ref: &ref})
	}
	names := make([]string, 0, len(result.Outputs))
	for name := range result.Outputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		output := result.Outputs[name]
		var ref *ArtifactRef
		if normalizedExecutionOutputKind(output.Kind) == ExecutionOutputArtifact && output.Artifact != nil {
			copyRef := *output.Artifact
			ref = &copyRef
		}
		declarations = append(declarations, taskResultArtifactDeclaration{
			label: fmt.Sprintf("outputs[%q]", name), name: name, ref: ref,
		})
	}
	return declarations
}

// resolveTaskResultArtifact resolves either an opaque artifact ID or a named
// structured output from one canonical TaskResult. Equivalent projections of
// one artifact are accepted; absent, ambiguous, and conflicting declarations
// are rejected deterministically.
func resolveTaskResultArtifact(result *TaskResult, selector string) (ArtifactRef, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return ArtifactRef{}, fmt.Errorf("artifact selector is required")
	}

	declarations := taskResultArtifactDeclarations(result)
	matches := make([]taskResultArtifactDeclaration, 0, len(declarations))
	for _, declaration := range declarations {
		if selector == declaration.name || declaration.ref != nil && (selector == declaration.ref.ID || selector == declaration.ref.Description) {
			matches = append(matches, declaration)
		}
	}
	if len(matches) == 0 {
		return ArtifactRef{}, fmt.Errorf("artifact %q was not declared by the task result", selector)
	}

	labels := make([]string, 0, len(matches))
	for _, match := range matches {
		labels = append(labels, match.label)
		if match.ref == nil {
			return ArtifactRef{}, artifactDeclarationConflictError(selector, labels, "invalid")
		}
	}

	canonical := matches[0]
	for _, match := range matches {
		if *canonical.ref != *match.ref {
			kind := "ambiguous"
			if selector == canonical.ref.ID || selector == match.ref.ID {
				kind = "conflicting"
			}
			return ArtifactRef{}, artifactDeclarationConflictError(selector, labels, kind)
		}
	}
	if canonical.ref == nil || strings.TrimSpace(canonical.ref.ID) == "" {
		return ArtifactRef{}, fmt.Errorf("structured output %q is not an immutable artifact declaration", selector)
	}
	return *canonical.ref, nil
}

func artifactDeclarationConflictError(selector string, labels []string, kind string) error {
	sort.Strings(labels)
	return fmt.Errorf("artifact %q has %s declarations: %s", selector, kind, strings.Join(labels, ", "))
}
