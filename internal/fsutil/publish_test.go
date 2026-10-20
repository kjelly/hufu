//go:build linux || darwin || windows

package fsutil

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPublishDirectoryConcurrentNoReplace(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "published")
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, name := range []string{"first", "second"} {
		source := filepath.Join(parent, name)
		if err := os.Mkdir(source, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, "content"), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		wait.Go(func() { results <- PublishDirectoryNoReplace(source, target) })
	}
	wait.Wait()
	close(results)
	var successes int
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly one exclusive publication, got %d", successes)
	}
	raw, err := os.ReadFile(filepath.Join(target, "content"))
	if err != nil || string(raw) != "first" && string(raw) != "second" {
		t.Fatal("published directory is incomplete")
	}
}
