package improver

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/improve/paired"
)

func TestObjectsPrivateImmutableAndReadOnly(t *testing.T) {
	workspace := t.TempDir()
	if store, err := OpenObjects(workspace, "absent", true); err == nil {
		_ = store.Close()
		t.Fatal("read-only opening created missing directories")
	}
	if _, err := os.Stat(filepath.Join(workspace, "improvement")); !os.IsNotExist(err) {
		t.Fatal("read-only opening wrote directories")
	}
	store, err := OpenObjects(workspace, "test", false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	content := []byte(`{"data":"sanitized canonical bytes","total_tokens":25}`)
	ref, err := store.Put("evidence", content)
	if err != nil {
		t.Fatal(err)
	}
	again, err := store.Put("evidence", content)
	if err != nil || ref != again {
		t.Fatalf("exclusive publication: %v", err)
	}
	file := filepath.Join(workspace, "improvement", "paired", "test", "objects", ref.Digest)
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", info, err)
	}
	readonly, err := OpenObjects(workspace, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = readonly.Close() }()
	raw, err := readonly.Read(ref)
	if err != nil || !bytes.Equal(raw, content) {
		t.Fatalf("read: %v %s", err, raw)
	}
	if _, err := readonly.Put("evidence", []byte("other")); err == nil {
		t.Fatal("read-only store wrote")
	}
	if _, err := store.Put("evidence", []byte(`{"api_key":"private-value"}`)); err == nil {
		t.Fatal("secret stored")
	}
	if err := os.WriteFile(file, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readonly.Read(ref); err == nil {
		t.Fatal("corrupted evidence accepted")
	}
}

func TestObjectsRejectSymlinksAndUnsafeReferences(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, "improvement")); err != nil {
		t.Fatal(err)
	}
	if objects, err := OpenObjects(workspace, "test", false); err == nil {
		_ = objects.Close()
		t.Fatal("symlink namespace accepted")
	}
	workspace = t.TempDir()
	store, err := OpenObjects(workspace, "test", false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ref := paired.ArtifactRef{Kind: "evidence", Digest: "../outside", SizeBytes: 0}
	if _, err := store.Read(ref); err == nil {
		t.Fatal("traversal accepted")
	}
	ref.Digest = paired.BytesDigest(nil)
	if err := store.root.Symlink(filepath.Join(outside, "absent"), ref.Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(ref); err == nil {
		t.Fatal("object symlink accepted")
	}
	if _, err := store.Put("evidence", nil); err == nil {
		t.Fatal("put replaced symlink")
	}
}
