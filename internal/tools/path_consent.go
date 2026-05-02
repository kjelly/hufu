package tools

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	if filepath.Ext(path) != "" {
		return filepath.Dir(path)
	}
	return filepath.Clean(path)
}

func (pc *PathConsent) AskConsent(path, operation string, toolName, toolArgs string) (ConsentResult, error) {
	dirToRemember := dirOfPath(path)
	normalized := dirToRemember + string(os.PathSeparator)

	pc.mu.Lock()
	for _, prefix := range pc.remembered {
		if strings.HasPrefix(normalized, prefix) {
			pc.mu.Unlock()
			return ConsentAlways, nil
		}
	}
	if pc.isDeniedLocked(path) {
		pc.mu.Unlock()
		return ConsentDenied, nil
	}
	pc.mu.Unlock()

	fmt.Fprintf(os.Stderr, "\n%s\n", boldFmt("─── Path Consent ───"))

	agentInfo := pc.currentAgent()
	if agentInfo.Name != "" {
		if agentInfo.Task != "" {
			taskPreview := agentInfo.Task
			if len(taskPreview) > 80 {
				taskPreview = taskPreview[:77] + "..."
			}
			fmt.Fprintf(os.Stderr, "Agent: %s — %s\n", agentInfo.Name, taskPreview)
		} else {
			fmt.Fprintf(os.Stderr, "Agent: %s\n", agentInfo.Name)
		}
	}

	if toolName != "" {
		if toolArgs != "" {
			argsPreview := toolArgs
			if len(argsPreview) > 120 {
				argsPreview = argsPreview[:117] + "..."
			}
			fmt.Fprintf(os.Stderr, "Tool:  %s → %s\n", toolName, argsPreview)
		} else {
			fmt.Fprintf(os.Stderr, "Tool:  %s\n", toolName)
		}
	}

	fmt.Fprintf(os.Stderr, "\n⚠ Path is outside allowed paths.\n")
	fmt.Fprintf(os.Stderr, "  %s Allow once\n", cyanFmt("[y]"))
	fmt.Fprintf(os.Stderr, "  %s Always allow %s\n", cyanFmt("[a]"), dirToRemember+string(os.PathSeparator))
	fmt.Fprintf(os.Stderr, "  %s Always deny %s\n", cyanFmt("[d]"), dirToRemember+string(os.PathSeparator))
	fmt.Fprintf(os.Stderr, "  %s Deny (default)\n", cyanFmt("[N]"))
	fmt.Fprintf(os.Stderr, "%s ", boldFmt("Your choice:"))

	StdinMu.Lock()
	defer StdinMu.Unlock()

	// Double-check: another goroutine may have updated remembered/denied while
	// we were waiting for StdinMu.
	pc.mu.Lock()
	for _, prefix := range pc.remembered {
		if strings.HasPrefix(normalized, prefix) {
			pc.mu.Unlock()
			return ConsentAlways, nil
		}
	}
	if pc.isDeniedLocked(path) {
		pc.mu.Unlock()
		return ConsentDenied, nil
	}
	pc.mu.Unlock()

	reader := bufio.NewReader(os.Stdin)
	input := readConsentLine(reader)

	switch strings.ToLower(strings.TrimSpace(input)) {
	case "a":
		pc.mu.Lock()
		pc.remembered = append(pc.remembered, dirToRemember+string(os.PathSeparator))
		pc.mu.Unlock()
		return ConsentAlways, nil
	case "d":
		pc.mu.Lock()
		pc.denied = append(pc.denied, dirToRemember+string(os.PathSeparator))
		pc.mu.Unlock()
		return ConsentDenied, nil
	case "y":
		return ConsentOnce, nil
	default:
		return ConsentDenied, nil
	}
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
