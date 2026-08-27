package tools

import (
	"strings"
	"testing"
)

func TestArtifactToolSchemasSeparateFilesystemPathsAndOpaqueRefs(t *testing.T) {
	viewInfo := NewViewTool().Info()
	if !strings.Contains(viewInfo.Description, "filesystem file_path") || !strings.Contains(viewInfo.Description, "opaque artifact_ref") {
		t.Fatalf("view description does not distinguish sources: %q", viewInfo.Description)
	}
	filePath, ok := viewInfo.Parameters["file_path"].(map[string]any)
	if !ok || !strings.Contains(filePath["description"].(string), "opaque artifact ID") {
		t.Fatalf("view file_path schema does not reject opaque IDs: %#v", viewInfo.Parameters["file_path"])
	}
	artifactRef, ok := viewInfo.Parameters["artifact_ref"].(map[string]any)
	if !ok || !strings.Contains(artifactRef["description"].(string), "Pass the ID unchanged") {
		t.Fatalf("view artifact_ref schema does not preserve opaque IDs: %#v", viewInfo.Parameters["artifact_ref"])
	}

	lsInfo := NewLsTool().Info()
	if !strings.Contains(lsInfo.Description, "filesystem directory") || !strings.Contains(lsInfo.Description, "cannot resolve opaque artifact IDs") {
		t.Fatalf("ls description does not state filesystem-only semantics: %q", lsInfo.Description)
	}
	path, ok := lsInfo.Parameters["path"].(map[string]any)
	if !ok || !strings.Contains(path["description"].(string), "does not resolve opaque artifact IDs") {
		t.Fatalf("ls path schema does not reject opaque IDs: %#v", lsInfo.Parameters["path"])
	}
}
