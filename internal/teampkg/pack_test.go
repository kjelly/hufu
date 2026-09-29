package teampkg

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const testResultSchema = `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","required":["status"],"properties":{"status":{"type":"string"}}}`

func writeFixture(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func basicTeam(t *testing.T, manifest string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "portable-team")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, dir, "team.yaml", manifest)
	writeFixture(t, dir, "worker.md", "---\nname: worker\nrole: worker\n---\nDo the work.\n")
	return dir
}

func archiveEntries(t *testing.T, path string) (map[string][]byte, []*zip.File) {
	t.Helper()
	reader, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	entries := make(map[string][]byte, len(reader.File))
	for _, file := range reader.File {
		stream, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data := new(bytes.Buffer)
		if _, err := data.ReadFrom(stream); err != nil {
			_ = stream.Close()
			t.Fatal(err)
		}
		if err := stream.Close(); err != nil {
			t.Fatal(err)
		}
		entries[file.Name] = data.Bytes()
	}
	return entries, reader.File
}

func TestPackIsByteDeterministicAndLocked(t *testing.T) {
	dir := basicTeam(t, "name: portable-team\nmax-rounds: 3\n")
	first := filepath.Join(t.TempDir(), "first.hufu")
	second := filepath.Join(t.TempDir(), "second.hufu")
	result, err := Pack(PackOptions{TeamDir: dir, Version: "1.0.0+test", Output: first})
	if err != nil {
		t.Fatal(err)
	}
	secondResult, err := Pack(PackOptions{TeamDir: dir, Version: "1.0.0+test", Output: second})
	if err != nil {
		t.Fatal(err)
	}
	firstBytes, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	secondBytes, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Fatal("identical inputs produced different package bytes")
	}
	if result.Lock.TeamDigest != secondResult.Lock.TeamDigest {
		t.Fatalf("team digests differ: %s != %s", result.Lock.TeamDigest, secondResult.Lock.TeamDigest)
	}
	entries, files := archiveEntries(t, first)
	names := make([]string, 0, len(files))
	for _, file := range files {
		names = append(names, file.Name)
		if file.Method != zip.Store {
			t.Errorf("%s method = %d, want Store", file.Name, file.Method)
		}
		if file.Mode().Perm() != 0o644 {
			t.Errorf("%s mode = %o, want 0644", file.Name, file.Mode().Perm())
		}
		if len(file.Extra) != 0 || file.Comment != "" {
			t.Errorf("%s has non-empty ZIP metadata", file.Name)
		}
		if got := file.ModTime().UTC(); !got.Equal(time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("%s timestamp = %s", file.Name, got)
		}
	}
	if !slices.IsSorted(names) {
		t.Fatalf("archive entries are not sorted: %v", names)
	}
	var lock Lock
	if err := json.Unmarshal(entries[LockFilename], &lock); err != nil {
		t.Fatal(err)
	}
	if lock.TeamDigest != result.Lock.TeamDigest || len(lock.Files) != len(entries)-1 {
		t.Fatalf("lock = %#v, result = %#v", lock, result.Lock)
	}
	if !bytes.HasSuffix(entries[ManifestFilename], []byte("\n")) || !bytes.HasSuffix(entries[LockFilename], []byte("\n")) {
		t.Fatal("generated metadata must end in a newline")
	}
}

