//go:build !linux

package golangruntime

import "errors"

func limitNormalizerMemory() error {
	return errors.New("result normalization requires a hard child memory limit")
}
