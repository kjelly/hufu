//go:build linux || darwin

package tools

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"
)

func TestAskUserInputProcess(t *testing.T) {
	if mode := os.Getenv("HUFU_TEST_ASK_INPUT"); mode != "" {
		testAskUserInputChild(t, mode)
		return
	}
	for _, mode := range []string{"cancel", "select_cancel", "blank", "invalid", "custom_blank", "login_blank", "login_cancel"} {
		t.Run(mode, func(t *testing.T) {
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			controlReader, controlWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = controlReader.Close(); _ = controlWriter.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary, "-test.run=^TestAskUserInputProcess$")
			dir := t.TempDir()
			command.Dir = dir
			command.Env = append(os.Environ(), "HUFU_TEST_ASK_INPUT="+mode, "HOME="+dir, "XDG_CONFIG_HOME="+dir)
			command.ExtraFiles = []*os.File{controlReader}
			master, err := pty.StartWithSize(command, &pty.Winsize{Rows: 24, Cols: 80})
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer func() { cancel(); _ = command.Wait() }()
			pending := ""
			waitFor := func(marker string) {
				t.Helper()
				for !strings.Contains(pending, marker) {
					var data [1024]byte
					n, err := master.Read(data[:])
					pending += string(data[:n])
					// A PTY has no terminal emulator. Answer readline's cursor
					// position query before waiting for the rendered prompt.
					for strings.Contains(pending, "\x1b[6n") {
						pending = strings.Replace(pending, "\x1b[6n", "", 1)
						if _, err := master.Write([]byte("\x1b[1;1R")); err != nil {
							t.Fatal(err)
						}
					}
					if len(pending) > 32768 {
						t.Fatalf("unexpectedly large prompt transcript; first: %s\nlast: %s", pending[:4096], pending[len(pending)-4096:])
					}
					if err != nil {
						t.Fatalf("waiting for %s: %v; transcript: %s", marker, err, pending)
					}
				}
				_, pending, _ = strings.Cut(pending, marker)
			}
			waitFor("INPUT_BOOT")
			before, err := term.GetState(master.Fd())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := controlWriter.Write([]byte{'s'}); err != nil {
				t.Fatal(err)
			}
			if mode == "custom_blank" {
				waitFor("Your choice(s) or free text")
				if _, err := master.Write([]byte("0\r")); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "select_cancel" {
				waitFor("Select Option")
			} else {
				waitFor("Your answer")
			}
			answeredAt := time.Now()
			if err := command.Process.Signal(syscall.SIGWINCH); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "cancel", "select_cancel", "login_cancel":
				_, err = controlWriter.Write([]byte{'c'})
			case "blank", "custom_blank", "login_blank":
				_, err = master.Write([]byte{'\r'})
			default:
				_, err = master.Write([]byte("wrong-code\r"))
			}
			if err != nil {
				t.Fatal(err)
			}
			waitFor("INPUT_FIRST_DONE")
			if time.Since(answeredAt) > 2*time.Second {
				t.Fatal("input did not release promptly")
			}
			after, err := term.GetState(master.Fd())
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("terminal not restored: before=%v after=%v err=%v", before, after, err)
			}
			if _, err := controlWriter.Write([]byte{'s'}); err != nil {
				t.Fatal(err)
			}
			waitFor("Your answer")
			if _, err := master.Write([]byte("retained\r")); err != nil {
				t.Fatal(err)
			}
			waitFor("INPUT_SECOND_DONE")
			if err := command.Wait(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func testAskUserInputChild(t *testing.T, mode string) {
	control := os.NewFile(3, "prompt-control")
	defer control.Close()
	readControl := func() {
		t.Helper()
		var b [1]byte
		if _, err := control.Read(b[:]); err != nil {
			t.Fatal(err)
		}
	}
	fmt.Println("INPUT_BOOT")
	readControl()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if strings.HasPrefix(mode, "login_") {
		ctx = WithHumanVerificationRequest(ctx, HumanVerificationObservation{RequestID: "login-request", Purpose: "authentication", Attempt: 1, Trigger: "automatic"})
	}
	if mode == "cancel" || mode == "select_cancel" || mode == "login_cancel" {
		done := make(chan struct{})
		go func() { defer close(done); var b [1]byte; _, _ = control.Read(b[:]); cancel() }()
		input := `{"question":"First prompt","type":"free_text"}`
		if mode == "select_cancel" {
			input = `{"question":"First prompt","type":"single_choice","options":[{"label":"No","value":"no"}]}`
		}
		var err error
		if mode == "login_cancel" {
			err = WaitForHumanVerification(ctx, "Choose whether to log in")
		} else {
			_, err = executeAskUser(ctx, fantasy.ToolCall{Input: input})
		}
		<-done
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	} else if mode == "custom_blank" {
		response, err := executeAskUser(ctx, fantasy.ToolCall{Input: `{"question":"First prompt","type":"mixed","options":[{"label":"No","value":"no"}]}`})
		var answer AskUserResponse
		if err != nil || response.IsError || json.Unmarshal([]byte(response.Content), &answer) != nil || !answer.Declined || answer.Cancelled {
			t.Fatalf("empty custom input was not a negative answer: %+v err=%v", response, err)
		}
	} else if err := WaitForHumanVerification(ctx, "Verify this public source"); !errors.Is(err, ErrHumanVerificationNotConfirmed) {
		t.Fatalf("invalid input was not a negative answer: %v", err)
	}
	if _, err := os.Stdin.Stat(); err != nil {
		t.Fatalf("prompt closed shared stdin: %v", err)
	}
	fmt.Println("INPUT_FIRST_DONE")
	readControl()
	response, err := executeAskUser(t.Context(), fantasy.ToolCall{Input: `{"question":"Second prompt","type":"free_text"}`})
	var answer AskUserResponse
	if err != nil || response.IsError || json.Unmarshal([]byte(response.Content), &answer) != nil || answer.Free != "retained" || answer.Declined {
		t.Fatalf("subsequent prompt failed: %+v err=%v", response, err)
	}
	fmt.Println("INPUT_SECOND_DONE")
}

func TestStdinMutexQueuedPromptCanCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu PromptMutex
		mu.Lock()
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- mu.LockContext(ctx) }()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		mu.Unlock()
		if err := mu.LockContext(t.Context()); err != nil {
			t.Fatal(err)
		}
		mu.Unlock()
	})
}
