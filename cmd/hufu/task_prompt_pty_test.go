//go:build linux

package main

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/readline"
)

func TestTaskPromptPTYHelper(t *testing.T) {
	if !slices.Contains(os.Args, "task-prompt-fixture") {
		return
	}
	var task string
	var err error
	if slices.Contains(os.Args, "fallback") {
		task, err = askUserForPromptFallback()
	} else {
		reader, createErr := readline.NewPromptReader("")
		if createErr != nil {
			t.Fatal(createErr)
		}
		defer reader.Close()
		if slices.Contains(os.Args, "initial") {
			task, err = askUserForPrompt(reader)
		} else {
			task, err = readChatLine(reader)
		}
	}
	if err != nil {
		fmt.Printf("DRAFT_CANCELLED:%q\n", task)
		return
	}
	fmt.Printf("TASK_RECEIVED:%q\n", task)
}

func TestTaskPromptPTYMultiline(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"initial", "chat", "fallback"} {
		for _, scenario := range []string{"block", "continuation", "cancel"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				process := startPTY(t, binary, []string{"-test.run=^TestTaskPromptPTYHelper$", "--", "task-prompt-fixture", mode}, 24, 100)
				prompt := "> "
				if mode == "chat" {
					prompt = "hufu> "
				}
				waitPrompt := func(label string, offset int) {
					t.Helper()
					process.waitForPTYOutput(t, "\x1b[6n", offset, true, 5*time.Second)
					process.writeInput(t, "\x1b[1;1R")
					process.waitForOutputAfter(t, label, offset, 5*time.Second)
				}
				waitPrompt(prompt, 0)
				input := []string{"/multiline", "first", "", "  第二行", "/end"}
				want := "TASK_RECEIVED:\"first\\n\\n  第二行\""
				if scenario == "continuation" {
					input = []string{"first\\", "  第二行"}
					want = "TASK_RECEIVED:\"first\\n  第二行\""
				}
				if scenario == "cancel" {
					input = []string{"/multiline", "partial\x03"}
					want = "DRAFT_CANCELLED:\"\""
				}
				for i, line := range input {
					offset := len(process.output.String())
					if strings.HasSuffix(line, "\x03") {
						process.writeInput(t, line)
					} else {
						process.writeInput(t, line+"\r")
					}
					if i < len(input)-1 {
						waitPrompt("...", offset)
					}
				}
				process.waitForOutput(t, want, 5*time.Second)
				if code := process.waitExit(t, 5*time.Second); code != 0 {
					t.Fatalf("input process exited with %d: %.8000s", code, process.output.String())
				}
			})
		}
	}
}
