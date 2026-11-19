package team

import (
	"strings"
	"testing"
	"time"
)

func TestResolveTaskResultArtifactCanonicalProjections(t *testing.T) {
	artifact := ArtifactRef{ID: "artifact-id", Description: "artifact-name", SHA256: "digest", Bytes: 3, ByteSize: 3}
	raw := ArtifactRef{ID: "raw-id", SHA256: "raw-digest", Bytes: 4, ByteSize: 4}
	tests := []struct {
		name     string
		result   *TaskResult
		selector string
		wantID   string
		wantErr  string
	}{
		{name: "artifacts by opaque id", result: &TaskResult{Artifacts: []ArtifactRef{artifact}}, selector: artifact.ID, wantID: artifact.ID},
		{name: "named structured output", result: &TaskResult{Outputs: map[string]StructuredOutputValue{
			"manifest": {Kind: ExecutionOutputArtifact, Artifact: &artifact},
		}}, selector: "manifest", wantID: artifact.ID},
		{name: "true artifact output", result: &TaskResult{Outputs: map[string]StructuredOutputValue{
			"manifest": {Kind: ExecutionOutputArtifact, Artifact: &artifact},
		}}, selector: artifact.Description, wantID: artifact.ID},
		{name: "raw output by opaque id", result: &TaskResult{RawOutputRef: &raw}, selector: raw.ID, wantID: raw.ID},
		{name: "artifact id collides with non-artifact output name", result: &TaskResult{
			Artifacts: []ArtifactRef{artifact},
			Outputs: map[string]StructuredOutputValue{
				artifact.ID: {Kind: ExecutionOutputFact, Fact: &StructuredFact{Name: artifact.ID}},
			},
		}, selector: artifact.ID, wantErr: "invalid declarations"},
		{name: "artifact description collides with non-artifact output name", result: &TaskResult{
			Artifacts: []ArtifactRef{artifact},
			Outputs: map[string]StructuredOutputValue{
				artifact.Description: {Kind: ExecutionOutputFact, Fact: &StructuredFact{Name: artifact.Description}},
			},
		}, selector: artifact.Description, wantErr: "invalid declarations"},
		{name: "absent", result: &TaskResult{}, selector: "missing", wantErr: "not declared"},
		{name: "ambiguous named declarations", result: &TaskResult{Artifacts: []ArtifactRef{
			{ID: "one", Description: "manifest", SHA256: "one"},
			{ID: "two", Description: "manifest", SHA256: "two"},
		}}, selector: "manifest", wantErr: "ambiguous"},
		{name: "conflicting opaque declarations", result: &TaskResult{Artifacts: []ArtifactRef{
			{ID: "same", SHA256: "one"}, {ID: "same", SHA256: "two"},
		}}, selector: "same", wantErr: "conflicting"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveTaskResultArtifact(tc.result, tc.selector)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("resolve error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got.ID != tc.wantID {
				t.Fatalf("resolve = %#v, err=%v, want id %q", got, err, tc.wantID)
			}
		})
	}
}

func TestResolveTaskResultArtifactEquivalentOccurrences(t *testing.T) {
	base := ArtifactRef{ID: "sha256-content", SHA256: "content", Bytes: 3, ByteSize: 3, RunID: "run", TaskID: "task", Attempt: 1, Agent: "producer", Provider: "native", Kind: "input", Path: "first", CreatedAt: time.Unix(1, 0)}
	for _, tc := range []struct {
		name    string
		change  func(*ArtifactRef)
		allowed bool
	}{
		{"path_and_time", func(r *ArtifactRef) { r.Path = "second"; r.CreatedAt = time.Unix(2, 0) }, true},
		{"hash", func(r *ArtifactRef) { r.SHA256 = "other" }, false},
		{"size", func(r *ArtifactRef) { r.Bytes++ }, false},
		{"byte_size", func(r *ArtifactRef) { r.ByteSize++ }, false},
		{"run", func(r *ArtifactRef) { r.RunID = "other" }, false},
		{"task", func(r *ArtifactRef) { r.TaskID = "other" }, false},
		{"attempt", func(r *ArtifactRef) { r.Attempt++ }, false},
		{"agent", func(r *ArtifactRef) { r.Agent = "other" }, false},
		{"provider", func(r *ArtifactRef) { r.Provider = "other" }, false},
		{"tool_call", func(r *ArtifactRef) { r.ToolCallID = "other" }, false},
		{"kind", func(r *ArtifactRef) { r.Kind = "other" }, false},
		{"missing_origin", func(r *ArtifactRef) { r.RunID = ""; r.Path = "second" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := base
			tc.change(&other)
			result := &TaskResult{Artifacts: []ArtifactRef{base, other}}
			got, err := resolveTaskResultArtifact(result, base.ID)
			if tc.allowed {
				if err != nil || got != base {
					t.Fatalf("canonical=%+v err=%v", got, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "conflicting declarations") {
				t.Fatalf("conflict lost: %v", err)
			}
			if result.Artifacts[0] != base || result.Artifacts[1] != other {
				t.Fatal("lookup rewrote declarations")
			}
		})
	}
}
