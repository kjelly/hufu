package main

import "strings"

const taskPromptHint = "Enter sends; end a line with \\ to continue, or type /multiline and finish with /end."

// readTaskPrompt collects a task without changing single-line readers used by
// confirmations, template variables and team selection. An interrupted draft
// is discarded so callers cannot accidentally execute a partial task.
func readTaskPrompt(readLine func(string) (string, error), prompt string) (string, error) {
	line, err := readLine(prompt)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(line) == "/multiline" {
		var lines []string
		for {
			line, err = readLine("... ")
			if err != nil {
				return "", err
			}
			if line == "/end" {
				return strings.Join(lines, "\n"), nil
			}
			// Escape the terminator when it is part of the task itself.
			if line == "\\/end" {
				line = "/end"
			}
			lines = append(lines, line)
		}
	}
	var task strings.Builder
	for {
		// An odd trailing backslash continues; doubled backslashes are literal.
		slashes := len(line) - len(strings.TrimRight(line, "\\"))
		if slashes%2 == 0 {
			task.WriteString(line)
			return task.String(), nil
		}
		task.WriteString(strings.TrimSuffix(line, "\\"))
		task.WriteByte('\n')
		line, err = readLine("... ")
		if err != nil {
			return "", err
		}
	}
}
