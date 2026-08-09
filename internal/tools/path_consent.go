//go:build linux || darwin
// +build linux darwin

package tools

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/x/term"

	"github.com/kjelly/hufu/internal/audit"
	"github.com/kjelly/hufu/internal/utils"
)

type ConsentResult int

const (
	ConsentDenied ConsentResult = iota
	ConsentOnce
	ConsentAlways
)

type AgentInfo struct {
	Name string
	Task string
}

type PathConsent struct {
	mu           sync.Mutex
	remembered   []string
	denied       []string
	currentAgent func() AgentInfo
	persistPath  string
}

func NewPathConsent() *PathConsent {
	return &PathConsent{
		currentAgent: func() AgentInfo { return AgentInfo{} },
	}
}

func (pc *PathConsent) SetAgentInfoSource(fn func() AgentInfo) {
	if fn == nil {
		return
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.currentAgent = fn
}

func NewPathConsentWithAgentInfo(fn func() AgentInfo) *PathConsent {
	if fn == nil {
		return NewPathConsent()
	}
	return &PathConsent{
		currentAgent: fn,
	}
}

// NewTeamPathConsent returns a consent store scoped to one agent team. Its
// "always" decisions are persisted in .hufu-path-consent.yaml beside the
// team definition, so a later hufu invocation reuses them. A plain
// NewPathConsent remains in-memory only, which is useful for callers that do
// not have a team directory.
func NewTeamPathConsent(teamDir string) (*PathConsent, error) {
	policy, err := LoadPathConsentPolicy(teamDir)
	if err != nil {
		return nil, err
	}
	return &PathConsent{
		remembered:   consentPrefixes(policy.Allowed),
		denied:       consentPrefixes(policy.Denied),
		currentAgent: func() AgentInfo { return AgentInfo{} },
		persistPath:  PathConsentPolicyPath(teamDir),
	}, nil
}

// dirOfPath returns the directory portion of a path.
// For non-existent paths, uses filepath.Ext as a heuristic:
// paths with extensions are treated as files, without as directories.
// This means "Makefile" (no extension) is treated as a directory.
func dirOfPath(path string) string {
	if info, err := os.Stat(path); err == nil {
		if info.IsDir() {
			return filepath.Clean(path)
		}
		return filepath.Dir(path)
	}
	if len(path) > 0 && os.IsPathSeparator(path[len(path)-1]) {
		return filepath.Clean(path)
	}
	return filepath.Dir(path)
}

// consentWaitWarnThreshold is how long a consent check may block a tool
// before it is worth a warning: past this the wait starts to look like a
// tool hang in the audit timeline.
const consentWaitWarnThreshold = 5 * time.Second

// defaultConsentPromptTimeout bounds how long one consent prompt may wait
// for an answer before auto-denying. A real run sat two hours on a prompt
// nobody saw, with a second agent queued behind the stdin lock the whole
// time; a bounded deny with an actionable error lets the agent adapt
// instead. Override with HUFU_CONSENT_TIMEOUT (seconds, 0 = wait forever).
const defaultConsentPromptTimeout = 120 * time.Second

func consentPromptTimeout() time.Duration {
	if v := os.Getenv("HUFU_CONSENT_TIMEOUT"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return defaultConsentPromptTimeout
}

// processUnattended is set from the coordinator's unattended flag so consent
// (which has no request context) can honor unattended mode.
var processUnattended atomic.Bool

// SetProcessUnattended marks the whole process as running without a human;
// path consent then fast-denies instead of prompting.
func SetProcessUnattended(v bool) { processUnattended.Store(v) }

func (pc *PathConsent) AskConsent(path, operation string, toolName, toolArgs string) (ConsentResult, string, error) {
	if IsInteractiveAbortRequested() {
		return ConsentDenied, "", nil
	}

	dirToRemember := dirOfPath(path)
	normalized := dirToRemember + string(os.PathSeparator)

	pc.mu.Lock()
	for _, prefix := range pc.remembered {
		if strings.HasPrefix(normalized, prefix) {
			pc.mu.Unlock()
			return ConsentAlways, "", nil
		}
	}
	if pc.isDeniedLocked(path) {
		pc.mu.Unlock()
		return ConsentDenied, "", nil
	}
	pc.mu.Unlock()

	if IsInteractiveAbortRequested() {
		return ConsentDenied, "", nil
	}

	agentName := pc.currentAgent().Name

	// No human, no prompt: deny with an error the model can act on instead
	// of blocking a prompt nobody will answer.
	if processUnattended.Load() || !IsInteractiveEnvironment() {
		audit.LogConsentWaitStart(agentName, toolName, path)
		audit.LogConsentResolved(agentName, toolName, path, "unattended-denied", 0)
		NotifyNeedsHuman(fmt.Sprintf("path consent needed for %s (tool %s), auto-denied: no human available", path, toolName))
		return ConsentDenied, "", fmt.Errorf(
			"no human available to grant path consent (unattended or non-interactive run) — use a path under the allowed paths, or re-run hufu with --allow-path %q", dirToRemember)
	}

	// Everything below can block for minutes: the stdin lock may be held by
	// another prompt, and the prompt itself waits on a human who may not
	// have noticed it (it writes to stderr underneath an active TUI). A real
	// run sat 2h15m here before the prompt timeout existed. Bracket it with
	// audit events so long call→result gaps are attributable, and warn when
	// the wait crosses the threshold.
	waitStart := time.Now()
	audit.LogConsentWaitStart(agentName, toolName, path)
	result, suggestion, err := pc.promptForConsent(path, dirToRemember, normalized, toolName, toolArgs)
	waited := time.Since(waitStart)
	audit.LogConsentResolved(agentName, toolName, path, consentOutcomeString(result, err), waited)
	if waited > consentWaitWarnThreshold {
		log.Printf("[WARN] path consent for tool %q blocked %s waiting for interactive approval (path %s, outcome %s)",
			toolName, waited.Round(time.Second), path, consentOutcomeString(result, err))
	}
	return result, suggestion, err
}

// warnSlowConsent logs when a tool's pre-run consent checks (which may span
// several AskConsent prompts for one command) blocked long enough to look
// like a hang. started is captured just before checkBashPathConsent. Note
// the tool's own timeout parameter does NOT cover this wait — it only times
// the command execution.
func warnSlowConsent(toolName string, started time.Time) {
	if waited := time.Since(started); waited > consentWaitWarnThreshold {
		log.Printf("[WARN] %s: path-consent checks blocked %s before the command ran", toolName, waited.Round(time.Second))
	}
}

// ErrConsentPromptTimeout marks a consent denial caused by nobody answering
// the prompt within consentPromptTimeout, so callers and audit lines can
// distinguish it from an explicit user denial.
var ErrConsentPromptTimeout = errors.New("path consent prompt timed out")

// consentOutcomeString renders a ConsentResult for audit/log lines.
func consentOutcomeString(result ConsentResult, err error) string {
	if errors.Is(err, ErrConsentPromptTimeout) {
		return "timeout-denied"
	}
	if err != nil {
		return "error"
	}
	switch result {
	case ConsentAlways:
		return "always"
	case ConsentOnce:
		return "once"
	default:
		return "denied"
	}
}

// promptForConsent is the blocking half of AskConsent: it serializes on the
// stdin lock, re-checks remembered/denied prefixes, and prompts the user.
func (pc *PathConsent) promptForConsent(path, dirToRemember, normalized, toolName, toolArgs string) (ConsentResult, string, error) {
	// Acquire stdin lock before any output so that we never write to the
	// terminal while it is in raw mode (e.g. when the TUI is active).
	// NotifyAskUserStart releases the altscreen / restores cooked mode in TUI
	// mode; it is a no-op in non-TUI mode.
	StdinMu.Lock()
	defer StdinMu.Unlock()

	SetAskUserActive(true)
	defer SetAskUserActive(false)

	NotifyAskUserStart()

	// Re-check: another goroutine may have updated remembered/denied while
	// we were waiting for StdinMu.
	pc.mu.Lock()
	for _, prefix := range pc.remembered {
		if strings.HasPrefix(normalized, prefix) {
			pc.mu.Unlock()
			return ConsentAlways, "", nil
		}
	}
	if pc.isDeniedLocked(path) {
		pc.mu.Unlock()
		return ConsentDenied, "", nil
	}
	pc.mu.Unlock()

	// A line typed after an earlier prompt timed out would otherwise be
	// delivered as the answer to this one.
	drainStaleConsentInput()

	fmt.Fprintf(os.Stderr, "\n%s\n", boldFmt("─── Path Consent ───"))

	agentInfo := pc.currentAgent()
	width := promptTerminalWidth(100)
	if agentInfo.Name != "" {
		if agentInfo.Task != "" {
			for _, line := range formatConsentPreviewLines("Agent: "+agentInfo.Name+" — ", agentInfo.Task, width) {
				fmt.Fprintln(os.Stderr, line)
			}
		} else {
			fmt.Fprintf(os.Stderr, "Agent: %s\n", agentInfo.Name)
		}
	}

	if toolName != "" {
		if toolArgs != "" {
			for _, line := range formatConsentPreviewLines("Tool:  "+toolName+" → ", toolArgs, width) {
				fmt.Fprintln(os.Stderr, line)
			}
		} else {
			fmt.Fprintf(os.Stderr, "Tool:  %s\n", toolName)
		}
	}

	fmt.Fprintf(os.Stderr, "\n⚠ Path is outside allowed paths.\n")
	fmt.Fprintf(os.Stderr, "  %s Allow once\n", cyanFmt("[y]"))
	fmt.Fprintf(os.Stderr, "  %s Always allow %s\n", cyanFmt("[a]"), dirToRemember+string(os.PathSeparator))
	fmt.Fprintf(os.Stderr, "  Type a path to suggest a replacement path to the agent\n")
	fmt.Fprintf(os.Stderr, "  %s Always deny %s\n", cyanFmt("[d]"), dirToRemember+string(os.PathSeparator))
	fmt.Fprintf(os.Stderr, "  %s Deny (default)\n", cyanFmt("[N]"))
	fmt.Fprintf(os.Stderr, "  You can type an absolute or relative path to suggest a replacement path.\n")
	fmt.Fprintf(os.Stderr, "%s ", boldFmt("Your choice:"))

	input, answered := consentReadLineWithTimeout(consentPromptTimeout())
	if !answered {
		fmt.Fprintf(os.Stderr, "\n(no answer within %s — denying)\n", consentPromptTimeout())
		return ConsentDenied, "", fmt.Errorf(
			"%w after %s (nobody answered) — use a path under the allowed paths, or re-run hufu with --allow-path %q",
			ErrConsentPromptTimeout, consentPromptTimeout(), dirToRemember)
	}

	selection := parseConsentInput(input, dirToRemember)
	switch selection.kind {
	case ConsentAlways:
		pc.mu.Lock()
		pc.remembered = append(pc.remembered, selection.rememberPrefix)
		err := pc.persistLocked()
		pc.mu.Unlock()
		if err != nil {
			return ConsentDenied, "", err
		}
		return ConsentAlways, "", nil
	case ConsentDenied:
		if selection.deniedPrefix != "" {
			pc.mu.Lock()
			pc.denied = append(pc.denied, selection.deniedPrefix)
			err := pc.persistLocked()
			pc.mu.Unlock()
			if err != nil {
				return ConsentDenied, "", err
			}
		}
		return ConsentDenied, selection.suggestedPath, nil
	case ConsentOnce:
		return ConsentOnce, "", nil
	default:
		return ConsentDenied, "", nil
	}
}

type consentSelection struct {
	kind           ConsentResult
	rememberPrefix string
	deniedPrefix   string
	suggestedPath  string
}

func parseConsentInput(input, defaultDir string) consentSelection {
	trimmed := strings.TrimSpace(input)
	switch strings.ToLower(trimmed) {
	case "a":
		return consentSelection{kind: ConsentAlways, rememberPrefix: defaultDir + string(os.PathSeparator)}
	case "d":
		return consentSelection{kind: ConsentDenied, deniedPrefix: defaultDir + string(os.PathSeparator)}
	case "y":
		return consentSelection{kind: ConsentOnce}
	}

	if suggestion, ok := normalizeConsentSuggestedPath(trimmed); ok {
		return consentSelection{kind: ConsentDenied, suggestedPath: suggestion}
	}

	return consentSelection{kind: ConsentDenied}
}

func normalizeConsentSuggestedPath(path string) (string, bool) {
	if path == "" {
		return "", false
	}

	expanded := os.ExpandEnv(path)
	if strings.HasPrefix(expanded, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			expanded = filepath.Join(home, expanded[1:])
		}
	}

	if !filepath.IsAbs(expanded) {
		abs, err := filepath.Abs(expanded)
		if err == nil {
			expanded = abs
		}
	}

	expanded = filepath.Clean(expanded)
	if expanded == "." || expanded == string(filepath.Separator) {
		return "", false
	}
	if !filepath.IsAbs(expanded) {
		return "", false
	}
	return expanded, true
}

func (pc *PathConsent) isDeniedLocked(path string) bool {
	normalized := path + string(os.PathSeparator)
	for _, prefix := range pc.denied {
		if strings.HasPrefix(normalized, prefix) {
			return true
		}
	}
	return false
}

func (pc *PathConsent) IsRemembered(path string) bool {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	normalized := path + string(os.PathSeparator)
	for _, prefix := range pc.remembered {
		if strings.HasPrefix(normalized, prefix) {
			return true
		}
	}
	return false
}

func (pc *PathConsent) IsDenied(path string) bool {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return pc.isDeniedLocked(path)
}

func readConsentLine(reader *bufio.Reader) string {
	line, err := reader.ReadString('\n')
	if err != nil {
		return ""
	}
	return strings.TrimRight(line, "\r\n")
}

// consentStdin is the shared line source for consent prompts. A prompt that
// times out leaves its stdin read goroutine alive; keeping one channel and a
// pending flag means the next prompt reuses that read instead of stacking a
// second reader that would race it for lines. Prompts are serialized on
// StdinMu, so this state is only touched by one prompt at a time.
var consentStdin struct {
	mu      sync.Mutex
	ch      chan string
	pending bool
	// readLine is swappable for tests.
	readLine func() string
}

// drainStaleConsentInput discards a line left over from a timed-out prompt
// (typed after the deny already happened). Call before printing a new prompt.
func drainStaleConsentInput() {
	consentStdin.mu.Lock()
	ch := consentStdin.ch
	consentStdin.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case <-ch:
	default:
	}
}

