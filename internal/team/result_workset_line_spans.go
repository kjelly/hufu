package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"strings"
)

// A team can opt in using a JSON artifact already assigned by its frozen
// workset manifest. The format describes original source lines, not bundle
// offsets or coordinates supplied by the worker.
type worksetLineSpansSpec struct {
	ArtifactKind string            `json:"artifact_kind"`
	WhenBinding  map[string]string `json:"when_binding,omitempty"`
}

func (s *worksetLineSpansSpec) validate() error {
	if s.ArtifactKind == "" || strings.TrimSpace(s.ArtifactKind) != s.ArtifactKind {
		return fmt.Errorf("line_spans requires an exact artifact_kind")
	}
	for key, value := range s.WhenBinding {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			return fmt.Errorf("line_spans when_binding requires non-empty names and values")
		}
	}
	return nil
}

type worksetLineSpan struct {
	Start int `json:"start_line"`
	End   int `json:"end_line"`
}

func (c *Coordinator) verifiedWorksetLineSpans(ctx context.Context, binding *WorksetBinding, spec *worksetLineSpansSpec) (map[string][]worksetLineSpan, error) {
	if spec == nil || len(binding.TouchedPaths) == 0 {
		return nil, nil
	}
	for key, value := range spec.WhenBinding {
		if binding.Bindings[key] != value {
			return nil, nil
		}
	}
	var ref ArtifactRef
	for _, input := range binding.Inputs {
		if input.Kind == spec.ArtifactKind {
			if ref.ID != "" {
				return nil, fmt.Errorf("ambiguous frozen source line artifact of kind %q", spec.ArtifactKind)
			}
			ref = input
		}
	}
	if ref.ID == "" || ref.ByteSize <= 0 || ref.ByteSize > maxWorksetBytes {
		return nil, fmt.Errorf("missing or oversized frozen source line artifact of kind %q", spec.ArtifactKind)
	}
	store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.session.Workspace)
	if err != nil {
		return nil, err
	}
	r, err := store.Open(ctx, ref.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(io.LimitReader(r, ref.ByteSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != ref.ByteSize || decisionBytesHash(data) != ref.SHA256 {
		return nil, fmt.Errorf("frozen source line artifact has changed")
	}
	return decodeWorksetLineSpans(data, binding.TouchedPaths)
}

func decodeWorksetLineSpans(data []byte, paths []string) (map[string][]worksetLineSpan, error) {
	var inventory struct {
		Sources []struct {
			Path  string             `json:"path"`
			Spans *[]worksetLineSpan `json:"available_spans"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(data, &inventory); err != nil {
		return nil, fmt.Errorf("invalid frozen source line artifact: %w", err)
	}
	spans := make(map[string][]worksetLineSpan, len(paths))
	for _, path := range paths {
		spans[path] = nil
	}
	for _, source := range inventory.Sources {
		previous, assigned := spans[source.Path]
		if !assigned {
			continue
		}
		if previous != nil || source.Spans == nil {
			return nil, fmt.Errorf("ambiguous or missing frozen source spans for %q", source.Path)
		}
		for _, span := range *source.Spans {
			if span.Start < 1 || span.End < span.Start {
				return nil, fmt.Errorf("invalid frozen source span for %q", source.Path)
			}
		}
		spans[source.Path] = *source.Spans
	}
	for _, path := range paths {
		if spans[path] == nil {
			return nil, fmt.Errorf("missing frozen source entry for %q", path)
		}
	}
	return spans, nil
}
