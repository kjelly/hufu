//go:build !linux && !darwin && !windows

package teampkg

import "errors"

func publishDirectoryNoReplace(_, _ string) error {
	return errors.New("atomic no-replace directory publication is unsupported on this platform")
}
