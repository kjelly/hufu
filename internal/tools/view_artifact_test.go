//go:build linux || darwin

package tools

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestViewArtifactUsesOpaqueResolverWithoutPathConsent(t *testing.T) {
	var opened string
	response, err := executeView(context.Background(), fantasy.ToolCall{
		Input: `{"artifact_ref":"ref-exact"}`,
	}, t.TempDir(), ToolConfig{
		PathConsent: NewPathConsent(),
		ArtifactOpener: func(_ context.Context, ref string) (io.ReadCloser, error) {
			opened = ref
			return io.NopCloser(strings.NewReader("first\nsecond\n")), nil
		},
	})
	if err != nil || response.IsError {
		t.Fatalf("view artifact response=%#v err=%v", response, err)
	}
	if opened != "ref-exact" || !strings.Contains(response.Content, "<artifact:ref-exact>") || !strings.Contains(response.Content, "second") {
		t.Fatalf("opaque artifact was not resolved exactly: opened=%q response=%q", opened, response.Content)
	}
}

func TestViewArtifactTypoFailsAsReferenceBeforeFilesystemPolicy(t *testing.T) {
	response, err := executeView(context.Background(), fantasy.ToolCall{
		Input: `{"artifact_ref":"ref-typo"}`,
	}, "/definitely/not/the/artifact/path", ToolConfig{
		PathConsent: NewPathConsent(),
		ArtifactOpener: func(_ context.Context, ref string) (io.ReadCloser, error) {
			return nil, fmt.Errorf("unknown reference %q", ref)
		},
	})
	if err != nil {
		t.Fatalf("view artifact returned Go error: %v", err)
	}
	if !response.IsError || !strings.Contains(response.Content, "invalid artifact_ref") || strings.Contains(response.Content, "outside allowed paths") {
		t.Fatalf("mistyped artifact ref reached filesystem policy: %#v", response)
	}
}

func TestViewRequiresExactlyOneSource(t *testing.T) {
	for _, input := range []string{`{}`, `{"file_path":"report.txt","artifact_ref":"ref-report"}`} {
		response, err := executeView(context.Background(), fantasy.ToolCall{Input: input}, t.TempDir(), ToolConfig{})
		if err != nil || !response.IsError || !strings.Contains(response.Content, "exactly one") {
			t.Fatalf("input %s response=%#v err=%v", input, response, err)
		}
	}
}
