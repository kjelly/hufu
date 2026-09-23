package versionstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
)

func sha(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

func assertTmpEmpty(t *testing.T, store *Store) {
	t.Helper()
	entries, err := os.ReadDir(store.layout.tmpDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("tmp/ still contains %d entries", len(entries))
	}
}

func TestPutObjectContentAddressing(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	first, err := store.putBytes(ctx, ObjectBlob, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != sha("hello") || first.Size != 5 || !first.New {
		t.Fatalf("first put = %#v", first)
	}
	again, err := store.putBytes(ctx, ObjectBlob, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if again.Hash != first.Hash || again.New {
		t.Fatalf("second put = %#v, want reuse of %s", again, first.Hash)
	}
	other, err := store.putBytes(ctx, ObjectBlob, []byte("world"))
	if err != nil {
		t.Fatal(err)
	}
	if other.Hash == first.Hash {
		t.Fatal("different bytes produced the same hash")
	}
	path, err := store.layout.objectPath(ObjectBlob, first.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "/blob/sha256/"+first.Hash[:2]+"/"+first.Hash[2:]) {
		t.Fatalf("object path %s does not follow the layout", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("object mode = %o, want 600", perm)
	}
	assertTmpEmpty(t, store)
}

func TestPutObjectRejectsCorruptExistingObject(t *testing.T) {
	tests := []struct {
		name    string
		kind    ObjectKind
		corrupt string
	}{
		{name: "blob with wrong size", kind: ObjectBlob, corrupt: "hello!"},
		{name: "tree with wrong bytes", kind: ObjectTree, corrupt: "HELLO"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := openTestStore(t)
			written, err := store.putBytes(ctx, tt.kind, []byte("hello"))
			if err != nil {
				t.Fatal(err)
			}
			path, _ := store.layout.objectPath(tt.kind, written.Hash)
			if err = os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(path, []byte(tt.corrupt), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err = store.putBytes(ctx, tt.kind, []byte("hello")); !errors.Is(err, ErrCASCorrupt) {
				t.Fatalf("put over corrupt object err = %v, want ErrCASCorrupt", err)
			}
			got, _ := os.ReadFile(path)
			if string(got) != tt.corrupt {
				t.Fatal("corrupt object was overwritten")
			}
			assertTmpEmpty(t, store)
		})
	}
}

func TestPutObjectExpectedHashMismatchPublishesNothing(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	_, err := store.putObject(ctx, ObjectBlob, bytes.NewReader([]byte("later bytes")), sha("observed bytes"), 0)
	if !errors.Is(err, ErrWorkspaceChangedAfterObservation) {
		t.Fatalf("err = %v, want ErrWorkspaceChangedAfterObservation", err)
	}
	if ok, _ := store.hasObject(ObjectBlob, sha("later bytes")); ok {
		t.Fatal("mismatched object was published")
	}
	assertTmpEmpty(t, store)
}

func TestPutObjectEnforcesSizeLimit(t *testing.T) {
	store := openTestStore(t)
	_, err := store.putObject(context.Background(), ObjectBlob, bytes.NewReader([]byte("0123456789")), "", 4)
	if !errors.Is(err, ErrCaptureLimitExceeded) {
		t.Fatalf("err = %v, want ErrCaptureLimitExceeded", err)
	}
	assertTmpEmpty(t, store)
}

func TestTreeAndLinkRoundTripAndIntegrity(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	written, err := store.putTree(ctx, sampleTree())
	if err != nil {
		t.Fatal(err)
	}
	if written.Hash != goldenSampleTreeHash {
		t.Fatalf("stored tree hash = %s", written.Hash)
	}
	tree, err := store.readTree(written.Hash)
	if err != nil || len(tree.Entries) != 4 {
		t.Fatalf("readTree = %#v, %v", tree, err)
	}
	link, err := store.putBytes(ctx, ObjectLink, []byte("target"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.readLink(link.Hash)
	if err != nil || target != "target" {
		t.Fatalf("readLink = %q, %v", target, err)
	}
	if _, err = store.readTree(strings.Repeat("d", 64)); !errors.Is(err, ErrCASObjectMissing) {
		t.Fatalf("missing tree err = %v", err)
	}
	path, _ := store.layout.objectPath(ObjectLink, link.Hash)
	if err = os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.readLink(link.Hash); !errors.Is(err, ErrCASCorrupt) {
		t.Fatalf("tampered link err = %v, want ErrCASCorrupt", err)
	}
	if _, err = store.hashObjectFile(ObjectLink, link.Hash); !errors.Is(err, ErrCASCorrupt) {
		t.Fatalf("hashObjectFile err = %v, want ErrCASCorrupt", err)
	}
}
