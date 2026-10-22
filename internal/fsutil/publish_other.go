//go:build !linux && !darwin && !windows

package fsutil

import "errors"

func publishDirectoryNoReplace(_, _ string) error {
	return errors.New("atomic no-replace directory publication is unsupported on this platform")
}
