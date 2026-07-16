package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	ergoreadline "github.com/ergochat/readline"
	"github.com/manifoldco/promptui"
	"gopkg.in/yaml.v3"

	"github.com/anomalyco/hufu/internal/readline"
	"github.com/anomalyco/hufu/internal/team"
	"github.com/anomalyco/hufu/internal/tools"
)

// offerFirstTimeWizard is shown when no teams are discovered in the
// search paths. In an interactive TTY it offers the user three escape
// hatches: try --default, scaffold a team with `hufu init`, or exit so
// they can read the docs. In a non-TTY environment it falls back to a
// plain error message.
func offerFirstTimeWizard(searchPaths []string) error {
	if !tools.IsInteractiveEnvironment() {
		return fmt.Errorf("no agent teams found in search paths: %s\n  Use --default for the built-in team, or scaffold one with `hufu init <team>`", strings.Join(searchPaths, ", "))
	}

	fmt.Fprintf(os.Stderr, "\n%s\n", boldStyle.Render("─── Welcome to hufu ───"))
	fmt.Fprintf(os.Stderr, "No agent teams found in:\n")
	for _, p := range searchPaths {
		fmt.Fprintf(os.Stderr, "  %s %s\n", dimStyle.Render("•"), p)
	}
	fmt.Fprintln(os.Stderr)

	options := []string{
		"Use --default (try hufu with no team files - built-in coordinator + Helper)",
		"Scaffold a team (create a new team interactively)",
		"Cancel (exit and read the docs)",
	}

	prompt := promptui.Select{
		Label: "No agent teams found. Pick an option",
		Items: options,
		Size:  3,
	}

	index, _, err := prompt.Run()
	if err != nil {
		return fmt.Errorf("no team configured; user cancelled first-time wizard")
	}

	switch index {
	case 1:
		fmt.Fprintf(os.Stderr, "%s Run `hufu init <team-name>` to scaffold a new team, then re-run hufu.\n", doneStyle.Render("→"))
		return fmt.Errorf("no team configured; scaffold one with `hufu init <team-name>`")
	case 2:
		fmt.Fprintf(os.Stderr, "%s See README.md Quick Start or run `hufu doctor` to verify your setup.\n", dimStyle.Render("○"))
		return fmt.Errorf("no team configured; user cancelled first-time wizard")
	default: // index == 0
		fmt.Fprintf(os.Stderr, "%s Re-run with --default (and --model <name> if you want a specific LLM):\n", doneStyle.Render("→"))
		fmt.Fprintf(os.Stderr, "    hufu --default --model ollama/qwen3:8b \"your task here\"\n")
		return fmt.Errorf("no team configured; run with --default")
	}
}

func defaultHistoryPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".hufu")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	return filepath.Join(dir, "prompt_history")
}

func askUserForPromptFallback() (string, error) {
	fmt.Fprintf(os.Stderr, "\n%s\n", boldStyle.Render("─── Enter Prompt ───"))
	fmt.Fprintf(os.Stderr, "Describe the task (use @team-name or @agent-name in the prompt):\n")
	prompt := promptui.Prompt{
		Label: "Prompt",
	}
	result, err := prompt.Run()
	if err != nil {
		if err == promptui.ErrInterrupt {
			return "", errInterrupted{}
		}
		return "", err
	}
	promptVal := strings.TrimSpace(result)
	if promptVal == "" {
		fmt.Fprintf(os.Stderr, "%s No prompt provided.\n", errStyle.Render("✗"))
		return "", fmt.Errorf("no prompt provided")
	}
	return promptVal, nil
}

func askUserForTeamFallback(teams []string) string {
	res, _ := askUserForTeamWithPromptUI(teams)
	return res
}

func readStdin() string {
	stat, err := os.Stdin.Stat()
	if err != nil || (stat.Mode()&os.ModeCharDevice) != 0 {
		return ""
	}
	data, _ := io.ReadAll(os.Stdin)
	return strings.TrimSpace(string(data))
}

func askUserForPrompt(pr *readline.PromptReader) (string, error) {
	fmt.Fprintf(os.Stderr, "\n%s\n", boldStyle.Render("─── Enter Prompt ───"))
	fmt.Fprintf(os.Stderr, "Describe the task (use @team-name or @agent-name in the prompt):\n")
	prompt, err := pr.ReadLine(boldStyle.Render("> "))
	if err != nil {
		if err == ergoreadline.ErrInterrupt || err == io.EOF {
			fmt.Fprintf(os.Stderr, "\n")
			return "", errInterrupted{}
		}
		fmt.Fprintf(os.Stderr, "%s Input error: %v\n", errStyle.Render("✗"), err)
		return "", fmt.Errorf("input error: %w", err)
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		fmt.Fprintf(os.Stderr, "%s No prompt provided.\n", errStyle.Render("✗"))
		return "", fmt.Errorf("no prompt provided")
	}
	return prompt, nil
}

