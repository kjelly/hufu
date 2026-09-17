//go:build darwin

package workspace

func defaultStateRootPlatform(environment stateRootEnvironment) (string, error) {
	return stateRootForOS("darwin", environment)
}
