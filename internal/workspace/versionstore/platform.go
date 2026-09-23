package versionstore

import "runtime"

// goos is a variable so tests can exercise the non-linux gate.
var goos = runtime.GOOS

// PlatformSupported reports whether workspace versioning may be enabled on
// this platform. v1 only enables it on linux; other platforms compile but
// must treat every mode as off.
func PlatformSupported() bool { return goos == "linux" }
