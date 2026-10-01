package team

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestDecodeWorksetManifestSchemaVersions(t *testing.T) {
	cases := []struct {
		name    string
		version int
		wantErr string
	}{
		{name: "version 1", version: 1},
		{name: "version 2", version: 2},
		{name: "version 3 with source snapshot inputs", version: 3},
		{name: "unknown future version", version: 4, wantErr: "unsupported workset schema_version 4"},
		{name: "missing version", version: 0, wantErr: "unsupported workset schema_version 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"schema_version":` + strconv.Itoa(tc.version) + `,"scope":{"satisfied":true},"items":[{"key":"unit-0000","bindings":{"x":"1"},"inputs":[` +
				`{"id":"sha256-diff","sha256":"aa","kind":"review_diff","path":"workset/primary/batches/unit-0000/diff.patch"},` +
				`{"id":"sha256-snapshot","sha256":"bb","kind":"review_source_snapshot","path":"workset/primary/batches/unit-0000/source.txt"}]}]}`
			manifest, err := decodeWorksetManifest([]byte(raw))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode workset manifest: %v", err)
			}
			if manifest.SchemaVersion != tc.version || len(manifest.Items) != 1 || len(manifest.Items[0].Inputs) != 2 {
				t.Fatalf("decoded manifest = %#v", manifest)
			}
		})
	}
}

// The hufu-code-review producer and the runtime decoder version the workset
// manifest separately. f99b8d6 raised the producer to version 3 while the
// runtime still accepted only 1 and 2, and every review run then failed in
// PREPARE; reviewprep's own tests could not see it.
func TestHufuCodeReviewProducerWorksetVersionIsAccepted(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(hufuCodeReviewTeamDir(t), "reviewprep", "main.go"))
	if err != nil {
		t.Fatalf("read reviewprep source: %v", err)
	}
	match := regexp.MustCompile(`(?m)^\s*manifestSchemaVersion\s*=\s*(\d+)`).FindSubmatch(source)
	if match == nil {
		t.Fatal("reviewprep no longer declares manifestSchemaVersion; update this guard")
	}
	version, err := strconv.Atoi(string(match[1]))
	if err != nil {
		t.Fatalf("parse manifestSchemaVersion %q: %v", match[1], err)
	}
	if err := validateWorksetManifest(WorksetManifest{SchemaVersion: version, Items: []WorksetItem{{Key: "unit-0000"}}}); err != nil {
		t.Fatalf("runtime rejects the producer's workset schema_version %d: %v", version, err)
	}
}
