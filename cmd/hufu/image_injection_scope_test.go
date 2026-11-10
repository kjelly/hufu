package main

import (
	"context"
	"testing"
)

func TestPromptInjectionRejectsImageControlsWithoutCallingCoordinator(t *testing.T) {
	previous := opts.isChatTUI
	opts.isChatTUI = false
	t.Cleanup(func() { opts.isChatTUI = previous })
	injector := newPromptInjector(nil)
	injector.ch <- "/image add /private/source.png"
	close(injector.ch)
	result, err := runWithInjection(context.Background(), nil, "previous result", injector)
	if err != nil || result != "previous result" {
		t.Fatal("unsupported image control entered a coordinator turn", err)
	}
}
