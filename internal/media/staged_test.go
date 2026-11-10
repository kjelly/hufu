package media

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/semaphore"
)

func TestStageImageRetainsOriginalBytesAndReturnsQuota(t *testing.T) {
	for _, change := range []string{"replace source", "remove source"} {
		t.Run(change, func(t *testing.T) {
			original := testPNG(t)
			path := filepath.Join(t.TempDir(), "input.png")
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			size := int64(len(original))
			pool := semaphore.NewWeighted(2 * size)
			limits := Limits{MaxBytesPerImage: size}
			first, err := stageImage(t.Context(), path, limits, pool)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(first.Close)
			second, err := stageImage(t.Context(), path, limits, pool)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(second.Close)
			if extra, err := stageImage(t.Context(), path, limits, pool); err == nil {
				extra.Close()
				t.Fatal("staging exceeded its process budget")
			} else {
				requireCode(t, err, LimitExceeded)
			}
			if change == "replace source" {
				err = os.WriteFile(path, []byte("different content"), 0o600)
			} else {
				err = os.Remove(path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := first.WithBytes(t.Context(), func(data []byte, ref Ref) error {
				if !bytes.Equal(data, original) || ref != first.ref || ref.ByteSize != size {
					t.Fatal("staged input re-read its source or changed validated content")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			owned := first.data
			first.Close()
			first.Close()
			if !bytes.Equal(owned, make([]byte, len(owned))) {
				t.Fatal("released staging retained raw image bytes")
			}
			requireCode(t, first.WithBytes(t.Context(), func([]byte, Ref) error { return nil }), InputMissing)
			second.Close()
			if !pool.TryAcquire(2 * size) {
				t.Fatal("released staging retained quota")
			}
			pool.Release(2 * size)
		})
	}
}

func TestStageImageFailuresReturnQuota(t *testing.T) {
	for _, scenario := range []string{"cancelled", "missing", "invalid format", "invalid limits"} {
		t.Run(scenario, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input.png")
			if err := os.WriteFile(path, []byte("invalid"), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			const budget = 1 << 20
			pool := semaphore.NewWeighted(budget)
			limits := Limits{MaxBytesPerImage: budget}
			want := FormatUnsupported
			switch scenario {
			case "cancelled":
				cancel()
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				want = InputMissing
			case "invalid limits":
				limits.MaxBytesPerImage = MaxBytesPerImage + 1
				want = LimitExceeded
			}
			staged, err := stageImage(ctx, path, limits, pool)
			if staged != nil {
				staged.Close()
				t.Fatal("invalid input became pending image data")
			}
			if scenario == "cancelled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("staging ignored cancellation", err)
				}
			} else {
				requireCode(t, err, want)
			}
			if !pool.TryAcquire(budget) {
				t.Fatal("failed staging leaked quota")
			}
			pool.Release(budget)
		})
	}
}

func TestStagedImageCloseDrainsActiveBorrowers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	original := testPNG(t)
	path := filepath.Join(t.TempDir(), "input.png")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	pool := semaphore.NewWeighted(int64(len(original)))
	staged, err := stageImage(ctx, path, Limits{MaxBytesPerImage: int64(len(original))}, pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(staged.Close)
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	borrowed := make(chan error, 1)
	go func() {
		borrowed <- staged.WithBytes(ctx, func(data []byte, _ Ref) error {
			close(entered)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
			}
			if !bytes.Equal(data, original) {
				return errors.New("staging cleared bytes while an ingress borrower was active")
			}
			return nil
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("borrower did not start")
	}
	closed := make(chan struct{})
	go func() { staged.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("staging closed before its borrower exited")
	default:
	}
	unblock()
	select {
	case err := <-borrowed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("borrower did not drain")
	}
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("staging close did not drain")
	}
}
