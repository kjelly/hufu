//go:build linux

package workspace

func defaultStateRootPlatform(environment stateRootEnvironment) (string, error) {
	return stateRootForOS("linux", environment)
}
