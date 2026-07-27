package main

import (
	"bytes"
	"testing"
)

func TestRenderTerminalScreenRendersOnlyChanges(t *testing.T) {
	var output bytes.Buffer
	last := ""
	renderTerminalScreen(&output, "ready", &last)
	renderTerminalScreen(&output, "ready", &last)
	if got, want := output.String(), "\x1b[H\x1b[2Jready"; got != want {
		t.Fatalf("rendered output = %q, want %q", got, want)
	}
}

func TestTerminalCommandIncludesAttach(t *testing.T) {
	command, _, err := terminalCmd.Find([]string{"attach"})
	if err != nil {
		t.Fatal(err)
	}
	if command != terminalAttachCmd {
		t.Fatalf("attach command = %p, want %p", command, terminalAttachCmd)
	}
}
