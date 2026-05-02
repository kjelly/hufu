package tools

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
)

type ConsentResult int

const (
	ConsentDenied ConsentResult = iota
	ConsentOnce
	ConsentAlways
)

type PathConsent struct {
	mu         sync.Mutex
	remembered []string
}

func NewPathConsent() *PathConsent {
	return &PathConsent{}
}

func (pc *PathConsent) AskConsent(path, operation string) (ConsentResult, error) {
	pc.mu.Lock()
	normalized := path + string(os.PathSeparator)
	for _, prefix := range pc.remembered {
		if strings.HasPrefix(normalized, prefix) {
			pc.mu.Unlock()
			return ConsentAlways, nil
		}
	}
	pc.mu.Unlock()

	fmt.Fprintf(os.Stderr, "\n%s\n", boldFmt("─── Path Consent ───"))
	fmt.Fprintf(os.Stderr, "⚠ Agent wants to %s %s (outside allowed paths).\n", operation, path)
	fmt.Fprintf(os.Stderr, "  %s Allow once\n", cyanFmt("[y]"))
	fmt.Fprintf(os.Stderr, "  %s Always allow this path prefix\n", cyanFmt("[a]"))
	fmt.Fprintf(os.Stderr, "  %s Deny (default)\n", cyanFmt("[N]"))
	fmt.Fprintf(os.Stderr, "%s ", boldFmt("Your choice:"))

	StdinMu.Lock()
	defer StdinMu.Unlock()

	reader := bufio.NewReader(os.Stdin)
	input := readConsentLine(reader)

	switch strings.ToLower(strings.TrimSpace(input)) {
	case "a":
		pc.mu.Lock()
		pc.remembered = append(pc.remembered, path+string(os.PathSeparator))
		pc.mu.Unlock()
		return ConsentAlways, nil
	case "y":
		return ConsentOnce, nil
	default:
		return ConsentDenied, nil
	}
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

func readConsentLine(reader *bufio.Reader) string {
	line, err := reader.ReadString('\n')
	if err != nil {
		return ""
	}
	return strings.TrimRight(line, "\r\n")
}
