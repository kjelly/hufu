//go:build aix || android || darwin || dragonfly || freebsd || hurd || illumos || ios || linux || netbsd || openbsd || solaris

package team

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/media"
	"golang.org/x/sys/unix"
)

func TestCodexStateLockNormalizesCreationPermissions(t *testing.T) {
	// Umask is process-global. Isolate it from every other test and fixture.
	if directory := os.Getenv("HUFU_TEST_CODEX_LOCK_DIRECTORY"); directory != "" {
		mask, err := strconv.ParseInt(os.Getenv("HUFU_TEST_CODEX_LOCK_UMASK"), 8, 32)
		if err != nil {
			t.Fatal(err)
		}
		previous := unix.Umask(int(mask))
		defer unix.Umask(previous)
		path := filepath.Join(directory, codexProjectionLockFile)
		// Both creation and reuse must work without replacing the lock inode.
		var original os.FileInfo
		for range 2 {
			release, err := acquireCodexStateFileLock(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(path)
			if err != nil {
				release()
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 || original != nil && !os.SameFile(original, info) {
				release()
				t.Fatalf("lock permissions or inode changed: %v", info.Mode())
			}
			original = info
			root, err := os.OpenRoot(directory)
			if err != nil {
				release()
				t.Fatal(err)
			}
			err = checkCodexProjectionCopies(t.Context(), root, root, nil, media.Limits{MaxBranchBlobs: 1, MaxBranchBytes: 1})
			_ = root.Close()
			release()
			if err != nil {
				t.Fatalf("quota rejected a safely created lock: %v", err)
			}
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mask := range []string{"0077", "0277", "0777"} {
		t.Run(mask, func(t *testing.T) {
			command := exec.CommandContext(t.Context(), executable, "-test.run=^TestCodexStateLockNormalizesCreationPermissions$")
			command.Env = append(os.Environ(), "HUFU_TEST_CODEX_LOCK_DIRECTORY="+t.TempDir(), "HUFU_TEST_CODEX_LOCK_UMASK="+mask)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("umask %s: %v\n%s", mask, err, output)
			}
		})
	}
}

func TestCodexStateLockPreservesExistingFiles(t *testing.T) {
	for _, scenario := range []string{"regular", "readonly", "public", "symlink", "directory"} {
		t.Run(scenario, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "lock")
			target := filepath.Join(directory, "target")
			if err := os.WriteFile(target, []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "symlink":
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			default:
				mode := os.FileMode(0o600)
				if scenario == "readonly" {
					mode = 0o400
				} else if scenario == "public" {
					mode = 0o644
				}
				if err := os.WriteFile(path, []byte("preserve"), mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			release, err := acquireCodexStateFileLock(t.Context(), path)
			if scenario == "regular" {
				if err != nil {
					t.Fatal(err)
				}
				release()
			} else if err == nil {
				release()
				t.Fatal("unsafe existing lock accepted")
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatalf("existing lock was changed: %v", err)
			}
			if data, err := os.ReadFile(target); err != nil || string(data) != "preserve" {
				t.Fatalf("symlink target changed: %q, %v", data, err)
			}
		})
	}
}

func TestCodexStateLockContentionHonorsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	release, err := acquireCodexStateFileLock(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if unlock, err := acquireCodexStateFileLock(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		if unlock != nil {
			unlock()
		}
		t.Fatalf("contended lock did not honor cancellation: %v", err)
	}
	release()
	unlocked, err := acquireCodexStateFileLock(t.Context(), path)
	if err != nil {
		t.Fatalf("cancelled waiter stranded the lock: %v", err)
	}
	unlocked()
}

func TestCodexProjectionLockScopeAcrossIndependentHomes(t *testing.T) {
	for _, shared := range []bool{true, false} {
		name := "separate workspaces"
		if shared {
			name = "shared workspace"
		}
		t.Run(name, func(t *testing.T) {
			// Distinct account homes can own app-server lifetimes concurrently.
			for range 2 {
				release, err := acquireCodexHomeStateLock(t.Context(), t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(release)
			}
			first := filepath.Join(t.TempDir(), codexProjectionLockFile)
			second := first
			if !shared {
				second = filepath.Join(t.TempDir(), codexProjectionLockFile)
			}
			release, err := acquireCodexStateFileLock(t.Context(), first)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			other, err := acquireCodexStateFileLock(ctx, second)
			if shared {
				if other != nil {
					other()
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("shared projection did not wait: %v", err)
				}
				release()
				other, err = acquireCodexStateFileLock(t.Context(), second)
			}
			if err != nil {
				t.Fatal(err)
			}
			other()
		})
	}
}
