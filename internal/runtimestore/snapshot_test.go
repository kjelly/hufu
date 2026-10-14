package runtimestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
)

func runtimeFileContents(t *testing.T, workspace string) map[string]string {
	t.Helper()
	contents := make(map[string]string)
	for _, name := range dirNames(t, workspace) {
		data, err := os.ReadFile(filepath.Join(workspace, name))
		if err != nil {
			t.Fatal(err)
		}
		contents[name] = string(data)
	}
	return contents
}

func TestReadOnlySnapshotsDuringConcurrentWrites(t *testing.T) {
	workspace := t.TempDir()
	writer, err := Open(t.Context(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	write := func(value string) error {
		return writer.WithTx(t.Context(), func(tx *sql.Tx) error {
			if err := PutDocument(t.Context(), tx, DocumentSession, []byte(value)); err != nil {
				return err
			}
			return PutDocument(t.Context(), tx, DocumentSessionHistory, []byte(value))
		})
	}
	if err := write("0"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	started := make(chan struct{})
	writeErrors := make(chan error, 1)
	wg.Go(func() {
		close(started)
		for i := range 100 {
			if err := write(strconv.Itoa(i + 1)); err != nil {
				writeErrors <- err
				return
			}
		}
	})
	<-started
	successfulReads := 0
	for range 20 {
		reader, err := OpenReadOnly(t.Context(), workspace)
		if errors.Is(err, errSnapshotChanged) {
			continue
		}
		if err != nil {
			t.Errorf("concurrent snapshot: %v", err)
			break
		}
		first, exists, firstErr := reader.GetDocument(t.Context(), DocumentSession)
		second, secondExists, secondErr := reader.GetDocument(t.Context(), DocumentSessionHistory)
		if firstErr != nil || secondErr != nil || !exists || !secondExists || string(first) != string(second) {
			t.Errorf("snapshot split an atomic commit: %q/%q, exists %v/%v, errors %v/%v", first, second, exists, secondExists, firstErr, secondErr)
		}
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
		successfulReads++
	}
	wg.Wait()
	select {
	case err := <-writeErrors:
		t.Fatalf("concurrent writer: %v", err)
	default:
	}
	if successfulReads == 0 {
		t.Fatal("no read-only snapshot succeeded")
	}
}

func TestOpenReadOnlySnapshotIncludesWALAndSurvivesLaterWriter(t *testing.T) {
	workspace := t.TempDir()
	writer, err := Open(t.Context(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.PutDocument(t.Context(), DocumentSession, []byte(`before`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	before := runtimeFileContents(t, workspace)
	reader, err := OpenReadOnly(t.Context(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if after := runtimeFileContents(t, workspace); !reflect.DeepEqual(before, after) {
		t.Fatal("opening a quiescent reader changed source files")
	}
	writer, err = Open(t.Context(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if err := writer.PutDocument(t.Context(), DocumentSession, []byte(`in WAL`)); err != nil {
		t.Fatal(err)
	}
	if body, exists, err := reader.GetDocument(t.Context(), DocumentSession); err != nil || !exists || string(body) != "before" {
		t.Fatalf("original snapshot = %q, %v, %v", body, exists, err)
	}
	before = runtimeFileContents(t, workspace)
	liveReader, err := OpenReadOnly(t.Context(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	privateDir := filepath.Dir(liveReader.snapshotPath)
	if liveReader.Path() != Path(workspace) {
		t.Fatal("the snapshot changed the logical store path")
	}
	if body, exists, err := liveReader.GetDocument(t.Context(), DocumentSession); err != nil || !exists || string(body) != "in WAL" {
		t.Fatalf("reopened snapshot omitted WAL commit: %q, %v, %v", body, exists, err)
	}
	if _, err := liveReader.DB().ExecContext(t.Context(), `DELETE FROM runtime_documents`); err == nil {
		t.Fatal("the read-only snapshot accepted a direct SQL write")
	}
	if err := liveReader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := liveReader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(privateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot directory remained after Close: %v", err)
	}
	if after := runtimeFileContents(t, workspace); !reflect.DeepEqual(before, after) {
		t.Fatal("reading or closing a live-WAL snapshot changed source files")
	}
}

func TestCopyRuntimeSnapshotRejectsConcurrentChanges(t *testing.T) {
	for _, name := range []string{"WAL appears", "WAL grows", "writer closes", "database replaced", "same metadata"} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			writer, err := Open(t.Context(), workspace)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = writer.Close() })
			if err := writer.PutDocument(t.Context(), DocumentSession, []byte(`before`)); err != nil {
				t.Fatal(err)
			}
			if name != "WAL grows" && name != "writer closes" {
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			}
			path := Path(workspace)
			copyFile := func(ctx context.Context, source, target string, size int64) ([sha256.Size]byte, error) {
				digest, err := copySnapshotFile(ctx, source, target, size)
				if err != nil || source != path {
					return digest, err
				}
				switch name {
				case "WAL appears", "WAL grows":
					if name == "WAL appears" {
						writer, err = Open(ctx, workspace)
						if err != nil {
							t.Fatal(err)
						}
					}
					if err := writer.PutDocument(ctx, DocumentSession, []byte(`after`)); err != nil {
						t.Fatal(err)
					}
				case "writer closes":
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
				case "database replaced":
					replaceDatabase(t, workspace, "after")
				case "same metadata":
					info, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					data[len(data)-1] ^= 1
					if err := os.WriteFile(path, data, info.Mode().Perm()); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
				}
				return digest, nil
			}
			if err := copyRuntimeSnapshot(t.Context(), path, t.TempDir(), copyFile); !errors.Is(err, errSnapshotChanged) {
				t.Fatalf("copy accepted a changing source: %v", err)
			}
		})
	}
}

func TestRuntimeSnapshotFailsClosedAndCleansUp(t *testing.T) {
	workspace := t.TempDir()
	writer, err := Open(t.Context(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", t.TempDir())
	assertNoSnapshot := func() {
		t.Helper()
		if entries := dirNames(t, os.TempDir()); len(entries) != 0 {
			t.Fatalf("failed open leaked snapshots: %v", entries)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := OpenReadOnly(ctx, workspace); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled open = %v", err)
	}
	assertNoSnapshot()
	if err := os.WriteFile(Path(workspace)+"-journal", []byte("unfinished rollback"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(t.Context(), workspace); !errors.Is(err, errSnapshotChanged) {
		t.Fatalf("open ignored a rollback journal: %v", err)
	}
	assertNoSnapshot()
	if err := os.Remove(Path(workspace) + "-journal"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(workspace), []byte("not SQLite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(t.Context(), workspace); err == nil {
		t.Fatal("open accepted a malformed snapshot")
	}
	assertNoSnapshot()
}
