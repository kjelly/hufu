package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install hufu binary globally and setup shell completions",
	Long: `Compiles or copies the currently running hufu binary into the user's local bin
directory (~/.local/bin) so that it can be called easily from anywhere.
Also prints instructions for enabling shell autocompletion.`,
	RunE: runInstall,
}

func runInstall(cmd *cobra.Command, args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to locate user home directory: %w", err)
	}

	destDir := filepath.Join(home, ".local", "bin")
	destPath := filepath.Join(destDir, "hufu")

	fmt.Fprintf(os.Stderr, "Installing hufu to: %s\n", destPath)

	// Create directory if not exists
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	srcPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get current executable path: %w", err)
	}

	// If source and destination are the same, no-op
	if srcPath == destPath {
		fmt.Fprintf(os.Stderr, "hufu is already installed at %s\n", destPath)
		checkPathEnv(destDir)
		showCompletionsHelp()
		return nil
	}

	srcFile, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("failed to open source binary: %w", err)
	}
	defer func() { _ = srcFile.Close() }()

	destFile, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("failed to create destination binary: %w", err)
	}
	defer func() { _ = destFile.Close() }()

	if _, err := io.Copy(destFile, srcFile); err != nil {
		return fmt.Errorf("failed to copy binary contents: %w", err)
	}

	fmt.Fprintf(os.Stderr, "✓ hufu binary successfully installed to %s!\n\n", destPath)

	checkPathEnv(destDir)
	showCompletionsHelp()

	return nil
}

func checkPathEnv(destDir string) {
	pathEnv := os.Getenv("PATH")
	paths := filepath.SplitList(pathEnv)

	found := false
	for _, p := range paths {
		if strings.TrimSuffix(p, "/") == strings.TrimSuffix(destDir, "/") {
			found = true
			break
		}
	}

	if !found {
		fmt.Fprintf(os.Stderr, "⚠ WARNING: %s is not in your PATH environment variable!\n", destDir)
		fmt.Fprintf(os.Stderr, "To add it, append the following line to your shell profile (~/.bashrc, ~/.zshrc, or config.nu):\n\n")

		shell := os.Getenv("SHELL")
		if strings.Contains(shell, "fish") {
			fmt.Fprintf(os.Stderr, "  fish: set -U fish_user_paths %s $fish_user_paths\n", destDir)
		} else if strings.Contains(shell, "nu") {
			fmt.Fprintf(os.Stderr, "  nushell: $env.PATH = ($env.PATH | append '%s')\n", destDir)
		} else {
			fmt.Fprintf(os.Stderr, "  bash/zsh: export PATH=\"$HOME/.local/bin:$PATH\"\n")
		}
		fmt.Fprintf(os.Stderr, "\n")
	}
}

func showCompletionsHelp() {
	fmt.Fprintf(os.Stderr, "─── Shell Completion Setup ───\n")
	fmt.Fprintf(os.Stderr, "To enable tab autocompletion, configure your shell:\n\n")

	shell := os.Getenv("SHELL")
	switch {
	case strings.Contains(shell, "zsh"):
		fmt.Fprintf(os.Stderr, "  Zsh:\n")
		fmt.Fprintf(os.Stderr, "    hufu completion zsh > ~/.zshrc\n")
	case strings.Contains(shell, "fish"):
		fmt.Fprintf(os.Stderr, "  Fish:\n")
		fmt.Fprintf(os.Stderr, "    hufu completion fish > ~/.config/fish/completions/hufu.fish\n")
	case strings.Contains(shell, "nu"):
		fmt.Fprintf(os.Stderr, "  Nushell:\n")
		fmt.Fprintf(os.Stderr, "    hufu completion nushell > ~/.config/nushell/completions/hufu-completion.nu\n")
		fmt.Fprintf(os.Stderr, "    (Then add 'use ~/.config/nushell/completions/hufu-completion.nu *' to your Nushell config.nu)\n")
	default:
		fmt.Fprintf(os.Stderr, "  Bash:\n")
		fmt.Fprintf(os.Stderr, "    hufu completion bash > ~/.bash_completion\n")
	}
	fmt.Fprintf(os.Stderr, "\n")
}
