package versionstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

// observe describes rel as an observer that just read it would.
func observe(t *testing.T, root, rel string) ObservedFile {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Lstat(full)
	if err != nil {
		t.Fatal(err)
	}
	observed := ObservedFile{Path: rel, Bytes: info.Size(), Mode: uint32(info.Mode().Perm())}
	if info.Mode().IsRegular() {
		data, readErr := os.ReadFile(full)
		if readErr != nil {
			t.Fatal(readErr)
		}
		observed.SHA256 = sha(string(data))
	}
	return observed
}

func mustRemove(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// publishedParent captures and publishes root as the delta's base.
func publishedParent(t *testing.T, store *Store, req CaptureRequest) CaptureRequest {
	t.Helper()
	parent := mustCapture(t, store, req)
	publish(t, store, parent, 0)
	next := req
	next.Parent, next.Reason = parent.ID, SnapshotManual
	return next
}

func assertSameCapture(t *testing.T, got, want Snapshot) {
	t.Helper()
	if got.RootTreeHash != want.RootTreeHash || got.ManifestDigest != want.ManifestDigest || got.FileCount != want.FileCount ||
		got.LogicalBytes != want.LogicalBytes || got.Materializable != want.Materializable {
		t.Fatalf("delta capture = %+v\nfull capture  = %+v", got, want)
	}
	if got.Parent != want.Parent || got.State != StatePending {
		t.Fatalf("delta capture parent/state = %s/%s, want %s/pending", got.Parent, got.State, want.Parent)
	}
}

// TestCaptureFromDeltaMatchesFullCapture: applying a delta to the parent tree
// yields exactly the snapshot a full capture of the same files records (§15).
func TestCaptureFromDeltaMatchesFullCapture(t *testing.T) {
	base := map[string]string{"a.txt": "a", "src/pkg/b.go": "b", "src/c.go": "c", "only/one.txt": "1", "gone/x": "x", "gone/sub/y": "y", "d/x": "dx"}
	tests := []struct {
		name    string
		exclude []string
		setup   func(t *testing.T, root string)
		mutate  func(t *testing.T, root string) ObservedDelta
	}{
		{name: "modify a nested file", mutate: func(t *testing.T, root string) ObservedDelta {
			writeFiles(t, root, map[string]string{"src/pkg/b.go": "b2"})
			return ObservedDelta{Modified: []ObservedFile{observe(t, root, "src/pkg/b.go")}}
		}},
		{name: "add below new directories", mutate: func(t *testing.T, root string) ObservedDelta {
			writeFiles(t, root, map[string]string{"new/deep/x.go": "x"})
			return ObservedDelta{Added: []ObservedFile{observe(t, root, "new/deep/x.go")}}
		}},
		{name: "delete the last file of a directory", mutate: func(t *testing.T, root string) ObservedDelta {
			mustRemove(t, root, "only")
			return ObservedDelta{Deleted: []string{"only/one.txt"}}
		}},
		{name: "delete a whole directory", mutate: func(t *testing.T, root string) ObservedDelta {
			mustRemove(t, root, "gone")
			return ObservedDelta{Deleted: []string{"gone"}}
		}},
		{name: "file becomes a directory", mutate: func(t *testing.T, root string) ObservedDelta {
			mustRemove(t, root, "a.txt")
			writeFiles(t, root, map[string]string{"a.txt/inner": "i"})
			return ObservedDelta{Deleted: []string{"a.txt"}, Added: []ObservedFile{observe(t, root, "a.txt/inner")}}
		}},
		{name: "directory becomes a file", mutate: func(t *testing.T, root string) ObservedDelta {
			mustRemove(t, root, "d")
			writeFiles(t, root, map[string]string{"d": "now a file"})
			return ObservedDelta{Deleted: []string{"d/x"}, Added: []ObservedFile{observe(t, root, "d")}}
		}},
		{name: "permission change comes from the file", mutate: func(t *testing.T, root string) ObservedDelta {
			if err := os.Chmod(filepath.Join(root, "a.txt"), 0o755); err != nil {
				t.Fatal(err)
			}
			observed := observe(t, root, "a.txt")
			observed.Mode = 0o600 // an observer's mode is never trusted
			return ObservedDelta{Modified: []ObservedFile{observed}}
		}},
		{name: "symlinks are read, escaping ones make it unmaterializable", mutate: func(t *testing.T, root string) ObservedDelta {
			mustSymlink(t, "a.txt", filepath.Join(root, "link"))
			mustSymlink(t, "/etc", filepath.Join(root, "src", "out"))
			return ObservedDelta{Added: []ObservedFile{{Path: "link", SHA256: "not checked for symlinks"}, {Path: "src/out"}}}
		}},
		{
			name: "excluded and ignored paths are skipped", exclude: []string{"control"},
			setup: func(t *testing.T, root string) {
				writeFiles(t, root, map[string]string{HufuignoreFile: "*.log\n"})
			},
			mutate: func(t *testing.T, root string) ObservedDelta {
				writeFiles(t, root, map[string]string{"debug.log": "l", "control/session.json": "{}", "vendor/.git": "gitdir: x", materializeTempPrefix + "1": "t", "kept.go": "k"})
				var delta ObservedDelta
				for _, rel := range []string{"debug.log", "control/session.json", "vendor/.git", materializeTempPrefix + "1", "kept.go"} {
					delta.Added = append(delta.Added, observe(t, root, rel))
				}
				return delta
			},
		},
		{name: "deleting an unknown path is a no-op", mutate: func(t *testing.T, root string) ObservedDelta {
			return ObservedDelta{Deleted: []string{"never/existed"}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openTestStore(t)
			root := t.TempDir()
			writeFiles(t, root, base)
			if tt.setup != nil {
				tt.setup(t, root)
			}
			req := baseRequest(root)
			req.ExcludeSubtrees = tt.exclude
			next := publishedParent(t, store, req)
			delta := tt.mutate(t, root)
			got, _, err := store.CaptureFromDelta(context.Background(), next, delta)
			if err != nil {
				t.Fatalf("CaptureFromDelta: %v", err)
			}
			assertSameCapture(t, got, mustCapture(t, store, next))
		})
	}
}

// TestCaptureFromDeltaFailsClosed: a delta that no longer describes the files
// fails instead of capturing later bytes, and records nothing.
func TestCaptureFromDeltaFailsClosed(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(t *testing.T, root string) ObservedDelta
		wantErr  error
		wantText string
	}{
		{name: "content changed after observation", wantErr: ErrWorkspaceChangedAfterObservation, mutate: func(t *testing.T, root string) ObservedDelta {
			writeFiles(t, root, map[string]string{"a.txt": "written later"})
			return ObservedDelta{Modified: []ObservedFile{{Path: "a.txt", SHA256: sha("observed")}}}
		}},
		{name: "missing observed hash", wantErr: ErrWorkspaceChangedAfterObservation, mutate: func(t *testing.T, root string) ObservedDelta {
			return ObservedDelta{Modified: []ObservedFile{{Path: "a.txt"}}}
		}},
		{name: "deleted path exists again", wantErr: ErrWorkspaceChangedAfterObservation, mutate: func(t *testing.T, root string) ObservedDelta {
			return ObservedDelta{Deleted: []string{"a.txt"}}
		}},
		{name: "changed path is gone", wantErr: ErrWorkspaceChangedAfterObservation, mutate: func(t *testing.T, root string) ObservedDelta {
			return ObservedDelta{Added: []ObservedFile{{Path: "missing/x.txt", SHA256: sha("x")}}}
		}},
		{name: "changed path became a directory", wantErr: ErrWorkspaceChangedAfterObservation, mutate: func(t *testing.T, root string) ObservedDelta {
			writeFiles(t, root, map[string]string{"dir/inner": "i"})
			return ObservedDelta{Added: []ObservedFile{{Path: "dir", SHA256: sha("x")}}}
		}},
		{name: "path below a symlinked directory", wantErr: ErrWorkspaceChangedAfterObservation, mutate: func(t *testing.T, root string) ObservedDelta {
			mustSymlink(t, "src", filepath.Join(root, "alias"))
			writeFiles(t, root, map[string]string{"src/new.go": "n"})
			return ObservedDelta{Added: []ObservedFile{observe(t, root, "alias/new.go")}}
		}},
		{name: "unsupported file type", wantErr: ErrUnsupportedFileType, mutate: func(t *testing.T, root string) ObservedDelta {
			if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
				t.Skipf("mkfifo: %v", err)
			}
			return ObservedDelta{Added: []ObservedFile{{Path: "pipe"}}}
		}},
		{name: "unclean path", wantText: "not a clean root-relative path", mutate: func(t *testing.T, root string) ObservedDelta {
			return ObservedDelta{Deleted: []string{"src/../a.txt"}}
		}},
		{name: "invalid entry name", wantText: "contains a separator or NUL", mutate: func(t *testing.T, root string) ObservedDelta {
			return ObservedDelta{Added: []ObservedFile{{Path: "bad\x00name"}}}
		}},
		{name: "hufuignore edit", wantText: "changes the inclusion policy", mutate: func(t *testing.T, root string) ObservedDelta {
			writeFiles(t, root, map[string]string{HufuignoreFile: "*.go\n"})
			return ObservedDelta{Added: []ObservedFile{observe(t, root, HufuignoreFile)}}
		}},
		{name: "nested gitignore deletion", wantText: "changes the inclusion policy", mutate: func(t *testing.T, root string) ObservedDelta {
			return ObservedDelta{Deleted: []string{"src/.gitignore"}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := openTestStore(t)
			root := t.TempDir()
			writeFiles(t, root, map[string]string{"a.txt": "a", "src/b.go": "b"})
			next := publishedParent(t, store, baseRequest(root))
			delta := tt.mutate(t, root)
			before, err := store.ListSnapshots(ctx, SnapshotFilter{})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = store.CaptureFromDelta(ctx, next, delta)
			switch {
			case tt.wantErr != nil && !errors.Is(err, tt.wantErr):
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			case tt.wantText != "" && (err == nil || !strings.Contains(err.Error(), tt.wantText)):
				t.Fatalf("err = %v, want %q", err, tt.wantText)
			}
			after, err := store.ListSnapshots(ctx, SnapshotFilter{})
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before) {
				t.Fatalf("a failed delta capture recorded a snapshot (%d -> %d rows)", len(before), len(after))
			}
		})
	}
}

