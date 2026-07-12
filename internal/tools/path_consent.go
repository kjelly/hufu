//go:build linux || darwin
// +build linux darwin

package tools

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/term"

	"github.com/anomalyco/hufu/internal/audit"
	"github.com/anomalyco/hufu/internal/utils"
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

	// Everything below can block for minutes: the stdin lock may be held by
	// another prompt, and the prompt itself waits on a human who may not
	// have noticed it (it writes to stderr underneath an active TUI). A real
	// run once showed a 10-minute sudo "hang" that was most plausibly this
	// wait. Bracket it with audit events so long call→result gaps are
	// attributable, and warn when the wait crosses the threshold.
	agentName := pc.currentAgent().Name
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

// consentOutcomeString renders a ConsentResult for audit/log lines.
func consentOutcomeString(result ConsentResult, err error) string {
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

	reader := bufio.NewReader(os.Stdin)
	input := readConsentLine(reader)

	selection := parseConsentInput(input, dirToRemember)
	switch selection.kind {
	case ConsentAlways:
		pc.mu.Lock()
		pc.remembered = append(pc.remembered, selection.rememberPrefix)
		pc.mu.Unlock()
		return ConsentAlways, "", nil
	case ConsentDenied:
		if selection.deniedPrefix != "" {
			pc.mu.Lock()
			pc.denied = append(pc.denied, selection.deniedPrefix)
			pc.mu.Unlock()
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
