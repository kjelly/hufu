package teampkg

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

type rawZipEntry struct {
	name   string
	data   []byte
	method uint16
	mode   os.FileMode
}

func rawZIP(t *testing.T, entries []rawZipEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, item := range entries {
		header := &zip.FileHeader{Name: item.name, Method: item.method}
		if item.mode == 0 {
			item.mode = 0o644
		}
		header.SetMode(item.mode)
		stream, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Write(item.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func validPackageFiles(t *testing.T) map[string][]byte {
	t.Helper()
	manifest, err := encodeManifest(Manifest{
		SchemaVersion: SchemaVersion, Name: "portable", Version: "v1",
		TeamManifest: "team.yaml", Authenticity: AuthenticityUnverified,
	})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		ManifestFilename: manifest,
		"team.yaml":      []byte("name: portable\n"),
		"worker.md":      []byte("---\nrole: worker\n---\nWork.\n"),
	}
	_, lock, err := buildLock(files)
	if err != nil {
		t.Fatal(err)
	}
	files[LockFilename] = lock
	return files
}

func archiveFromFiles(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	data, err := encodeArchive(files)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestReadArchiveBytesValidatesGeneratedPackage(t *testing.T) {
	archive, err := ReadArchiveBytes(archiveFromFiles(t, validPackageFiles(t)))
	if err != nil {
		t.Fatal(err)
	}
	if archive.Manifest.Name != "portable" || archive.Lock.TeamDigest == "" || len(archive.Files) != 4 {
		t.Fatalf("archive = %#v", archive)
	}
}

func TestReadArchiveBytesRejectsEntryPathAndTypeHazards(t *testing.T) {
	tests := map[string][]rawZipEntry{
		"parent":         {{name: "../escape", data: []byte("x"), method: zip.Store}},
		"absolute":       {{name: "/escape", data: []byte("x"), method: zip.Store}},
		"backslash":      {{name: `dir\escape`, data: []byte("x"), method: zip.Store}},
		"drive":          {{name: "C:/escape", data: []byte("x"), method: zip.Store}},
		"nul":            {{name: "bad\x00name", data: []byte("x"), method: zip.Store}},
		"case collision": {{name: "A.md", method: zip.Store}, {name: "a.md", method: zip.Store}},
		"duplicate":      {{name: "a.md", method: zip.Store}, {name: "a.md", method: zip.Store}},
		"symlink":        {{name: "link", data: []byte("target"), method: zip.Store, mode: os.ModeSymlink | 0o777}},
		"directory":      {{name: "dir/", method: zip.Store, mode: os.ModeDir | 0o755}},
	}
	for name, entries := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadArchiveBytes(rawZIP(t, entries)); err == nil {
				t.Fatal("unsafe archive was accepted")
			}
		})
	}
}

func TestReadArchiveBytesRejectsBoundsAndCompressionRatio(t *testing.T) {
	if _, err := ReadArchiveBytes(make([]byte, MaxArchiveBytes+1)); err == nil || !strings.Contains(err.Error(), "archive_too_large") {
		t.Fatalf("archive size error = %v", err)
	}
	large := rawZIP(t, []rawZipEntry{{name: "large", data: make([]byte, MaxFileBytes+1), method: zip.Store}})
	if _, err := ReadArchiveBytes(large); err == nil || !strings.Contains(err.Error(), "file_too_large") {
		t.Fatalf("file size error = %v", err)
	}
	bomb := rawZIP(t, []rawZipEntry{{name: "bomb", data: make([]byte, 1<<20), method: zip.Deflate}})
	if _, err := ReadArchiveBytes(bomb); err == nil || !strings.Contains(err.Error(), "compression_ratio") {
		t.Fatalf("compression ratio error = %v", err)
	}
	entries := make([]rawZipEntry, MaxEntries+1)
	for index := range entries {
		entries[index] = rawZipEntry{name: "entry-" + strconv.Itoa(index), method: zip.Store}
	}
	if _, err := ReadArchiveBytes(rawZIP(t, entries)); err == nil || !strings.Contains(err.Error(), "too_many_entries") {
		t.Fatalf("entry count error = %v", err)
	}
}

func TestReadArchiveBytesRejectsLockMismatches(t *testing.T) {
	tests := map[string]func(map[string][]byte){
		"missing": func(files map[string][]byte) { delete(files, LockFilename) },
		"hash":    func(files map[string][]byte) { files["worker.md"] = []byte("changed") },
		"extra":   func(files map[string][]byte) { files["extra.md"] = []byte("extra") },
		"size": func(files map[string][]byte) {
			var lock Lock
			if err := json.Unmarshal(files[LockFilename], &lock); err != nil {
				t.Fatal(err)
			}
			lock.Files[0].Size++
			data, err := json.Marshal(lock)
			if err != nil {
				t.Fatal(err)
			}
			files[LockFilename] = data
		},
		"duplicate lock path": func(files map[string][]byte) {
			var lock Lock
			if err := json.Unmarshal(files[LockFilename], &lock); err != nil {
				t.Fatal(err)
			}
			lock.Files[1] = lock.Files[0]
			data, err := json.Marshal(lock)
			if err != nil {
				t.Fatal(err)
			}
			files[LockFilename] = data
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			files := validPackageFiles(t)
			mutate(files)
			if _, err := ReadArchiveBytes(archiveFromFiles(t, files)); err == nil {
				t.Fatal("invalid lock was accepted")
			}
		})
	}
}

func TestReadArchiveBytesStrictMetadata(t *testing.T) {
	files := validPackageFiles(t)
	files[ManifestFilename] = append(files[ManifestFilename], []byte("unknown: true\n")...)
	_, lock, err := buildLock(map[string][]byte{
		ManifestFilename: files[ManifestFilename], "team.yaml": files["team.yaml"], "worker.md": files["worker.md"],
	})
	if err != nil {
		t.Fatal(err)
	}
	files[LockFilename] = lock
	if _, err := ReadArchiveBytes(archiveFromFiles(t, files)); err == nil || !strings.Contains(err.Error(), "field unknown not found") {
		t.Fatalf("strict metadata error = %v", err)
	}
}

func TestReadArchiveBytesRejectsSecretContentAfterValidLock(t *testing.T) {
	const secret = "sk-abcdefghijklmnopqrstuvwx"
	files := validPackageFiles(t)
	files["worker.md"] = []byte(secret)
	lockInputs := make(map[string][]byte, len(files)-1)
	for path, data := range files {
		if path != LockFilename {
			lockInputs[path] = data
		}
	}
	_, lock, err := buildLock(lockInputs)
	if err != nil {
		t.Fatal(err)
	}
	files[LockFilename] = lock
	_, err = ReadArchiveBytes(archiveFromFiles(t, files))
	if err == nil || !strings.Contains(err.Error(), "known_token_prefix") {
		t.Fatalf("secret error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("diagnostic leaked secret: %v", err)
	}
}
