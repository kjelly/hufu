package versionstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// makeBenchTree writes files spread over 100 files per directory.
func makeBenchTree(b *testing.B, root string, count int) {
	b.Helper()
	for index := 0; index < count; index++ {
		dir := filepath.Join(root, fmt.Sprintf("d%03d", index/100))
		if index%100 == 0 {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				b.Fatal(err)
			}
		}
		content := fmt.Sprintf("package bench\n// file %d\nvar V%d = %d\n", index, index, index)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d.go", index)), []byte(content), 0o644); err != nil {
			b.Fatal(err)
		}
	}
}

// benchStore uses a clock one hour ahead so every file is settled for the
// stat cache, which is what repeated admissions see in practice.
func benchStore(b *testing.B) *Store {
	b.Helper()
	future := time.Now().Add(time.Hour)
	store, err := Open(context.Background(), Options{StateDir: b.TempDir(), Now: func() time.Time { return future }})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	return store
}

func benchCapture(b *testing.B, store *Store, root string) Snapshot {
	b.Helper()
	snapshot, _, err := store.Capture(context.Background(), baseRequest(root))
	if err != nil {
		b.Fatal(err)
	}
	return snapshot
}

func BenchmarkCaptureFull(b *testing.B) {
	for _, count := range []int{10000, 50000} {
		b.Run(fmt.Sprintf("%dfiles", count), func(b *testing.B) {
			root := b.TempDir()
			makeBenchTree(b, root, count)
			for b.Loop() {
				b.StopTimer()
				store := benchStore(b)
				b.StartTimer()
				benchCapture(b, store, root)
			}
		})
	}
}

func BenchmarkCaptureUnchanged(b *testing.B) {
	for _, useCache := range []bool{true, false} {
		b.Run(fmt.Sprintf("100000files/statcache=%v", useCache), func(b *testing.B) {
			root := b.TempDir()
			makeBenchTree(b, root, 100000)
			store := benchStore(b)
			benchCapture(b, store, root)
			for b.Loop() {
				if !useCache {
					b.StopTimer()
					if _, err := store.db.Exec("DELETE FROM stat_cache"); err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
				}
				benchCapture(b, store, root)
			}
		})
	}
}

func BenchmarkCaptureChanged(b *testing.B) {
	cases := []struct{ files, changed int }{{10000, 1}, {50000, 10}}
	for _, tc := range cases {
		b.Run(fmt.Sprintf("%dfiles/%dchanged", tc.files, tc.changed), func(b *testing.B) {
			root := b.TempDir()
			makeBenchTree(b, root, tc.files)
			store := benchStore(b)
			benchCapture(b, store, root)
			iteration := 0
			for b.Loop() {
				b.StopTimer()
				iteration++
				for change := 0; change < tc.changed; change++ {
					path := filepath.Join(root, fmt.Sprintf("d%03d", change), fmt.Sprintf("f%05d.go", change*100))
					if err := os.WriteFile(path, []byte(fmt.Sprintf("changed %d\n", iteration)), 0o644); err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()
				benchCapture(b, store, root)
			}
		})
	}
}

func BenchmarkDiffLocalChange(b *testing.B) {
	root := b.TempDir()
	makeBenchTree(b, root, 50000)
	store := benchStore(b)
	from := benchCapture(b, store, root)
	if err := os.WriteFile(filepath.Join(root, "d250", "f25000.go"), []byte("changed\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	to := benchCapture(b, store, root)
	for b.Loop() {
		if _, err := store.Diff(context.Background(), from.ID, to.ID); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMaterialize10000(b *testing.B) {
	source := b.TempDir()
	makeBenchTree(b, source, 10000)
	store := benchStore(b)
	snapshot := benchCapture(b, store, source)
	for b.Loop() {
		b.StopTimer()
		target := b.TempDir()
		b.StartTimer()
		if _, err := store.Materialize(context.Background(), MaterializeRequest{Root: target, SnapshotID: snapshot.ID}); err != nil {
			b.Fatal(err)
		}
	}
}
