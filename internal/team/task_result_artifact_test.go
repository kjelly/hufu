package team

import (
	"strings"
	"testing"
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
