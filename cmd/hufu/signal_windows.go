//go:build windows

package main

func setupPromptSignals(injector *promptInjector) func() {
	return func() {}
}
