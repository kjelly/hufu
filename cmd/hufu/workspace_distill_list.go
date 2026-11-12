package main

import (
	"cmp"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	workspacepkg "github.com/kjelly/hufu/internal/workspace"
	"github.com/spf13/cobra"
)

type distillationAuditWorkspace struct {
	ID             string    `json:"id"`
	AuditWorkspace string    `json:"audit_workspace"`
	ModifiedAt     time.Time `json:"modified_at"`
}

func newWorkspaceDistillListCommand(deps workspaceCommandDeps) *cobra.Command {
	var teamName string
	command := &cobra.Command{
		Use: "list", Short: "List distillation audit workspaces without running models or modifying state",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if teamName == "" {
				return errors.New("--team is required")
			}
			stateRoot, err := deps.stateRoot()
			if err != nil {
				return err
			}
			start, err := deps.getwd()
			if err != nil {
				return err
			}
			manager, err := workspacepkg.NewManager(stateRoot, deps.registryOptions...)
			if err != nil {
				return err
			}
			resolution, err := manager.Resolve(cmd.Context(), workspacepkg.ResolveRequest{StartDir: start, TeamName: teamName, Mode: workspacepkg.ResolveExisting})
			if err != nil {
				return err
			}
			entries, err := listDistillationAuditWorkspaces(resolution.ControlRoot)
			if err != nil {
				return err
			}
			return writeDistillationJSON(cmd, entries)
		},
	}
	command.Flags().StringVar(&teamName, "team", "", "Destination team workspace (required)")
	return command
}

func listDistillationAuditWorkspaces(workspace string) ([]distillationAuditWorkspace, error) {
	result := []distillationAuditWorkspace{}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	const parent = "maintenance/task-distillation"
	dir, err := root.Open(parent)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "draft-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		result = append(result, distillationAuditWorkspace{ID: entry.Name(), AuditWorkspace: filepath.Join(workspace, filepath.FromSlash(parent), entry.Name()), ModifiedAt: info.ModTime().UTC()})
	}
	slices.SortFunc(result, func(a, b distillationAuditWorkspace) int {
		return cmp.Or(b.ModifiedAt.Compare(a.ModifiedAt), strings.Compare(a.ID, b.ID))
	})
	return result, nil
}
