//go:build !linux && !darwin && !windows

package workspace

func defaultStateRootPlatform(environment stateRootEnvironment) (string, error) {
	return stateRootForOS("other", environment)
}
