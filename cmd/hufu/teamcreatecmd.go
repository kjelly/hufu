package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	internalteam "github.com/kjelly/hufu/internal/team"
)

var (
	teamCreatePreset   string
	teamCreateFrom     string
	teamCreateModel    string
	teamCreateForce    bool
	teamCreateExpanded bool
	teamCreateWizard   bool
)

var teamCreateCmd = &cobra.Command{
	Use:   "create NAME",
	Short: "Create the smallest runnable agent team",
	Long: `Create a new team under .agent-teams/<name>/.

With no flags, scaffolds a single generic worker (the coding-single team
preset). --preset selects a deterministic team preset (coding-single,
coding-reviewed, research, safe-ops) or a single-worker agent preset
(readonly, coding, review, research, writer, ops). --from generates a
task-specific team from a natural-language description using the same
deterministic classifier as ` + "`hufu team generate`" + ` (no model call).

A small team.yaml is written only when --model is given, since every
other setting already has a built-in default; --expanded additionally
pins the built-in defaults for users who prefer configuration pinning.

The generated team is compiled and validated before anything is reported
as ready. Existing directories are never overwritten without --force.

--wizard is an explicit terminal-only authoring flow. It previews and
statically validates a conservative definition before an exact write
confirmation. The wizard never honors --force, runs the team, contacts a
provider, or enables active memory learning.

Examples:
  hufu team create dev
  hufu team create dev --preset coding-reviewed
  hufu team create dev --wizard
  hufu team create dev --preset coding-reviewed --model ollama/qwen3.5:27b
  hufu team create oauth-fix --from "Fix OAuth callback bugs and add regression tests"`,
	Args: cobra.ExactArgs(1),
	RunE: runTeamCreate,
}

func init() {
	teamCmd.AddCommand(teamCreateCmd)
	teamCreateCmd.Flags().StringVar(&teamCreatePreset, "preset", "", "Team or agent preset name")
	teamCreateCmd.Flags().StringVar(&teamCreateFrom, "from", "", "Generate a task-specific team from a natural-language description")
	teamCreateCmd.Flags().StringVar(&teamCreateModel, "model", "", "Pin a model in team.yaml")
	teamCreateCmd.Flags().BoolVar(&teamCreateForce, "force", false, "Overwrite an existing team directory")
	teamCreateCmd.Flags().BoolVar(&teamCreateExpanded, "expanded", false, "Write a fully explicit team.yaml pinning built-in defaults")
	teamCreateCmd.Flags().BoolVar(&teamCreateWizard, "wizard", false, "Interactively preview and validate a conservative team before writing")
}

func runTeamCreate(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if name == "" {
		return fmt.Errorf("team name required: hufu team create <name>")
	}
	if strings.TrimSpace(teamCreatePreset) != "" && strings.TrimSpace(teamCreateFrom) != "" {
		return fmt.Errorf("--preset and --from cannot be used together")
	}
	if teamCreateWizard {
		if teamCreateForce || strings.TrimSpace(teamCreateFrom) != "" || teamCreateExpanded {
			return fmt.Errorf("--wizard cannot be combined with --force, --from, or --expanded")
		}
		file, ok := cmd.InOrStdin().(*os.File)
		if !ok || !term.IsTerminal(file.Fd()) {
			return fmt.Errorf("team create --wizard requires an interactive TTY; use hufu team create %s --preset readonly --model <target>", name)
		}
		return runTeamCreateWizardSession(cmd, name, bufio.NewReader(file), cmd.OutOrStdout())
	}

	var files map[string]string
	if strings.TrimSpace(teamCreateFrom) != "" {
		normalized, err := normalizeGeneratedTeamName(name)
		if err != nil {
			return err
		}
		name = normalized
		generated := buildGeneratedTeam(name, teamCreateFrom, teamCreateModel)
		if err := validateGeneratedTeam(generated); err != nil {
			return fmt.Errorf("generated team is invalid: %w", err)
		}
		files = generated.Files
	} else {
		presetName := teamCreatePreset
		if strings.TrimSpace(presetName) == "" {
			presetName = "coding-single"
		}
		resolved, err := resolvePresetFiles(presetName)
		if err != nil {
			return err
		}
		files = resolved
		if strings.TrimSpace(teamCreateModel) != "" {
			files["team.yaml"] = "model: " + teamCreateModel + "\n"
		}
	}

	if teamCreateExpanded {
		files["team.yaml"] = applyExpandedDefaults(files["team.yaml"])
	}

	teamDir := filepath.Join(".agent-teams", name)
	if err := writeTeamFilesWithValidation(teamDir, files, teamCreateForce); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "\n%s Team created: %s\n", doneStyle.Render("✓"), teamStyle.Render(teamDir))
	fmt.Fprintf(os.Stderr, "\nTry it:\n  hufu @%s \"say hello\"\n", name)
	fmt.Fprintf(os.Stderr, "\nInspect:\n  hufu team explain %s\n", teamDir)
	fmt.Fprintf(os.Stderr, "\nValidate:\n  hufu team validate %s\n", teamDir)
	return nil
}

type teamWizardValues struct {
	preset, workerModel, coordinatorModel, allowedPaths, verification, learning string
}

