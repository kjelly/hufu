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

// BenchmarkCaptureFromDelta is the incremental Merkle rewrite of §15.2 for
// the same change set as BenchmarkCaptureChanged/50000files/10changed.
func BenchmarkCaptureFromDelta(b *testing.B) {
	const files, changed = 50000, 10
	root := b.TempDir()
	makeBenchTree(b, root, files)
	store := benchStore(b)
	parent := benchCapture(b, store, root)
	if _, err := store.PublishSnapshot(context.Background(), parent.ID, "evt-bench", 0); err != nil {
		b.Fatal(err)
	}
	req := baseRequest(root)
	req.Parent, req.Reason = parent.ID, SnapshotManual
	iteration := 0
	for b.Loop() {
		b.StopTimer()
		iteration++
		var delta ObservedDelta
		for change := 0; change < changed; change++ {
			rel := fmt.Sprintf("d%03d/f%05d.go", change, change*100)
			content := fmt.Sprintf("changed %d\n", iteration)
			if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
				b.Fatal(err)
			}
			delta.Modified = append(delta.Modified, ObservedFile{Path: rel, SHA256: sha(content)})
		}
		b.StartTimer()
		if _, _, err := store.CaptureFromDelta(context.Background(), req, delta); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBranchesShareBaseline records the storage cost of 100 branches
// that each change one file of a shared 1000-file baseline.
func BenchmarkBranchesShareBaseline(b *testing.B) {
	const files, branches = 1000, 100
	for b.Loop() {
		b.StopTimer()
		root := b.TempDir()
		makeBenchTree(b, root, files)
		store := benchStore(b)
		b.StartTimer()
		baseline := benchCapture(b, store, root)
		logical := baseline.LogicalBytes
		for branch := 0; branch < branches; branch++ {
			rel := filepath.Join(fmt.Sprintf("d%03d", branch%10), fmt.Sprintf("f%05d.go", (branch%10)*100))
			if err := os.WriteFile(filepath.Join(root, rel), []byte(fmt.Sprintf("branch %d\n", branch)), 0o644); err != nil {
				b.Fatal(err)
			}
			req := baseRequest(root)
			req.BranchID = fmt.Sprintf("b%03d", branch)
			snapshot, _, err := store.Capture(context.Background(), req)
			if err != nil {
				b.Fatal(err)
			}
			logical += snapshot.LogicalBytes
		}
		b.StopTimer()
		_, physical, err := store.StoreUsage()
		if err != nil {
			b.Fatal(err)
		}
		info, err := os.Stat(store.layout.dbPath())
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(logical), "logical-bytes")
		b.ReportMetric(float64(physical), "cas-bytes")
		b.ReportMetric(float64(logical)/float64(physical), "dedup-ratio")
		b.ReportMetric(float64(info.Size()), "sqlite-bytes")
		b.StartTimer()
	}
}
