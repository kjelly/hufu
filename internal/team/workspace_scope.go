package team

import (
	"fmt"
	"os"
	"path/filepath"
)

// WorkspaceScope separates hufu's durable control state from the subject
// workspace that workers are allowed to mutate.
type WorkspaceScope struct {
	ControlRoot string `json:"control_root" yaml:"control-root"`
	SubjectRoot string `json:"subject_root" yaml:"subject-root"`
	ProjectRoot string `json:"project_root,omitempty" yaml:"project-root,omitempty"`
}

func canonicalWorkspacePath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("workspace path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved), nil
	}
	parent, err := canonicalWorkspacePath(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

func isWorkspaceAncestor(parent, child string) bool {
	return parent == child || (parent != string(filepath.Separator) && len(child) > len(parent) && child[:len(parent)] == parent && child[len(parent)] == filepath.Separator)
}

// ValidateWorkspaceSeparation rejects overlap in either direction and
// refuses root/home roots that could make cleanup destroy the control plane.
func ValidateWorkspaceSeparation(control, subject string) error {
	controlPath, err := canonicalWorkspacePath(control)
	if err != nil {
		return fmt.Errorf("control workspace: %w", err)
	}
	subjectPath, err := canonicalWorkspacePath(subject)
	if err != nil {
		return fmt.Errorf("subject workspace: %w", err)
	}
	home, _ := os.UserHomeDir()
	homePath := ""
	if home != "" {
		homePath, _ = canonicalWorkspacePath(home)
	}
	for name, path := range map[string]string{"control": controlPath, "subject": subjectPath} {
		if path == string(filepath.Separator) || (homePath != "" && path == homePath) {
			return fmt.Errorf("%s workspace %q is a protected root", name, path)
		}
	}
	if isWorkspaceAncestor(controlPath, subjectPath) || isWorkspaceAncestor(subjectPath, controlPath) {
		return fmt.Errorf("control and subject workspaces overlap: control=%q subject=%q", controlPath, subjectPath)
	}
	return nil
}

func ValidateWorkspaceScope(scope WorkspaceScope) error {
	if err := ValidateWorkspaceSeparation(scope.ControlRoot, scope.SubjectRoot); err != nil {
		return err
	}
	if scope.ProjectRoot != "" {
		project, err := canonicalWorkspacePath(scope.ProjectRoot)
		if err != nil {
			return fmt.Errorf("project workspace: %w", err)
		}
		control, _ := canonicalWorkspacePath(scope.ControlRoot)
		if isWorkspaceAncestor(project, control) || isWorkspaceAncestor(control, project) {
			return fmt.Errorf("control and project workspaces overlap: control=%q project=%q", control, project)
		}
	}
	return nil
}
