package team

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/media"
)

func TestCodexProjectionConsumerRejectsUnsafeMarkers(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Codex ProcessSupervisor is not implemented on Windows")
	}
	for _, scenario := range []string{"valid stale", "nonempty", "public", "symlink", "directory", "invalid name"} {
		t.Run(scenario, func(t *testing.T) {
			path := t.TempDir()
			root, err := os.OpenRoot(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			name := codexProjectionConsumerPrefix + rand.Text()
			if scenario == "invalid name" {
				name = codexProjectionConsumerPrefix + "unknown"
			}
			switch scenario {
			case "directory":
				if err := root.Mkdir(name, 0o700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.WriteFile(filepath.Join(path, "target"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := root.Symlink("target", name); err != nil {
					t.Fatal(err)
				}
			default:
				data := []byte{}
				mode := os.FileMode(0o600)
				if scenario == "nonempty" {
					data = []byte("untrusted")
				}
				if scenario == "public" {
					mode = 0o644
				}
				if err := os.WriteFile(filepath.Join(path, name), data, mode); err != nil {
					t.Fatal(err)
				}
				if err := root.Chmod(name, mode); err != nil {
					t.Fatal(err)
				}
			}
			_, err = pruneCodexProjectionConsumers(t.Context(), root, 1)
			if scenario == "valid stale" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := root.Lstat(name); !os.IsNotExist(err) {
					t.Fatal("stale consumer was not reclaimed")
				}
			} else {
				requireMediaCode(t, err, media.AccessDenied)
				if _, err := root.Lstat(name); err != nil {
					t.Fatal("unsafe marker was silently removed", err)
				}
			}
		})
	}
}

func TestCodexProjectionConsumerPreservesLiveOwners(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Codex ProcessSupervisor is not implemented on Windows")
	}
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	consumer, err := newCodexProjectionConsumer(t.Context(), root, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = consumer.file.Close() }()
	active, err := pruneCodexProjectionConsumers(t.Context(), root, 1)
	if err != nil || active != 1 {
		t.Fatalf("live consumer lost: %d %v", active, err)
	}
	if _, err := root.Lstat(consumer.name); err != nil {
		t.Fatal(err)
	}
	if err := consumer.file.Close(); err != nil {
		t.Fatal(err)
	}
	active, err = pruneCodexProjectionConsumers(t.Context(), root, 1)
	if err != nil || active != 0 {
		t.Fatalf("closed consumer retained: %d %v", active, err)
	}
}

func TestCodexProjectionConsumerDirectoryBudget(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Codex ProcessSupervisor is not implemented on Windows")
	}
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for index := range maxCodexProjectionConsumers + 2 {
		// The reader must refuse an oversized directory before collecting it.
		file, err := root.Create(strings.Repeat("x", index%10+1) + rand.Text())
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	_, err = pruneCodexProjectionConsumers(t.Context(), root, 1)
	requireMediaCode(t, err, media.LimitExceeded)
}
