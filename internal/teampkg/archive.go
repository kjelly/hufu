package teampkg

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Archive is a fully bounded, hash-verified V1 team package held in memory.
type Archive struct {
	Manifest      Manifest
	Lock          Lock
	Files         map[string][]byte
	ArchiveSHA256 string
	ArchiveSize   int64
}

// ReadArchive reads and validates a local package without extracting it.
func ReadArchive(filename string) (*Archive, error) {
	data, err := readArchiveData(filename)
	if err != nil {
		return nil, err
	}
	return ReadArchiveBytes(data)
}

func readArchiveData(filename string) ([]byte, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open team package: %w", err)
	}
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat team package: %w", statErr)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, validationError(filename, validationFieldArchive, "archive_not_regular")
	}
	if info.Size() > MaxArchiveBytes {
		_ = file.Close()
		return nil, validationError(filename, validationFieldArchive, "archive_too_large")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, int64(MaxArchiveBytes)+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read team package: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close team package: %w", closeErr)
	}
	if len(data) > MaxArchiveBytes {
		return nil, validationError(filename, validationFieldArchive, "archive_too_large")
	}
	return data, nil
}

// ReadArchiveBytes validates archive structure, bounds, metadata, lock hashes,
// and source secret policy without extracting any entry.
func ReadArchiveBytes(data []byte) (*Archive, error) {
	if len(data) > MaxArchiveBytes {
		return nil, validationError("", validationFieldArchive, "archive_too_large")
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open team package ZIP: %w", err)
	}
	if len(reader.File) > MaxEntries {
		return nil, validationError("", validationFieldArchive, "too_many_entries")
	}
	registry := newPathRegistry()
	files := make(map[string][]byte, len(reader.File))
	var total uint64
	for _, file := range reader.File {
		if err := registry.add(file.Name); err != nil {
			return nil, err
		}
		if file.FileInfo().IsDir() || !file.Mode().IsRegular() {
			return nil, validationError(file.Name, "mode", "entry_not_regular")
		}
		if file.UncompressedSize64 > MaxFileBytes {
			return nil, validationError(file.Name, "size", "file_too_large")
		}
		total += file.UncompressedSize64
		if total > MaxTotalBytes {
			return nil, validationError(file.Name, "size", "total_too_large")
		}
		if file.UncompressedSize64 > 0 && (file.CompressedSize64 == 0 || file.UncompressedSize64 > file.CompressedSize64*MaxCompressionRatio) {
			return nil, validationError(file.Name, "compression", "compression_ratio")
		}
		stream, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("open package entry %q: %w", file.Name, err)
		}
		content, readErr := io.ReadAll(io.LimitReader(stream, MaxFileBytes+1))
		closeErr := stream.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read package entry %q: %w", file.Name, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close package entry %q: %w", file.Name, closeErr)
		}
		if len(content) > MaxFileBytes || uint64(len(content)) != file.UncompressedSize64 {
			return nil, validationError(file.Name, "size", "entry_size_mismatch")
		}
		files[file.Name] = content
	}
	manifestData, ok := files[ManifestFilename]
	if !ok {
		return nil, validationError(ManifestFilename, "metadata", "package_manifest_missing")
	}
	if err := scanSourceContent(ManifestFilename, manifestData); err != nil {
		return nil, err
	}
	lockData, ok := files[LockFilename]
	if !ok {
		return nil, validationError(LockFilename, "metadata", "lock_missing")
	}
	manifest, err := decodeManifest(manifestData)
	if err != nil {
		return nil, err
	}
	lock, err := decodeLock(lockData)
	if err != nil {
		return nil, err
	}
	if err := verifyLock(lock, files); err != nil {
		return nil, err
	}
	if err := validateSourceFiles(manifest.TeamManifest, files); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	return &Archive{
		Manifest: manifest, Lock: lock, Files: files,
		ArchiveSHA256: hex.EncodeToString(sum[:]), ArchiveSize: int64(len(data)),
	}, nil
}

func decodeManifest(data []byte) (Manifest, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode package manifest: %w", err)
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Manifest{}, validationError(ManifestFilename, "document", "multiple_yaml_documents")
		}
		return Manifest{}, fmt.Errorf("decode package manifest: %w", err)
	}
	if manifest.SchemaVersion != SchemaVersion {
		return Manifest{}, validationError(ManifestFilename, "schema_version", "unsupported_schema")
	}
	if manifest.Authenticity != AuthenticityUnverified {
		return Manifest{}, validationError(ManifestFilename, "authenticity", "unsupported_authenticity")
	}
	if err := validateManifestIdentity(manifest.Name, manifest.TeamManifest, manifest.Version); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func decodeLock(data []byte) (Lock, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var lock Lock
	if err := decoder.Decode(&lock); err != nil {
		return Lock{}, fmt.Errorf("decode package lock: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return Lock{}, validationError(LockFilename, "document", "multiple_json_values")
		}
		return Lock{}, fmt.Errorf("decode package lock: %w", err)
	}
	if lock.SchemaVersion != SchemaVersion {
		return Lock{}, validationError(LockFilename, "schema_version", "unsupported_schema")
	}
	return lock, nil
}

func verifyLock(lock Lock, files map[string][]byte) error {
	actual := make(map[string][]byte, len(files)-1)
	for path, data := range files {
		if path != LockFilename {
			actual[path] = data
		}
	}
	expected, _, err := buildLock(actual)
	if err != nil {
		return err
	}
	if len(lock.Files) != len(expected.Files) {
		return validationError(LockFilename, "files", "lock_file_set_mismatch")
	}
	for index, file := range lock.Files {
		if index > 0 && lock.Files[index-1].Path >= file.Path {
			return validationError(LockFilename, "files", "lock_paths_not_strictly_sorted")
		}
		if err := ValidateEntryPath(file.Path); err != nil {
			return err
		}
		if len(file.SHA256) != sha256.Size*2 {
			return validationError(LockFilename, "files.sha256", "lock_hash_invalid")
		}
		if _, err := hex.DecodeString(file.SHA256); err != nil || strings.ToLower(file.SHA256) != file.SHA256 {
			return validationError(LockFilename, "files.sha256", "lock_hash_invalid")
		}
		want := expected.Files[index]
		if file.Path != want.Path {
			return validationError(file.Path, "lock.path", "lock_file_set_mismatch")
		}
		if file.Size != want.Size {
			return validationError(file.Path, "lock.size", "lock_size_mismatch")
		}
		if file.SHA256 != want.SHA256 {
			return validationError(file.Path, "lock.sha256", "lock_hash_mismatch")
		}
	}
	if lock.TeamDigest != expected.TeamDigest {
		return validationError(LockFilename, "team_digest", "team_digest_mismatch")
	}
	return nil
}
