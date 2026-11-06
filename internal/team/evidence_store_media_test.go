package team

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/media"
)

func mediaStorePNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Fixtures own this directory; normalize its trusted temporary root so OS
// aliases such as /var do not masquerade as an untrusted input symlink.
func mediaStoreDirectory(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestMediaArtifactStore(t *testing.T) {
	workspace := mediaStoreDirectory(t)
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	data := mediaStorePNG(t)
	first, err := store.Put(t.Context(), PutArtifactRequest{Kind: "image", Content: data, TaskID: "first", RunID: "first-run", Role: "input"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(t.Context(), PutArtifactRequest{Kind: "image", Content: data, TaskID: "second", RunID: "other-run", Role: "output"})
	if err != nil {
		t.Fatal(err)
	}
	if !sameArtifactRef(first.ArtifactRef, second.ArtifactRef) || second.CreatedData || second.CreatedMetadata || !first.CreatedData || !first.CreatedMetadata {
		t.Fatalf("incorrect deduplication: %+v %+v", first, second)
	}
	if first.TaskID != "" || first.RunID != "" || first.Role != "media" || first.Path != "media/image/"+first.ID {
		t.Fatal("CAS retained occurrence authority")
	}
	readOnly, err := OpenFileArtifactStoreReadOnly(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := readOnly.Resolve(t.Context(), ArtifactRef{ID: first.ID})
	if err != nil || !sameArtifactRef(canonical, first.ArtifactRef) {
		t.Fatalf("resolve: %v", err)
	}
	reader, err := readOnly.Open(t.Context(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatalf("read: %v", err)
	}
	for _, entry := range []struct {
		path string
		mode os.FileMode
	}{
		{filepath.Join(store.root, "data"), 0o755}, {filepath.Join(store.root, "meta"), 0o755},
		{filepath.Join(store.root, "media"), 0o700}, {filepath.Join(store.root, "media", "data"), 0o700}, {filepath.Join(store.root, "media", "meta"), 0o700},
		{store.mediaPath("data", first.ID), 0o600}, {store.mediaPath("meta", first.ID+".json"), 0o600},
	} {
		info, err := os.Stat(entry.path)
		if err != nil || info.Mode().Perm() != entry.mode {
			t.Fatalf("mode %s: info=%v err=%v", entry.path, info, err)
		}
	}
	if _, err := readOnly.Put(t.Context(), PutArtifactRequest{Kind: "image", Content: data}); err == nil {
		t.Fatal("read-only mutation accepted")
	}
	if _, err := store.Resolve(t.Context(), ArtifactRef{SHA256: first.SHA256}); err == nil {
		t.Fatal("digest-only private lookup accepted")
	}
	if _, err := store.Resolve(t.Context(), ArtifactRef{ID: first.ID, TaskID: "guessed-owner"}); err == nil {
		t.Fatal("ownership claim accepted")
	}
	if err := store.Verify(t.Context(), ArtifactRef{ID: first.ID}); err == nil {
		t.Fatal("sparse reference verified without immutable claims")
	}
	occurrence, err := projectArtifactOccurrence(t.Context(), store, first.ArtifactRef, "run", "task", 1, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Verify(t.Context(), occurrence); err != nil {
		t.Fatalf("manifest occurrence rejected: %v", err)
	}
	occurrence.SHA256 = "changed"
	if err := store.Verify(t.Context(), occurrence); err == nil {
		t.Fatal("occurrence masked changed content metadata")
	}
	if _, err := store.TrustedArtifactMetadata(t.Context(), first.ArtifactRef); err != nil {
		t.Fatal(err)
	}
}

func TestMediaArtifactStoreRejectsConflicts(t *testing.T) {
	for _, scenario := range []string{"mime", "data", "metadata", "namespace", "file-symlink", "source-symlink"} {
		t.Run(scenario, func(t *testing.T) {
			workspace := mediaStoreDirectory(t)
			store, err := NewFileArtifactStore(workspace, workspace)
			if err != nil {
				t.Fatal(err)
			}
			data := mediaStorePNG(t)
			ref, err := store.Put(t.Context(), PutArtifactRequest{Kind: "image", Content: data})
			if err != nil {
				t.Fatal(err)
			}
			request := PutArtifactRequest{Kind: "image", Content: data}
			switch scenario {
			case "mime":
				request.MediaType = "image/jpeg"
			case "data":
				if err := os.WriteFile(store.mediaPath("data", ref.ID), []byte("changed"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "metadata":
				metadata, err := store.mediaMetadata(t.Context(), ref.ID)
				if err != nil {
					t.Fatal(err)
				}
				metadata.TaskID = "other-owner"
				encoded, err := json.Marshal(metadata)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(store.mediaPath("meta", ref.ID+".json"), encoded, 0o600); err != nil {
					t.Fatal(err)
				}
			case "namespace":
				if err := os.Chmod(filepath.Join(store.root, "media"), 0o755); err != nil {
					t.Fatal(err)
				}
			case "file-symlink":
				path := store.mediaPath("data", ref.ID)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(workspace, "unrelated"), path); err != nil {
					t.Fatal(err)
				}
			case "source-symlink":
				path := filepath.Join(workspace, "source.png")
				if err := os.Symlink(store.mediaPath("data", ref.ID), path); err != nil {
					t.Fatal(err)
				}
				request.Content = nil
				request.SourcePath = path
			}
			if _, err := store.Put(t.Context(), request); err == nil {
				t.Fatal("conflicting publication accepted")
			}
			if scenario == "data" || scenario == "metadata" || scenario == "file-symlink" {
				if err := store.Verify(t.Context(), ref.ArtifactRef); err == nil {
					t.Fatal("tampered content verified")
				}
				if _, err := store.Open(t.Context(), ref.ID); err == nil {
					t.Fatal("tampered content opened")
				}
			}
		})
	}
}

func TestMediaPublicationWithoutOccurrenceGrantsNoTaskOwnership(t *testing.T) {
	workspace := mediaStoreDirectory(t)
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(t.Context(), PutArtifactRequest{Kind: "image", Content: mediaStorePNG(t), TaskID: "not-committed"})
	if err != nil {
		t.Fatal(err)
	}
	refs, err := store.ListByTask(t.Context(), "not-committed")
	if err != nil || len(refs) != 0 {
		t.Fatalf("uncommitted media granted ownership: %v", err)
	}
	if ref.TaskID != "" {
		t.Fatal("first-writer owns media")
	}
	// Data remains available for idempotent publication after a crash; no
	// cleanup may remove a blob based solely on a failed occurrence commit.
	if err := store.Verify(t.Context(), ref.ArtifactRef); err != nil {
		t.Fatal(err)
	}
	// Reproduce interruption between data and metadata publication. Reads do
	// not invent metadata, while an identical trusted publication repairs it.
	if err := os.Remove(store.mediaPath("meta", ref.ID+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(t.Context(), ref.ID); err == nil {
		t.Fatal("missing metadata was silently repaired on read")
	}
	retry, err := store.Put(t.Context(), PutArtifactRequest{Kind: "image", Content: mediaStorePNG(t)})
	if err != nil || retry.CreatedData || !retry.CreatedMetadata {
		t.Fatalf("incomplete publication repair: %+v %v", retry, err)
	}
	_, err = store.Put(t.Context(), PutArtifactRequest{Kind: "image", Content: []byte("invalid")})
	if _, ok := errors.AsType[*media.Error](err); !ok {
		t.Fatalf("unclassified validation failure: %v", err)
	}
}

func TestMediaReaderRetainsReservationUntilClosed(t *testing.T) {
	workspace := mediaStoreDirectory(t)
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(t.Context(), PutArtifactRequest{Kind: "image", Content: mediaStorePNG(t)})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := store.Open(t.Context(), ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	remainder, err := media.ReserveTransport(t.Context(), media.MaxConcurrentRawBytes-ref.ByteSize)
	if err != nil {
		t.Fatal(err)
	}
	defer remainder.Release()
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	last, err := media.ReserveTransport(ctx, ref.ByteSize)
	if err != nil {
		t.Fatalf("reader leaked reservation: %v", err)
	}
	last.Release()
}

func TestMediaStoreRespectsAggregateReadBudget(t *testing.T) {
	workspace := mediaStoreDirectory(t)
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(t.Context(), PutArtifactRequest{Kind: "image", Content: mediaStorePNG(t)})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := os.Stat(store.mediaPath("meta", ref.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		bytes int64
		pass  bool
	}{
		{name: "insufficient", bytes: 1},
		{name: "exact metadata and content", bytes: metadata.Size() + ref.ByteSize, pass: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, budget := WithArtifactReadBudget(t.Context(), test.bytes)
			reader, err := store.Open(ctx, ref.ID)
			if reader != nil {
				t.Cleanup(func() { _ = reader.Close() })
			}
			if test.pass {
				if err != nil || budget.Err() != nil {
					t.Fatalf("exact image read budget was charged more than once: %v", err)
				}
			} else if !errors.Is(err, ErrReadLimit) || !errors.Is(budget.Err(), ErrReadLimit) {
				t.Fatalf("budget bypass: %v, %v", err, budget.Err())
			}
		})
	}
}

func TestMediaReservedReaderUsesBundleCreditAndVerifiesContent(t *testing.T) {
	for _, variant := range []string{"valid", "metadata conflict", "corrupt content"} {
		t.Run(variant, func(t *testing.T) {
			workspace := mediaStoreDirectory(t)
			store, err := NewFileArtifactStore(workspace, workspace)
			if err != nil {
				t.Fatal(err)
			}
			published, err := store.Put(t.Context(), PutArtifactRequest{Kind: "image", Content: mediaStorePNG(t)})
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := store.mediaMetadata(t.Context(), published.ID)
			if err != nil {
				t.Fatal(err)
			}
			expected := *metadata.Image
			if variant == "metadata conflict" {
				expected.ByteSize++
			} else if variant == "corrupt content" {
				if err := os.WriteFile(store.mediaPath("data", published.ID), []byte("corrupt fixture"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// Holding all raw credit proves this reader does not nest another
			// storage reservation underneath the caller's bundle reservation.
			reservation, err := media.ReserveTransportFrames(t.Context(), media.MaxConcurrentRawBytes)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(reservation.Release)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			data, err := store.readReservedMedia(ctx, expected, media.Limits{})
			t.Cleanup(func() { clear(data) })
			if variant == "valid" {
				if err != nil || !bytes.Equal(data, mediaStorePNG(t)) {
					t.Fatal("reserved reader changed content or acquired nested credit", err)
				}
			} else if err == nil || data != nil {
				t.Fatal("reserved reader returned unverified image bytes")
			}
		})
	}
}

func TestTaskArtifactOpenerRefusesNativeMedia(t *testing.T) {
	c := newTestCoordinator(t)
	if _, err := c.openArtifactRef(t.Context(), media.ContentID("digest", "image/png")); err == nil {
		t.Fatal("text artifact opener accepted native media")
	}
}
