package versionstore

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

var (
	hashA = strings.Repeat("a", 64)
	hashB = strings.Repeat("b", 64)
	hashC = strings.Repeat("c", 64)
)

func sampleTree() TreeObject {
	return TreeObject{Entries: []TreeEntry{
		{Name: "README.md", Kind: EntryBlob, ObjectHash: hashA, Mode: 0o644, Size: 12},
		{Name: "bin", Kind: EntryTree, ObjectHash: hashB},
		{Name: "link", Kind: EntrySymlink, ObjectHash: hashC, Size: 6},
		{Name: "run.sh", Kind: EntryBlob, ObjectHash: hashA, Mode: 0o755, Size: 12},
	}}
}

// goldenSampleTreeHash pins canonical format v1. Changing it means every
// existing store's tree identities change; bump treeFormatVersion instead.
const goldenSampleTreeHash = "952ef0ac2d1dcc6917de6d7003e4aada66f7b2053919d0c7f9aad55eeaa49ff2"

func TestEncodeTreeIsDeterministicAndStable(t *testing.T) {
	first, firstHash, err := EncodeTree(sampleTree())
	if err != nil {
		t.Fatal(err)
	}
	second, secondHash, err := EncodeTree(sampleTree())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || firstHash != secondHash {
		t.Fatal("EncodeTree is not deterministic")
	}
	if firstHash != goldenSampleTreeHash {
		t.Fatalf("tree hash = %s, want golden %s", firstHash, goldenSampleTreeHash)
	}
	decoded, err := DecodeTree(first)
	if err != nil {
		t.Fatalf("DecodeTree: %v", err)
	}
	if len(decoded.Entries) != 4 || decoded.Entries[3] != sampleTree().Entries[3] {
		t.Fatalf("round trip mismatch: %#v", decoded)
	}
	emptyData, _, err := EncodeTree(TreeObject{})
	if err != nil {
		t.Fatal(err)
	}
	if empty, err := DecodeTree(emptyData); err != nil || len(empty.Entries) != 0 {
		t.Fatalf("empty tree round trip: %#v, %v", empty, err)
	}
}

func TestTreeValidationRejectsInvalidEntries(t *testing.T) {
	blob := func(name string) TreeEntry {
		return TreeEntry{Name: name, Kind: EntryBlob, ObjectHash: hashA, Mode: 0o644}
	}
	tests := []struct {
		name    string
		entries []TreeEntry
	}{
		{name: "duplicate names", entries: []TreeEntry{blob("a"), blob("a")}},
		{name: "unsorted", entries: []TreeEntry{blob("b"), blob("a")}},
		{name: "dot", entries: []TreeEntry{blob(".")}},
		{name: "dotdot", entries: []TreeEntry{blob("..")}},
		{name: "empty name", entries: []TreeEntry{blob("")}},
		{name: "slash", entries: []TreeEntry{blob("a/b")}},
		{name: "nul", entries: []TreeEntry{blob("a\x00b")}},
		{name: "invalid utf8", entries: []TreeEntry{blob("\xff")}},
		{name: "unknown kind", entries: []TreeEntry{{Name: "a", Kind: 9, ObjectHash: hashA}}},
		{name: "malformed hash", entries: []TreeEntry{{Name: "a", Kind: EntryBlob, ObjectHash: "xyz"}}},
		{name: "uppercase hash", entries: []TreeEntry{{Name: "a", Kind: EntryBlob, ObjectHash: strings.Repeat("A", 64)}}},
		{name: "blob setuid mode", entries: []TreeEntry{{Name: "a", Kind: EntryBlob, ObjectHash: hashA, Mode: 0o4755}}},
		{name: "symlink with mode", entries: []TreeEntry{{Name: "a", Kind: EntrySymlink, ObjectHash: hashA, Mode: 0o777}}},
		{name: "tree with size", entries: []TreeEntry{{Name: "a", Kind: EntryTree, ObjectHash: hashA, Size: 3}}},
		{name: "negative size", entries: []TreeEntry{{Name: "a", Kind: EntryBlob, ObjectHash: hashA, Size: -1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := EncodeTree(TreeObject{Entries: tt.entries}); !errors.Is(err, ErrInvalidTree) {
				t.Fatalf("EncodeTree err = %v, want ErrInvalidTree", err)
			}
		})
	}
}

func TestDecodeTreeRejectsNonCanonicalBytes(t *testing.T) {
	valid, _, err := EncodeTree(sampleTree())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		data []byte
	}{
		{name: "bad magic", data: append([]byte("NOTATREE\x00"), valid[len(treeMagic):]...)},
		{name: "bad version", data: append(append([]byte(treeMagic), 2), valid[len(treeMagic)+1:]...)},
		{name: "trailing bytes", data: append(append([]byte(nil), valid...), 0)},
		{name: "truncated", data: valid[:len(valid)-5]},
		{name: "empty", data: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DecodeTree(tt.data); !errors.Is(err, ErrInvalidTree) {
				t.Fatalf("DecodeTree err = %v, want ErrInvalidTree", err)
			}
		})
	}
}
