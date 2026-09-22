//go:build linux

package processutil

import "runtime"

func lockProcessParentThread() func() {
	runtime.LockOSThread()
	return runtime.UnlockOSThread
}
