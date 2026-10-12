//go:build !linux && !aix && !darwin && !dragonfly && !freebsd && !illumos && !netbsd && !openbsd && !solaris

package golangruntime

import "os/exec"

func configureProcessAttributes(_ *exec.Cmd) {}