// consentReadLineWithTimeout reads one line from stdin, giving up after
// timeout (0 = wait forever). The second return is false on timeout.
func consentReadLineWithTimeout(timeout time.Duration) (string, bool) {
	consentStdin.mu.Lock()
	if consentStdin.ch == nil {
		consentStdin.ch = make(chan string, 1)
	}
	if consentStdin.readLine == nil {
		consentStdin.readLine = func() string { return readConsentLine(bufio.NewReader(os.Stdin)) }
	}
	if !consentStdin.pending {
		consentStdin.pending = true
		read := consentStdin.readLine
		ch := consentStdin.ch
		go func() {
			line := read()
			ch <- line
			consentStdin.mu.Lock()
			consentStdin.pending = false
			consentStdin.mu.Unlock()
		}()
	}
	ch := consentStdin.ch
	consentStdin.mu.Unlock()

	if timeout <= 0 {
		return <-ch, true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case line := <-ch:
		return line, true
	case <-timer.C:
		return "", false
	}
}

func promptTerminalWidth(fallback int) int {
	if cols := os.Getenv("COLUMNS"); cols != "" {
		if n, err := strconv.Atoi(cols); err == nil && n > 0 {
			return n
		}
	}
	if w, _, err := term.GetSize(os.Stderr.Fd()); err == nil && w > 0 {
		return w
	}
	if w, _, err := term.GetSize(os.Stdout.Fd()); err == nil && w > 0 {
		return w
	}
	return fallback
}

func formatConsentPreviewLines(prefix, text string, width int) []string {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return []string{strings.TrimRight(prefix, " ")}
	}

	if width <= 0 {
		return []string{strings.TrimRight(prefix, " ") + text}
	}
	if width <= len(prefix) {
		return []string{strings.TrimRight(prefix, " ") + text}
	}

	contentWidth := width - len(prefix)
	wrapped := utils.WrapLine(text, contentWidth, 9999)
	if len(wrapped.Lines) == 0 {
		return []string{strings.TrimRight(prefix, " ") + text}
	}

	lines := make([]string, 0, len(wrapped.Lines))
	indent := strings.Repeat(" ", len(prefix))
	for i, line := range wrapped.Lines {
		if i == 0 {
			lines = append(lines, prefix+line)
			continue
		}
		line = strings.TrimPrefix(line, "  - ")
		lines = append(lines, indent+line)
	}
	return lines
}