func askUserForTeam(teams []string, pr *readline.PromptReader) (string, error) {
	return askUserForTeamWithPromptUI(teams)
}

func askUserForTeamWithPromptUI(teams []string) (string, error) {
	if len(teams) == 0 {
		return "", nil
	}
	sort.Strings(teams)

	stat, err := os.Stdin.Stat()
	isTTY := err == nil && (stat.Mode()&os.ModeCharDevice) != 0
	if opts.unattended || !isTTY {
		return teams[0], nil
	}

	searcher := func(input string, index int) bool {
		team := teams[index]
		name := strings.ReplaceAll(strings.ToLower(team), " ", "")
		input = strings.ReplaceAll(strings.ToLower(input), " ", "")
		return strings.Contains(name, input)
	}

	prompt := promptui.Select{
		Label:    "Select Team",
		Items:    teams,
		Size:     10,
		Searcher: searcher,
	}

	_, result, err := prompt.Run()
	if err != nil {
		if err == promptui.ErrInterrupt {
			return "", errInterrupted{}
		}
		return "", err
	}
	return result, nil
}

func completeAtNames(toComplete string) []string {
	registry := team.NewTeamRegistry(resolveSearchPaths())
	if err := registry.Discover(); err != nil {
		return nil
	}

	var results []string
	prefix := strings.ToLower(toComplete)
	if !strings.HasPrefix(prefix, "@") {
		// Suggest all teams with @ prefix
		for _, name := range registry.ListTeams() {
			results = append(results, "@"+name)
		}
		sort.Strings(results)
		return results
	}

	subToComplete := prefix[1:]

	// Find matching teams
	for _, name := range registry.ListTeams() {
		if strings.HasPrefix(strings.ToLower(name), subToComplete) {
			results = append(results, "@"+name)
		}
	}

	// Scan all team directories for agent names too!
	for _, dir := range registry.TeamDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			agentName := strings.ToLower(strings.TrimSuffix(entry.Name(), ".md"))
			if strings.HasPrefix(agentName, subToComplete) {
				results = append(results, "@"+agentName)
			}
		}
	}

	unique := make(map[string]bool)
	for _, r := range results {
		unique[r] = true
	}
	var sorted []string
	for k := range unique {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	return sorted
}

func injectFileContexts(prompt string) (string, string) {
	re := regexp.MustCompile(`\B#([^\s#]+)`)
	matches := re.FindAllStringSubmatch(prompt, -1)
	if len(matches) == 0 {
		return prompt, ""
	}

	var additions strings.Builder
	replacedPrompt := prompt
	injected := make(map[string]bool)
	var missing []string

	for _, match := range matches {
		rawPath := match[1]
		cleanPath := strings.TrimRight(rawPath, ".,;:!?\")'")
		if injected[cleanPath] {
			continue
		}
		injected[cleanPath] = true

		fi, err := os.Stat(cleanPath)
		if err != nil || fi.IsDir() {
			missing = append(missing, cleanPath)
			continue
		}

		if fi.Size() > 100*1024 {
			stderrLog("%s Warning: File %s is too large (>100KB), skipping context injection.\n", errStyle.Render("⚠"), cleanPath)
			continue
		}

		data, err := os.ReadFile(cleanPath)
		if err != nil {
			continue
		}

		isBinary := false
		for i := 0; i < len(data) && i < 1024; i++ {
			if data[i] == 0 {
				isBinary = true
				break
			}
		}
		if isBinary {
			stderrLog("%s Warning: File %s appears to be binary, skipping context injection.\n", errStyle.Render("⚠"), cleanPath)
			continue
		}

		ext := strings.TrimPrefix(filepath.Ext(cleanPath), ".")
		if ext == "" {
			ext = "text"
		}

		fmt.Fprintf(&additions, "\n\n---\nFile: %s\n```%s\n%s\n```", cleanPath, ext, string(data))
		injected[cleanPath] = true

		replacedPrompt = strings.ReplaceAll(replacedPrompt, "#"+rawPath, rawPath)
	}

	if len(missing) > 0 {
		stderrLog("%s %d file reference(s) not found and were skipped: %s\n", errStyle.Render("⚠"), len(missing), strings.Join(missing, ", "))
	}
	if additions.Len() > 0 {
		stderrLog("%s Injected content of %d file(s) into context.\n", doneStyle.Render("✓"), len(injected))
		return replacedPrompt + additions.String(), replacedPrompt
	}

	return prompt, ""
}

