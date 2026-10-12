//go:build !unix

package team

import "os"

func processWasInterrupted(*os.ProcessState) bool { return false }
