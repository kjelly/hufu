//go:build !windows

package teampkg

import "github.com/kjelly/hufu/internal/fsutil"

func syncDirectory(path string) error { return fsutil.SyncDir(path) }
