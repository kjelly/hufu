package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/team/preset"
)

func runSingleFileTeamCreate(cmd *cobra.Command, name string) error {
	for _, flag := range []string{"force", "wizard", "expanded", "from", "category"} {
		if cmd != nil && cmd.Flags().Changed(flag) {
			return fmt.Errorf("--single-file cannot be combined with --%s", flag)
		}
	}
	normalized, err := normalizeGeneratedTeamName(name)
	if err != nil {
		return err
	}
	selected := strings.TrimSpace(teamCreatePreset)
	if selected == "" {
		selected = "coding-single"
	}
	declaration, ok := preset.LookupTeam(selected)
	if !ok {
		return fmt.Errorf("--single-file requires a Team Preset; choose one of: %s", strings.Join(preset.TeamNames(), ", "))
	}
	data, err := yaml.Marshal(struct {
		Preset string `yaml:"preset"`
		Model  string `yaml:"model,omitempty"`
	}{Preset: declaration.Name, Model: strings.TrimSpace(teamCreateModel)})
	if err != nil {
		return err
	}
	dir := filepath.Join(".agent-teams", normalized)
	if err := writeSingleFileTeam(dir, data, validateSingleFileTeam); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\n%s Team created: %s\n\nTry it:\n  hufu @%s \"say hello\"\n\nInspect:\n  hufu team explain %s\n\nValidate:\n  hufu team validate %s\n", doneStyle.Render("✓"), teamStyle.Render(dir), normalized, dir, dir)
	return nil
}

func validateSingleFileTeam(dir string) error {
	spec, err := team.CompileTeam(dir, nil, nil, team.DefaultProviderRegistry)
	if err != nil {
		return err
	}
	for _, finding := range team.ValidateEffectiveTeam(spec) {
		if finding.Severity == team.FindingSeverityError {
			return fmt.Errorf("%s: %s (%s)", finding.Field, finding.Message, finding.Code)
		}
	}
	return nil
}

type singleFileManifest interface {
	io.WriteCloser
	Stat() (os.FileInfo, error)
}

// writeSingleFileTeam owns only an exclusively created directory and manifest.
// Cleanup is non-recursive so concurrently added files are never deleted.
func writeSingleFileTeam(dir string, data []byte, validate func(string) error) error {
	return writeSingleFileTeamWithOpen(dir, data, validate, func(path string) (singleFileManifest, error) {
		return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	})
}

func writeSingleFileTeamWithOpen(dir string, data []byte, validate func(string) error, open func(string) (singleFileManifest, error)) (err error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return fmt.Errorf("create new team directory %s: %w", dir, err)
	}
	directoryInfo, err := os.Stat(dir)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "team.yaml")
	var manifestInfo os.FileInfo
	defer func() {
		if err == nil {
			return
		}
		currentDir, statErr := os.Lstat(dir)
		if statErr != nil || !os.SameFile(directoryInfo, currentDir) {
			return
		}
		if current, statErr := os.Lstat(path); manifestInfo != nil && statErr == nil && os.SameFile(manifestInfo, current) {
			_ = os.Remove(path)
		}
		_ = os.Remove(dir)
	}()
	file, err := open(path)
	if err != nil {
		return err
	}
	manifestInfo, err = file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := validate(dir); err != nil {
		return fmt.Errorf("generated single-file team is invalid: %w", err)
	}
	return nil
}
