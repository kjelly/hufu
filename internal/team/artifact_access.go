package team

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// openArtifactRef is the only bridge from a model-visible opaque artifact ID
// to stored bytes. It deliberately resolves IDs, not paths: an invalid or
// mistyped reference fails here before a filesystem tool or path-consent layer
// can interpret model text as a new path.
func (c *Coordinator) openArtifactRef(ctx context.Context, id string) (io.ReadCloser, error) {
	id = strings.TrimSpace(id)
	if c == nil || c.session == nil || id == "" {
		return nil, fmt.Errorf("artifact reference is required")
	}
	ref, producerTaskID, ok := c.authorizedArtifactRef(ctx, id)
	if !ok {
		return nil, fmt.Errorf("artifact reference %q is unknown or not authorized for this task", id)
	}
	if ref.ID != id || strings.TrimSpace(ref.SHA256) == "" {
		return nil, fmt.Errorf("artifact reference %q is not immutable", id)
	}
	if ref.TaskID != "" && producerTaskID != "" && ref.TaskID != producerTaskID {
		return nil, fmt.Errorf("artifact reference %q has mismatched producer ownership", id)
	}

	store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
	if err != nil {
		return nil, fmt.Errorf("open artifact store: %w", err)
	}
	if err := store.Verify(ctx, ref); err != nil {
		return nil, fmt.Errorf("artifact reference %q failed integrity verification: %w", id, err)
	}
	reader, err := store.Open(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("open artifact reference %q: %w", id, err)
	}
	return reader, nil
}

func (c *Coordinator) authorizedArtifactRef(ctx context.Context, id string) (ArtifactRef, string, bool) {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return ArtifactRef{}, "", false
	}
	consumerTaskID, _ := ctx.Value(todoIDKey{}).(string)
	allowedProducers := make(map[string]bool)
	if consumerTaskID != "" {
		allowedProducers[consumerTaskID] = true
		consumer := c.todoItemByID(consumerTaskID)
		if consumer == nil {
			return ArtifactRef{}, "", false
		}
		for _, dependency := range consumer.DependsOn {
			allowedProducers[dependency] = true
		}
	}

	for _, item := range c.taskTracker.TodoList().Items() {
		if item == nil || item.Status != TaskDone {
			continue
		}
		if consumerTaskID != "" && !allowedProducers[item.ID] {
			continue
		}
		result := item.TypedResult
		if result == nil || !taskResultStatusIsSuccessful(result.Status) {
			continue
		}
		if ref, ok := artifactRefByID(result, id); ok {
			return ref, item.ID, true
		}
	}
	return ArtifactRef{}, "", false
}

func artifactRefByID(result *TaskResult, id string) (ArtifactRef, bool) {
	if result == nil || id == "" {
		return ArtifactRef{}, false
	}
	if result.RawOutputRef != nil && result.RawOutputRef.ID == id {
		return *result.RawOutputRef, true
	}
	for _, artifact := range result.Artifacts {
		if artifact.ID == id {
			return artifact, true
		}
	}
	for _, output := range result.Outputs {
		if output.Artifact != nil && output.Artifact.ID == id {
			return *output.Artifact, true
		}
	}
	return ArtifactRef{}, false
}