func runTeamCreateWizardSession(_ *cobra.Command, name string, input *bufio.Reader, output io.Writer) error {
	normalized, err := normalizeGeneratedTeamName(name)
	if err != nil {
		return err
	}
	target := filepath.Join(".agent-teams", normalized)
	if entries, readErr := os.ReadDir(target); readErr == nil {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		slices.Sort(names)
		return fmt.Errorf("team %s already exists with files: %s; wizard never overwrites existing teams", target, strings.Join(names, ", "))
	} else if !os.IsNotExist(readErr) {
		return fmt.Errorf("inspect team directory %s: %w", target, readErr)
	}

	values := teamWizardValues{preset: strings.TrimSpace(teamCreatePreset), workerModel: strings.TrimSpace(teamCreateModel), learning: "observe"}
	if values.preset == "" {
		values.preset = "readonly"
	}
	questions := []struct {
		label        string
		defaultValue func() string
		set          func(string)
	}{
		{"Purpose/preset (readonly, coding-single, coding-reviewed, research, safe-ops)", func() string { return values.preset }, func(value string) { values.preset = value }},
		{"Worker backend/model (blank uses configured default)", func() string { return values.workerModel }, func(value string) { values.workerModel = value }},
		{"Coordinator LLM (blank uses configured default)", func() string { return values.coordinatorModel }, func(value string) { values.coordinatorModel = value }},
		{"Allowed paths, comma-separated (blank grants no extra paths)", func() string { return values.allowedPaths }, func(value string) { values.allowedPaths = value }},
		{"Objective verification command (blank leaves acceptance unconfigured)", func() string { return values.verification }, func(value string) { values.verification = value }},
		{"Memory learning mode (off or observe; active is never selected here)", func() string { return values.learning }, func(value string) { values.learning = value }},
	}
	for step := 0; step < len(questions); {
		question := questions[step]
		if _, err = fmt.Fprintf(output, "%s [%s] (type 'back' to return): ", question.label, question.defaultValue()); err != nil {
			return err
		}
		value, readErr := readPromotionReviewLine(input)
		if readErr != nil {
			return readErr
		}
		if strings.EqualFold(value, "back") {
			if step > 0 {
				step--
			}
			continue
		}
		if value == "" {
			value = question.defaultValue()
		}
		question.set(value)
		step++
	}
	if values.learning != "off" && values.learning != "observe" {
		return fmt.Errorf("wizard memory learning mode must be off or observe; active requires a separate maintainer decision")
	}
	files, err := buildWizardTeamFiles(normalized, values)
	if err != nil {
		return err
	}
	if err = validateWizardTeamFiles(files); err != nil {
		return fmt.Errorf("wizard static check failed: %w", err)
	}
	if _, err = fmt.Fprintln(output, "\nPreview (validated; no target files written):"); err != nil {
		return err
	}
	for _, filename := range sortedGeneratedFileNames(files) {
		if _, err = fmt.Fprintf(output, "\n--- /dev/null\n+++ %s/%s\n%s", target, filename, files[filename]); err != nil {
			return err
		}
	}
	if wizardPresetMayHaveSideEffects(files) {
		if _, err = fmt.Fprintln(output, "\nSafety: this preset includes bash-capable work; preset names are not an OS sandbox or a network guarantee."); err != nil {
			return err
		}
	}
	if _, err = fmt.Fprint(output, "Type exactly 'write' to create this team (blank cancels): "); err != nil {
		return err
	}
	confirmation, err := readPromotionReviewLine(input)
	if err != nil {
		return err
	}
	if confirmation != "write" {
		_, err = fmt.Fprintln(output, "Cancelled; no team files were written.")
		return err
	}
	if err = writeTeamFilesWithValidation(target, files, false); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Team created: %s\nNext: hufu team check %s\n", target, normalized)
	return err
}

func buildWizardTeamFiles(name string, values teamWizardValues) (map[string]string, error) {
	files, err := resolvePresetFiles(values.preset)
	if err != nil {
		return nil, err
	}
	var config strings.Builder
	fmt.Fprintf(&config, "name: %s\n", name)
	if values.workerModel != "" {
		fmt.Fprintf(&config, "worker-model: %s\n", strconv.Quote(values.workerModel))
	}
	if values.coordinatorModel != "" {
		fmt.Fprintf(&config, "coordinator-model: %s\n", strconv.Quote(values.coordinatorModel))
	}
	paths := strings.Split(values.allowedPaths, ",")
	quotedPaths := make([]string, 0, len(paths))
	for _, path := range paths {
		if path = strings.TrimSpace(path); path != "" {
			quotedPaths = append(quotedPaths, strconv.Quote(path))
		}
	}
	if len(quotedPaths) > 0 {
		fmt.Fprintf(&config, "allowed-paths: [%s]\n", strings.Join(quotedPaths, ", "))
	}
	if values.verification != "" {
		fmt.Fprintf(&config, "acceptance: %s\n", strconv.Quote(values.verification))
	}
	fmt.Fprintf(&config, "memory-learning:\n  mode: %s\n", values.learning)
	files["team.yaml"] = config.String()
	return files, nil
}

func validateWizardTeamFiles(files map[string]string) error {
	base, err := os.MkdirTemp("", "hufu-team-wizard-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(base) }()
	dir := filepath.Join(base, "candidate")
	if err = writeTeamFilesWithValidation(dir, files, false); err != nil {
		return err
	}
	spec, err := internalteam.CompileTeam(dir, nil, nil, internalteam.DefaultProviderRegistry)
	if err != nil {
		return err
	}
	for _, finding := range internalteam.ValidateEffectiveTeam(spec) {
		if finding.Severity == internalteam.FindingSeverityError {
			return fmt.Errorf("%s: %s (%s)", finding.Field, finding.Message, finding.Code)
		}
	}
	return nil
}

func wizardPresetMayHaveSideEffects(files map[string]string) bool {
	for _, content := range files {
		if strings.Contains(content, "preset: coding") || strings.Contains(content, "preset: review") || strings.Contains(content, "preset: ops") || strings.Contains(content, "tools: all") {
			return true
		}
	}
	return false
}
