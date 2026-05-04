//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

func setupPromptSignals(injector *promptInjector) func() {
	sigTstp := make(chan os.Signal, 1)
	signal.Notify(sigTstp, syscall.SIGTSTP)
	go func() {
		for range sigTstp {
			injector.promptAndEnqueue()
		}
	}()

	sigUsr1 := make(chan os.Signal, 1)
	signal.Notify(sigUsr1, syscall.SIGUSR1)
	go func() {
		for range sigUsr1 {
			injector.promptAndEnqueue()
		}
	}()

	return func() {
		signal.Stop(sigTstp)
		close(sigTstp)
		signal.Stop(sigUsr1)
		close(sigUsr1)
	}
}
