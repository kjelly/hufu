//go:build windows

package workspace

func defaultStateRootPlatform(environment stateRootEnvironment) (string, error) {
	return stateRootForOS("windows", environment)
}