func loadPromptTemplate(name string, existingVars map[string]string) (string, error) {
	var searchDirs []string
	cwd, err := os.Getwd()
	if err == nil {
		searchDirs = append(searchDirs, filepath.Join(cwd, ".hufu-templates"))
	}
	home, err := os.UserHomeDir()
	if err == nil {
		searchDirs = append(searchDirs, filepath.Join(home, ".config", "hufu", "templates"))
	}

	var filePath string
	var found bool
	for _, dir := range searchDirs {
		for _, ext := range []string{".md", ".yaml", ".yml", ""} {
			p := filepath.Join(dir, name+ext)
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				filePath = p
				found = true
				break
			}
		}
		if found {
			break
		}
	}

	if !found {
		return "", fmt.Errorf("prompt template %q not found in search directories", name)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read template file: %w", err)
	}

	content := string(data)
	body := content
	var vars []string

	if strings.HasPrefix(content, "---\n") {
		parts := strings.SplitN(content, "---\n", 3)
		if len(parts) >= 3 {
			var metadata struct {
				Description string   `yaml:"description"`
				Vars        []string `yaml:"vars"`
			}
			if yaml.Unmarshal([]byte(parts[1]), &metadata) == nil {
				vars = metadata.Vars
				body = parts[2]
				if metadata.Description != "" {
					stderrLog("%s Template: %s (%s)\n", boldStyle.Render("📋"), name, metadata.Description)
				}
			}
		}
	}

	if len(vars) > 0 {
		for _, v := range vars {
			v = strings.TrimSpace(v)
			if _, ok := existingVars[v]; ok {
				continue
			}

			if opts.unattended {
				return "", fmt.Errorf("template variable %q is required but not provided in unattended mode", v)
			}

			var val string
			var inputErr error
			promptStr := fmt.Sprintf("  Enter value for template variable %s: ", teamStyle.Render(v))

			pr := globalPromptReader.Load()
			if pr != nil {
				val, inputErr = pr.ReadLine(boldStyle.Render(promptStr))
			} else {
				prompt := promptui.Prompt{
					Label: fmt.Sprintf("Enter value for template variable %s", v),
				}
				val, inputErr = prompt.Run()
			}

			if inputErr != nil {
				return "", fmt.Errorf("failed to read variable input: %w", inputErr)
			}
			existingVars[v] = val
		}
	}

	templated := body
	for k, v := range existingVars {
		templated = strings.ReplaceAll(templated, "{{"+k+"}}", v)
		templated = strings.ReplaceAll(templated, "{{ "+k+" }}", v)
	}

	return strings.TrimSpace(templated), nil
}

func injectProjectContext(prompt string) string {
	isGit := false
	if _, err := os.Stat(".git"); err == nil {
		isGit = true
	}

	var sb strings.Builder
	sb.WriteString(prompt)

	if isGit {
		cmd := exec.Command("git", "status", "--short")
		if output, err := cmd.Output(); err == nil && len(output) > 0 {
			sb.WriteString("\n\n---\n## Git Status\n```text\n")
			sb.WriteString(string(output))
			sb.WriteString("```")
		}
	}

	var tree []string
	cwd, err := os.Getwd()
	if err == nil {
		tree = generateDirectoryTree(cwd, "", 0, 2)
	}

	if len(tree) > 0 {
		sb.WriteString("\n\n## Project Directory Structure\n```text\n")
		sb.WriteString(strings.Join(tree, "\n"))
		sb.WriteString("\n```")
	}

	return sb.String()
}

func generateDirectoryTree(dir string, prefix string, depth int, maxDepth int) []string {
	if depth >= maxDepth {
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var lines []string
	skipDirs := map[string]bool{
		".git":         true,
		"node_modules": true,
		"workspace":    true,
		".agent-teams": true,
		"tmp":          true,
		"temp":         true,
		"vendor":       true,
		"dist":         true,
		"build":        true,
	}

	var filtered []os.DirEntry
	for _, e := range entries {
		if skipDirs[e.Name()] {
			continue
		}
		filtered = append(filtered, e)
	}

	for i, e := range filtered {
		isLast := i == len(filtered)-1
		connector := "├── "
		nextPrefix := prefix + "│   "
		if isLast {
			connector = "└── "
			nextPrefix = prefix + "    "
		}

		lines = append(lines, prefix+connector+e.Name())
		if e.IsDir() {
			subLines := generateDirectoryTree(filepath.Join(dir, e.Name()), nextPrefix, depth+1, maxDepth)
			lines = append(lines, subLines...)
		}
	}

	return lines
}