func TestPackInventoryIncludesOwnedSourcesOnly(t *testing.T) {
	dir := basicTeam(t, `name: portable-team
action-providers:
  transform:
    runtime: golang
    source: actions/transform
    mode: trusted-static
`)
	writeFixture(t, dir, "worker.md", "---\nname: worker\nrole: worker\nresult-contract:\n  schema: schemas/result.json\n  require-structured: true\n---\nDo the work.\n")
	writeFixture(t, dir, "README.md", "# Portable team\n")
	writeFixture(t, dir, "schemas/result.json", testResultSchema)
	writeFixture(t, dir, "skills/cp201-portable-skill/SKILL.md", "---\nname: cp201-portable-skill\ndescription: Test package inventory.\n---\nUse the nested reference.\n")
	writeFixture(t, dir, "skills/cp201-portable-skill/references/nested.md", "nested\n")
	writeFixture(t, dir, "actions/transform/main.go", "package main\nimport (\"context\"; \"io\")\nfunc Run(_ context.Context, _ io.Reader, _ io.Writer) error { return nil }\n")
	writeFixture(t, dir, "actions/transform/main_test.go", "package main\n")
	writeFixture(t, dir, "notes.txt", "not structurally owned\n")
	output := filepath.Join(t.TempDir(), "team.hufu")
	if _, err := Pack(PackOptions{TeamDir: dir, Version: "build-7", Output: output}); err != nil {
		t.Fatal(err)
	}
	entries, _ := archiveEntries(t, output)
	want := []string{
		LockFilename, ManifestFilename, "README.md", "actions/transform/main.go",
		"schemas/result.json", "skills/cp201-portable-skill/SKILL.md",
		"skills/cp201-portable-skill/references/nested.md", "team.yaml", "worker.md",
	}
	got := make([]string, 0, len(entries))
	for path := range entries {
		got = append(got, path)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	if _, ok := entries["actions/transform/main_test.go"]; ok {
		t.Fatal("Go test source was packaged")
	}
	if _, ok := entries["notes.txt"]; ok {
		t.Fatal("unowned top-level file was packaged")
	}
}

func TestPackSupportsLegacyAndV1Alpha1Manifests(t *testing.T) {
	tests := map[string]string{
		"legacy":               "name: portable-team\n",
		"legacy-filename-name": "{}\n",
		"v1alpha1":             "apiVersion: hufu.io/v1alpha1\nkind: AgentTeam\nmetadata:\n  name: portable-team\nspec: {}\n",
	}
	for name, manifest := range tests {
		t.Run(name, func(t *testing.T) {
			dir := basicTeam(t, manifest)
			output := filepath.Join(t.TempDir(), name+".hufu")
			if _, err := Pack(PackOptions{TeamDir: dir, Version: "v1", Output: output}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPackChangesDigestWhenIdentityChanges(t *testing.T) {
	dir := basicTeam(t, "name: portable-team\n")
	first, err := Pack(PackOptions{TeamDir: dir, Version: "v1", Output: filepath.Join(t.TempDir(), "v1.hufu")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Pack(PackOptions{TeamDir: dir, Version: "v2", Output: filepath.Join(t.TempDir(), "v2.hufu")})
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, dir, "worker.md", "---\nname: worker\nrole: worker\n---\nChanged worker bytes.\n")
	third, err := Pack(PackOptions{TeamDir: dir, Version: "v2", Output: filepath.Join(t.TempDir(), "content.hufu")})
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, dir, "team.yaml", "name: renamed-team\n")
	fourth, err := Pack(PackOptions{TeamDir: dir, Version: "v2", Output: filepath.Join(t.TempDir(), "renamed.hufu")})
	if err != nil {
		t.Fatal(err)
	}
	if first.Lock.TeamDigest == second.Lock.TeamDigest || second.Lock.TeamDigest == third.Lock.TeamDigest || third.Lock.TeamDigest == fourth.Lock.TeamDigest {
		t.Fatal("version, source bytes, or team name change did not change the team digest")
	}
}

func TestPackRejectsOverwriteAndInvalidVersion(t *testing.T) {
	dir := basicTeam(t, "name: portable-team\n")
	output := filepath.Join(t.TempDir(), "team.hufu")
	writeFixture(t, filepath.Dir(output), filepath.Base(output), "keep")
	if _, err := Pack(PackOptions{TeamDir: dir, Version: "v1", Output: output}); err == nil || !strings.Contains(err.Error(), "without overwrite") {
		t.Fatalf("overwrite error = %v", err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep" {
		t.Fatalf("existing output changed to %q", data)
	}
	if _, err := Pack(PackOptions{TeamDir: dir, Version: "bad version", Output: filepath.Join(t.TempDir(), "bad.hufu")}); err == nil {
		t.Fatal("invalid version was accepted")
	}
}

func TestPackRejectsUnsupportedUnownedCompileDependency(t *testing.T) {
	dir := basicTeam(t, "name: portable-team\n")
	writeFixture(t, dir, "invariants.yaml", "schema-version: 1\ninvariants:\n  - id: preserve\n    statement: preserve behavior\n    severity: error\n    applies-to: [internal/]\n")
	_, err := Pack(PackOptions{TeamDir: dir, Version: "v1", Output: filepath.Join(t.TempDir(), "team.hufu")})
	if err == nil || !strings.Contains(err.Error(), "unsupported package dependency") || !strings.Contains(err.Error(), "invariant catalog") {
		t.Fatalf("error = %v", err)
	}
}

func TestPackRequiresExactlyOneTeamManifest(t *testing.T) {
	dir := basicTeam(t, "name: portable-team\n")
	writeFixture(t, dir, "team.yml", "name: duplicate\n")
	_, err := Pack(PackOptions{TeamDir: dir, Version: "v1", Output: filepath.Join(t.TempDir(), "team.hufu")})
	if err == nil || !strings.Contains(err.Error(), "manifest_count") {
		t.Fatalf("error = %v", err)
	}
}
