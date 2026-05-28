package notify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const defaultCommandTimeout = 10 * time.Second

var defaultEvents = []string{"done", "error", "wrap_up"}

type NotifyConfig struct {
	OSC     bool     `yaml:"osc" json:"osc"`
	Command string   `yaml:"command" json:"command"`
	Events  []string `yaml:"events" json:"events"`
}

func DefaultConfig() NotifyConfig {
	return NotifyConfig{
		OSC:    true,
		Events: append([]string{}, defaultEvents...),
	}
}

func (c NotifyConfig) Enabled() bool {
	if c.OSC {
		return true
	}
	if c.Command != "" {
		return true
	}
	return false
}

func (c NotifyConfig) ShouldNotify(eventType string) bool {
	for _, e := range c.Events {
		if e == eventType {
			return true
		}
	}
	return false
}

type Notifier struct {
	cfg    NotifyConfig
	writer io.Writer
	mu     sync.Mutex
}

func NewNotifier(cfg NotifyConfig, w io.Writer) *Notifier {
	if len(cfg.Events) == 0 {
		cfg.Events = make([]string, len(defaultEvents))
		copy(cfg.Events, defaultEvents)
	}
	return &Notifier{
		cfg:    cfg,
		writer: w,
	}
}

func (n *Notifier) Notify(eventType, agent, message, output string) {
	if !n.cfg.Enabled() {
		return
	}
	if !n.cfg.ShouldNotify(eventType) {
		return
	}

	title, msg := formatEvent(eventType, agent, message, output)

	if n.cfg.OSC {
		n.sendOSC(title, msg)
	}
	if n.cfg.Command != "" {
		n.runCommand(title, msg)
	}
}

func formatEvent(eventType, agent, message, output string) (title, msg string) {
	if agent == "" {
		agent = "coordinator"
	}
	msg = strings.TrimSpace(message)
	out := strings.TrimSpace(output)
	if out != "" && len(out) > 200 {
		out = out[:197] + "..."
	}

	switch eventType {
	case "done":
		title = "hufu - done"
		if out != "" {
			msg = fmt.Sprintf("%s finished: %s", agent, out)
		} else {
			msg = fmt.Sprintf("%s done", agent)
		}
	case "error":
		title = "hufu - error"
		msg = fmt.Sprintf("%s: %s", agent, msg)
	case "wrap_up":
		title = "hufu"
		msg = "Wrap up requested — no new tasks"
	case "start":
		title = "hufu - start"
		msg = fmt.Sprintf("%s starting: %s", agent, msg)
	default:
		title = "hufu"
		if msg != "" {
			msg = fmt.Sprintf("%s: %s", eventType, msg)
		} else {
			msg = eventType
		}
	}
	return title, msg
}

func (n *Notifier) sendOSC(title, message string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	_, _ = n.writer.Write([]byte{0x07})
	_, _ = fmt.Fprintf(n.writer, "\x1b]777;notify;%s;%s\x07", escapeOSC(title), escapeOSC(message))
	_, _ = fmt.Fprintf(n.writer, "\x1b]9;%s\x07", escapeOSC(message))
}

func escapeOSC(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, ";", "\\;")
	s = strings.ReplaceAll(s, "\x07", "\\a")
	return s
}

func (n *Notifier) runCommand(title, message string) {
	cmdStr := n.cfg.Command
	cmdStr = strings.ReplaceAll(cmdStr, "{title}", title)
	cmdStr = strings.ReplaceAll(cmdStr, "{message}", message)

	ctx, cancel := context.WithTimeout(context.Background(), defaultCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		log.Printf("notify: command failed: %v: %s", err, stderr.String())
	}
}
