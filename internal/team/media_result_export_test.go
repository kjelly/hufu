package team

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// Called after the public native MCP action fixture has completed. All
// permissions are derived from its original terminal event, not current Todo.
func testResultImageExport(t *testing.T, c *Coordinator, item *TodoItem) {
	t.Helper()
	events, err := readValidatedEventJournal(t.Context(), c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	var scope ResultImageScope
	for _, event := range events {
		if event.Type == string(EventTaskCompleted) && event.TaskID == item.ID {
			scope = ResultImageScope{BranchID: event.BranchID, RunID: event.RunID, TaskID: event.TaskID, EventID: event.ID, EventHash: event.Hash}
		}
	}
	images, err := ListResultImages(t.Context(), c.session.Workspace, scope)
	if err != nil || len(images) != 1 || images[0].Occurrences != 2 || len(images[0].Sources) != 1 {
		t.Fatalf("canonical result lost ordered image evidence: images=%+v err=%v", images, err)
	}
	id := images[0].Image.ID
	var runFinished string
	for _, event := range events {
		if event.Type == string(EventRunFinished) && event.RunID == scope.RunID {
			runFinished = event.ID
		}
	}
	report, err := LoadRunReport(t.Context(), c.session.Workspace, runFinished)
	if err != nil || report.ImageResultIDs[item.ID] == "" {
		t.Fatal("immutable report lost its image result locator", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "original.png")
	if err := ExportResultImage(t.Context(), c.session.Workspace, scope, id, path); err != nil {
		t.Fatal(err)
	}
	writer, err := NewFileArtifactStore(c.session.Workspace, c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	var unpublished bytes.Buffer
	if err := png.Encode(&unpublished, image.NewRGBA(image.Rect(0, 0, 19, 23))); err != nil {
		t.Fatal(err)
	}
	unpublishedRef, err := writer.Put(t.Context(), PutArtifactRequest{Kind: "image", Content: unpublished.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	if err := ExportResultImage(t.Context(), c.session.Workspace, scope, unpublishedRef.ID, filepath.Join(dir, "unpublished.png")); err == nil {
		t.Fatal("knowing a valid CAS image ID bypassed publication scope")
	}
	tree, err := LoadSessionTree(c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := tree.CreateRootBranch("unrelated-images")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveSessionTree(c.session.Workspace, tree); err != nil {
		t.Fatal(err)
	}
	siblingScope := scope
	siblingScope.BranchID = sibling.ID
	if _, err := ListResultImages(t.Context(), c.session.Workspace, siblingScope); err == nil {
		t.Fatal("sibling branch borrowed an image result")
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, mediaStorePNG(t)) {
		t.Fatal("export changed original image bytes", err)
	}
	if err := ExportResultImage(t.Context(), c.session.Workspace, scope, id, path); err == nil {
		t.Fatal("export overwrote an existing destination")
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, actual) {
		t.Fatal("overwrite rejection changed existing data", err)
	}
	for _, invalid := range []struct {
		name  string
		scope ResultImageScope
		id    string
	}{
		{"task", ResultImageScope{BranchID: scope.BranchID, RunID: scope.RunID, TaskID: "unrelated", EventID: scope.EventID, EventHash: scope.EventHash}, id},
		{"run", ResultImageScope{BranchID: scope.BranchID, RunID: "unrelated", TaskID: scope.TaskID, EventID: scope.EventID, EventHash: scope.EventHash}, id},
		{"hash", ResultImageScope{BranchID: scope.BranchID, RunID: scope.RunID, TaskID: scope.TaskID, EventID: scope.EventID, EventHash: "unrelated"}, id},
		{"branch", ResultImageScope{BranchID: "unrelated", RunID: scope.RunID, TaskID: scope.TaskID, EventID: scope.EventID, EventHash: scope.EventHash}, id},
		{"artifact", scope, "media-unrelated"},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "denied.png")
			if err := ExportResultImage(t.Context(), c.session.Workspace, invalid.scope, invalid.id, target); err == nil {
				t.Fatal("export accepted another source")
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatal("denied export created a destination")
			}
		})
	}
	link := filepath.Join(dir, "link.png")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := ExportResultImage(t.Context(), c.session.Workspace, scope, id, link); err == nil {
		t.Fatal("export followed a destination symlink")
	}
	parentLink := filepath.Join(dir, "linked-parent")
	if err := os.Symlink(dir, parentLink); err != nil {
		t.Fatal(err)
	}
	if err := ExportResultImage(t.Context(), c.session.Workspace, scope, id, filepath.Join(parentLink, "redirected.png")); err == nil {
		t.Fatal("export followed a parent symlink")
	}
	if _, err := os.Stat(filepath.Join(dir, "redirected.png")); !os.IsNotExist(err) {
		t.Fatal("parent symlink rejection wrote through the link")
	}
	store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.mediaPath("data", id)); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.png")
	if err := ExportResultImage(t.Context(), c.session.Workspace, scope, id, missing); err == nil {
		t.Fatal("export accepted missing canonical content")
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatal("failed verification created a destination")
	}
}

func TestImageExportDestinationAndCancellation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.png")
	if err := writeExclusiveImage(t.Context(), path, &failedImageExportReader{}); err == nil {
		t.Fatal("failed copy succeeded")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("failed export retained a partial file")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := writeExclusiveImage(ctx, path, bytes.NewReader([]byte("image"))); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled export did not stop", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("cancelled export created a file")
	}
}

type failedImageExportReader struct{}

func (*failedImageExportReader) Read([]byte) (int, error) {
	return 0, errors.New("fixture read failed")
}

// The copy boundary receives already-verified bytes. Retain one 1 MiB source
// and one destination; each iteration closes and removes its own output.
func BenchmarkImageExportCopy(b *testing.B) {
	data := bytes.Repeat([]byte{0x42}, 1<<20)
	path := filepath.Join(b.TempDir(), "image.bin")
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if err := writeExclusiveImage(b.Context(), path, bytes.NewReader(data)); err != nil {
			b.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			b.Fatal(err)
		}
	}
}
