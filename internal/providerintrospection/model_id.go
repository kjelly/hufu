package providerintrospection

import "strings"

// unqualifiedModelID removes only the provider qualifier from a model ID.
// The remaining path is opaque to this package and may contain namespaces.
func unqualifiedModelID(modelID string) string {
	modelID = strings.TrimSpace(modelID)
	_, unqualified, found := strings.Cut(modelID, "/")
	if !found {
		return modelID
	}
	return unqualified
}

// matchesRequestedModel accepts exact IDs and IDs where exactly one side has
// a provider qualifier. Comparing each unqualified side with the other full
// ID keeps provider-qualified IDs from different providers distinct.
func matchesRequestedModel(candidate, requested string) bool {
	candidate = strings.TrimSpace(candidate)
	requested = strings.TrimSpace(requested)
	if candidate == requested {
		return true
	}
	return unqualifiedModelID(candidate) == requested || unqualifiedModelID(requested) == candidate
}
