package team

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/media"
)

func TestCodexImageCopyBudgetIncludesRetainedDuplicates(t *testing.T) {
	for _, scenario := range []string{"fits", "blob limit", "byte limit", "current retained", "writable stale"} {
		t.Run(scenario, func(t *testing.T) {
			c, ctx, request, hash := codexImageProjectionFixture(t)
			basePath := filepath.Join(c.artifactStoreRootPath(), logsDir, "media-projections")
			currentPath := codexImageProjectionPath(c.artifactStoreRootPath(), hash)
			if err := os.MkdirAll(currentPath, 0o700); err != nil {
				t.Fatal(err)
			}
			staleHash, err := media.Digest("other task binding")
			if err != nil {
				t.Fatal(err)
			}
			stalePath := filepath.Join(basePath, "input-"+strings.TrimPrefix(staleHash, "sha256:"))
			if scenario == "current retained" {
				stalePath = currentPath
			}
			if err := os.MkdirAll(stalePath, 0o700); err != nil {
				t.Fatal(err)
			}
			ref := request.Task.MediaBinding.Refs[0]
			mode := os.FileMode(0o400)
			if scenario == "writable stale" {
				mode = 0o600
			}
			if err := os.WriteFile(filepath.Join(stalePath, codexImageFilename(ref)), mediaStorePNG(t), mode); err != nil {
				t.Fatal(err)
			}
			base, err := os.OpenRoot(basePath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = base.Close() }()
			current, err := os.OpenRoot(currentPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = current.Close() }()
			limits := media.Limits{MaxBranchBlobs: 2, MaxBranchBytes: 2 * ref.ByteSize}
			if scenario == "blob limit" || scenario == "current retained" {
				limits.MaxBranchBlobs = 1
			}
			if scenario == "byte limit" || scenario == "current retained" {
				limits.MaxBranchBytes = ref.ByteSize
			}
			err = checkCodexProjectionCopies(ctx, base, current, request.Task.MediaBinding.Refs, limits)
			if scenario == "fits" || scenario == "current retained" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			rejected, ok := errors.AsType[*media.Error](err)
			if !ok || rejected.Code != media.LimitExceeded && rejected.Code != media.AccessDenied {
				t.Fatalf("unsafe retained copy budget accepted: %v", err)
			}
		})
	}
}
