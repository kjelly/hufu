package main

import (
	"sort"
	"strings"

	"github.com/anomalyco/hufu/internal/readline"
)

// chatCompleter provides tab completion for the chat REPL. It completes
// REPL commands (starting with /), team/agent names (starting with @),
// and falls back to no completion for free text.
type chatCompleter struct {
	teamName string
	registry *teamRegistryLike
}

type teamRegistryLike struct {
	teams  []string
	agents []string
}

// newChatCompleter builds a completer for the given team context. Pass
// nil for registry when team discovery is not available (e.g. before the
// REPL loop has loaded a team).
func newChatCompleter(teamName string, teamList []string, agentList []string) readline.AutoCompleter {
	return &chatCompleter{
		teamName: teamName,
		registry: &teamRegistryLike{teams: teamList, agents: agentList},
	}
}

func (c *chatCompleter) Do(line []rune, pos int) (newLine [][]rune, length int) {
	// Find the partial word being completed (the token at cursor).
	start := pos
	for start > 0 && !isWordBoundary(line[start-1]) {
		start--
	}
	prefix := string(line[start:pos])

	switch {
	case strings.HasPrefix(prefix, "/"):
		return completeCommand(prefix)
	case strings.HasPrefix(prefix, "@"):
		return completeAtName(prefix, c)
	}
	return nil, 0
}

func isWordBoundary(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == ','
}

func completeCommand(prefix string) (newLine [][]rune, length int) {
	cmds := []string{"/exit", "/help", "/quit", "/reset"}
	return filterCompletions(cmds, prefix, len(prefix))
}

func completeAtName(prefix string, c *chatCompleter) (newLine [][]rune, length int) {
	if c.registry == nil {
		return nil, 0
	}
	all := make([]string, 0, len(c.registry.teams)+len(c.registry.agents))
	for _, t := range c.registry.teams {
		all = append(all, "@"+t)
	}
	for _, a := range c.registry.agents {
		all = append(all, "@"+a)
	}
	return filterCompletions(all, prefix, len(prefix))
}

func filterCompletions(candidates []string, prefix string, prefixLen int) (newLine [][]rune, length int) {
	var matches []string
	for _, c := range candidates {
		if strings.HasPrefix(c, prefix) {
			matches = append(matches, c)
		}
	}
	if len(matches) == 0 {
		return nil, 0
	}
	sort.Strings(matches)
	for _, m := range matches {
		newLine = append(newLine, []rune(m[prefixLen:]))
	}
	return newLine, prefixLen
}
