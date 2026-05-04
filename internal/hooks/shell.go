package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"time"
)

const defaultShellTimeout = 30 * time.Second

type ShellHook struct {
	Command string
	Timeout time.Duration
}

func NewShellHook(command string) *ShellHook {
	return &ShellHook{
		Command: command,
		Timeout: defaultShellTimeout,
	}
}

func (s *ShellHook) Run(ctx context.Context, payload HookPayload) HookResponse {
	timeout := s.Timeout
	if timeout == 0 {
		timeout = defaultShellTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		log.Printf("hook: failed to marshal payload for %q: %v", s.Command, err)
		return HookResponse{Result: HookContinue}
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", s.Command)
	cmd.Stdin = bytes.NewReader(payloadJSON)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			log.Printf("hook: %q timed out after %v", s.Command, timeout)
		} else {
			log.Printf("hook: %q failed: %v stderr: %s", s.Command, err, stderr.String())
		}
		return HookResponse{Result: HookContinue}
	}

	output := stdout.Bytes()
	if len(output) == 0 {
		return HookResponse{Result: HookContinue}
	}

	var resp HookResponse
	if err := json.Unmarshal(output, &resp); err != nil {
		log.Printf("hook: %q returned invalid JSON: %v output: %s", s.Command, err, string(output))
		return HookResponse{Result: HookContinue}
	}

	return resp
}

func ShellHookFunc(command string) HookFunc {
	sh := NewShellHook(command)
	return func(ctx context.Context, payload HookPayload) HookResponse {
		return sh.Run(ctx, payload)
	}
}

func RegisterShellHooks(registry *HookRegistry, hooks map[string]string) error {
	for hookPoint, command := range hooks {
		if command == "" {
			continue
		}
		if err := validateHookPoint(hookPoint); err != nil {
			return fmt.Errorf("invalid hook point %q: %w", hookPoint, err)
		}
		registry.Register(hookPoint, ShellHookFunc(command))
	}
	return nil
}

var validHookPoints = map[string]bool{
	"before_tool_call": true,
	"after_tool_call":  true,
	"before_llm_step":  true,
	"after_llm_step":   true,
}

func validateHookPoint(hookPoint string) error {
	if validHookPoints[hookPoint] {
		return nil
	}
	valid := make([]string, 0, len(validHookPoints))
	for k := range validHookPoints {
		valid = append(valid, k)
	}
	return fmt.Errorf("unknown hook point; valid: %v", valid)
}
