//go:build !linux

package processutil

func lockProcessParentThread() func() { return func() {} }
