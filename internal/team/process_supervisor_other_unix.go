//go:build !linux && !windows

package team

// Other Unix platforms rely on the group-existence probe performed by the
// caller and wait for the OS to reap all members.
func unixProcessGroupHasLiveMember(int) (bool, error) {
	return true, nil
}
