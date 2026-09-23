package versionstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// EntryKind is the on-disk kind byte of a tree entry.
type EntryKind uint8

const (
	EntryBlob    EntryKind = 1
	EntryTree    EntryKind = 2
	EntrySymlink EntryKind = 3
)

func (k EntryKind) String() string {
	switch k {
	case EntryBlob:
		return "blob"
	case EntryTree:
		return "tree"
	case EntrySymlink:
		return "symlink"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(k))
	}
}

// TreeEntry is one child of a Merkle tree node. Field values are fixed per
// kind so that every implementation hashes the same tree identically:
//
//	blob:    Mode = perm & 0o777, Size = file bytes, ObjectHash = SHA256(bytes)
//	symlink: Mode = 0, Size = len(target), ObjectHash = SHA256(target)
//	tree:    Mode = 0, Size = 0, ObjectHash = child tree hash
type TreeEntry struct {
	Name       string
	Kind       EntryKind
	ObjectHash string
	Mode       uint32
	Size       int64
}

// TreeObject is one directory. Entries are sorted bytewise by Name.
type TreeObject struct {
	Entries []TreeEntry
}

const (
	treeMagic         = "HUFUTREE\x00"
	treeFormatVersion = 1
)

// validateEntryName rejects every name that could escape or alias a path.
func validateEntryName(name string) error {
	switch {
	case name == "", name == ".", name == "..":
		return fmt.Errorf("%w: entry name %q", ErrInvalidTree, name)
	case strings.ContainsAny(name, "/\x00"):
		return fmt.Errorf("%w: entry name %q contains a separator or NUL", ErrInvalidTree, name)
	case !utf8.ValidString(name):
		return fmt.Errorf("%w: entry name is not valid UTF-8", ErrInvalidTree)
	}
	return nil
}

func validateEntry(entry TreeEntry) error {
	if err := validateEntryName(entry.Name); err != nil {
		return err
	}
	if !validHash(entry.ObjectHash) {
		return fmt.Errorf("%w: entry %q has malformed hash", ErrInvalidTree, entry.Name)
	}
	if entry.Size < 0 {
		return fmt.Errorf("%w: entry %q has negative size", ErrInvalidTree, entry.Name)
	}
	switch entry.Kind {
	case EntryBlob:
		if entry.Mode&^0o777 != 0 {
			return fmt.Errorf("%w: blob %q has mode %o outside 0777", ErrInvalidTree, entry.Name, entry.Mode)
		}
	case EntrySymlink:
		if entry.Mode != 0 {
			return fmt.Errorf("%w: symlink %q must have mode 0", ErrInvalidTree, entry.Name)
		}
	case EntryTree:
		if entry.Mode != 0 || entry.Size != 0 {
			return fmt.Errorf("%w: tree %q must have mode 0 and size 0", ErrInvalidTree, entry.Name)
		}
	default:
		return fmt.Errorf("%w: entry %q has unknown kind %d", ErrInvalidTree, entry.Name, entry.Kind)
	}
	return nil
}

// Validate checks entry fields and strict bytewise ordering (which also
// rules out duplicate names). Emptiness is checked by callers, because only a
// non-root tree may not be empty.
func (t TreeObject) Validate() error {
	for index, entry := range t.Entries {
		if err := validateEntry(entry); err != nil {
			return err
		}
		if index > 0 && t.Entries[index-1].Name >= entry.Name {
			return fmt.Errorf("%w: entries are unsorted or duplicated at %q", ErrInvalidTree, entry.Name)
		}
	}
	return nil
}

// EncodeTree returns the canonical v1 bytes of t and their SHA-256 hash.
func EncodeTree(t TreeObject) ([]byte, string, error) {
	if err := t.Validate(); err != nil {
		return nil, "", err
	}
	var buf bytes.Buffer
	buf.WriteString(treeMagic)
	buf.WriteByte(treeFormatVersion)
	buf.Write(binary.AppendUvarint(nil, uint64(len(t.Entries))))
	var fixed [4 + 8]byte
	for _, entry := range t.Entries {
		buf.Write(binary.AppendUvarint(nil, uint64(len(entry.Name))))
		buf.WriteString(entry.Name)
		buf.WriteByte(byte(entry.Kind))
		binary.LittleEndian.PutUint32(fixed[:4], entry.Mode)
		binary.LittleEndian.PutUint64(fixed[4:], uint64(entry.Size))
		buf.Write(fixed[:])
		raw, err := hex.DecodeString(entry.ObjectHash)
		if err != nil {
			return nil, "", fmt.Errorf("%w: entry %q hash: %v", ErrInvalidTree, entry.Name, err)
		}
		buf.Write(raw)
	}
	data := buf.Bytes()
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

// DecodeTree parses canonical v1 bytes and rejects anything a canonical
// encoder could not have produced, including trailing bytes.
func DecodeTree(data []byte) (TreeObject, error) {
	if !bytes.HasPrefix(data, []byte(treeMagic)) {
		return TreeObject{}, fmt.Errorf("%w: bad magic", ErrInvalidTree)
	}
	reader := bytes.NewReader(data[len(treeMagic):])
	version, err := reader.ReadByte()
	if err != nil || version != treeFormatVersion {
		return TreeObject{}, fmt.Errorf("%w: unsupported tree format version", ErrInvalidTree)
	}
	count, err := binary.ReadUvarint(reader)
	if err != nil || count > uint64(reader.Len()) {
		return TreeObject{}, fmt.Errorf("%w: bad entry count", ErrInvalidTree)
	}
	tree := TreeObject{Entries: make([]TreeEntry, 0, count)}
	for range count {
		entry, entryErr := decodeEntry(reader)
		if entryErr != nil {
			return TreeObject{}, entryErr
		}
		tree.Entries = append(tree.Entries, entry)
	}
	if reader.Len() != 0 {
		return TreeObject{}, fmt.Errorf("%w: trailing bytes", ErrInvalidTree)
	}
	if err = tree.Validate(); err != nil {
		return TreeObject{}, err
	}
	return tree, nil
}

func decodeEntry(reader *bytes.Reader) (TreeEntry, error) {
	nameLen, err := binary.ReadUvarint(reader)
	if err != nil || nameLen > uint64(reader.Len()) {
		return TreeEntry{}, fmt.Errorf("%w: bad name length", ErrInvalidTree)
	}
	name := make([]byte, nameLen)
	if _, err = io.ReadFull(reader, name); err != nil {
		return TreeEntry{}, fmt.Errorf("%w: truncated name", ErrInvalidTree)
	}
	kind, err := reader.ReadByte()
	if err != nil {
		return TreeEntry{}, fmt.Errorf("%w: truncated kind", ErrInvalidTree)
	}
	var fixed [4 + 8 + sha256.Size]byte
	if _, readErr := io.ReadFull(reader, fixed[:]); readErr != nil {
		return TreeEntry{}, fmt.Errorf("%w: truncated entry", ErrInvalidTree)
	}
	size := binary.LittleEndian.Uint64(fixed[4:12])
	if size > 1<<62 {
		return TreeEntry{}, fmt.Errorf("%w: entry size overflow", ErrInvalidTree)
	}
	return TreeEntry{
		Name:       string(name),
		Kind:       EntryKind(kind),
		Mode:       binary.LittleEndian.Uint32(fixed[:4]),
		Size:       int64(size),
		ObjectHash: hex.EncodeToString(fixed[12:]),
	}, nil
}