func TestCaptureFromDeltaRequiresPublishedParent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a.txt": "a"})
	req := baseRequest(root)
	if _, _, err := store.CaptureFromDelta(ctx, req, ObservedDelta{}); err == nil || !strings.Contains(err.Error(), "published parent") {
		t.Fatalf("no parent: err = %v", err)
	}
	req.Parent = mustCapture(t, store, req).ID
	if _, _, err := store.CaptureFromDelta(ctx, req, ObservedDelta{}); !errors.Is(err, ErrWorkspaceSnapshotUnavailable) {
		t.Fatalf("pending parent: err = %v, want ErrWorkspaceSnapshotUnavailable", err)
	}
}

// TestCaptureFromDeltaRewritesOnlyAffectedTrees covers §15.2: changing one
// file writes one blob and only the trees on its path.
func TestCaptureFromDeltaRewritesOnlyAffectedTrees(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	files := map[string]string{}
	for dir := 0; dir < 20; dir++ {
		for file := 0; file < 5; file++ {
			files[fmt.Sprintf("pkg%02d/sub/file%d.go", dir, file)] = fmt.Sprintf("%d-%d", dir, file)
		}
	}
	writeFiles(t, root, files)
	next := publishedParent(t, store, baseRequest(root))
	writeFiles(t, root, map[string]string{"pkg07/sub/file3.go": "changed"})

	writes := &atomic.Int64{}
	store.treeWrites = writes
	got, stats, err := store.CaptureFromDelta(context.Background(), next, ObservedDelta{Modified: []ObservedFile{observe(t, root, "pkg07/sub/file3.go")}})
	if err != nil {
		t.Fatal(err)
	}
	store.treeWrites = nil
	// pkg07/sub, pkg07, and the root; the 19 untouched packages keep their hashes.
	if n := writes.Load(); n != 3 {
		t.Fatalf("tree writes = %d, want 3", n)
	}
	if stats.NewCASBytes == 0 || stats.ReusedCASBytes != 0 {
		t.Fatalf("stats = %+v, want only new bytes for the changed file and its trees", stats)
	}
	assertSameCapture(t, got, mustCapture(t, store, next))
}

func TestCaptureFromDeltaGitPolicy(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	gitInit(t, root)
	writeFiles(t, root, map[string]string{".gitignore": "*.log\n", "keep.log": "k", "main.go": "m"})
	cmd := exec.Command("git", "add", "-f", ".gitignore", "keep.log", "main.go")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	next := publishedParent(t, store, baseRequest(root))
	writeFiles(t, root, map[string]string{"keep.log": "k2", "debug.log": "d", "new.go": "n", "nested/file.go": "f"})
	gitInit(t, filepath.Join(root, "nested"))
	delta := ObservedDelta{Modified: []ObservedFile{observe(t, root, "keep.log")}}
	for _, rel := range []string{"debug.log", "new.go", "nested/file.go"} {
		delta.Added = append(delta.Added, observe(t, root, rel))
	}
	got, _, err := store.CaptureFromDelta(context.Background(), next, delta)
	if err != nil {
		t.Fatal(err)
	}
	want := mustCapture(t, store, next)
	assertSameCapture(t, got, want)
	// A tracked file is managed even when it matches an ignore pattern.
	if paths := leafPaths(t, store, got); strings.Join(paths, ",") != ".gitignore,keep.log,main.go,new.go" {
		t.Fatalf("paths = %v", paths)
	}
}
