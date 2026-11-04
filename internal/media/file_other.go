//go:build !linux && !darwin

package media

import "os"

// Refuse file ingress until an equally race-safe platform adapter is available.
// Generated in-memory images continue to use the shared validator.
func openRegularFile(_ string) (*os.File, error) {
	return nil, Reject(Unsupported, "secure image file ingress is unavailable on this platform")
}
