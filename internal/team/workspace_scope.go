package team

import (
	"fmt"
	"os"
	"path/filepath"
)

// WorkspaceScope separates hufu's durable control state from the subject
// workspace that workers are allowed to mutate.
type WorkspaceScope struct {
	ProjectID      string `json:"project_id,omitempty" yaml:"project-id,omitempty"`
	ContextScopeID string `json:"context_scope_id" yaml:"context-scope-id"`
	ControlRoot    string `json:"control_root" yaml:"control-root"`
	SubjectRoot    string `json:"subject_root" yaml:"subject-root"`
	ProjectRoot    string `json:"project_root,omitempty" yaml:"project-root,omitempty"`
	Managed        bool   `json:"managed" yaml:"managed"`
}

// NewCompatibilityWorkspaceScope describes the legacy runtime layout without
// changing its paths. The current working directory remains the subject and
// context identity while the already-resolved session workspace remains the
// durable control root.
func NewCompatibilityWorkspaceScope(controlRoot, subjectRoot string) (WorkspaceScope, error) {
	control, err := canonicalWorkspacePath(controlRoot)
	if err != nil {
		return WorkspaceScope{}, fmt.Errorf("control workspace: %w", err)
	}
	subject, err := canonicalWorkspacePath(subjectRoot)
	if err != nil {
		return WorkspaceScope{}, fmt.Errorf("subject workspace: %w", err)
	}
	return WorkspaceScope{
		ContextScopeID: subject,
		ControlRoot:    control,
		SubjectRoot:    subject,
		ProjectRoot:    subject,
	}, nil
}

// SetWorkspaceScope installs the canonical runtime scope and keeps Workspace
// as the compatibility alias for ControlRoot. Managed scopes are always
// isolated; unmanaged compatibility scopes retain the execution-profile
// validation performed by the CLI boundary.
func (s *TeamSession) SetWorkspaceScope(scope WorkspaceScope) error {
	return s.setWorkspaceScope(scope, true)
}

// SetWorkspacePreviewScope installs a managed dry-run scope that deliberately
// has no persisted project identity. It applies the same path validation as a
// runtime scope while preserving the no-state-creation preview contract.
func (s *TeamSession) SetWorkspacePreviewScope(scope WorkspaceScope) error {
	if !scope.Managed {
		return fmt.Errorf("workspace preview scope must be managed")
	}
	if scope.ProjectID != "" {
		return fmt.Errorf("workspace preview scope must not have a project ID")
	}
	return s.setWorkspaceScope(scope, false)
}

func (s *TeamSession) setWorkspaceScope(scope WorkspaceScope, requireManagedIdentity bool) error {
	if s == nil {
		return fmt.Errorf("team session is nil")
	}
	control, err := canonicalWorkspacePath(scope.ControlRoot)
	if err != nil {
		return fmt.Errorf("control workspace: %w", err)
	}
	subject, err := canonicalWorkspacePath(scope.SubjectRoot)
	if err != nil {
		return fmt.Errorf("subject workspace: %w", err)
	}
	scope.ControlRoot = control
	scope.SubjectRoot = subject
	scope.ProjectRoot = subject
	if scope.ContextScopeID == "" {
		return fmt.Errorf("context scope ID is empty")
	}
	if scope.Managed && requireManagedIdentity && scope.ProjectID == "" {
		return fmt.Errorf("managed workspace scope requires a project ID")
	}
	if scope.Managed {
		if err := ValidateWorkspaceScope(scope); err != nil {
			return err
		}
	}
	s.Scope = scope
	s.Workspace = control
	s.Config.WorkspaceDir = control
	return nil
}

// SetCompatibilityWorkspaceScope binds the legacy runtime paths to the new
// explicit scope contract. It is intentionally side-effect free.
func (s *TeamSession) SetCompatibilityWorkspaceScope(subjectRoot string) error {
	scope, err := NewCompatibilityWorkspaceScope(s.Workspace, subjectRoot)
	if err != nil {
		return err
	}
	return s.SetWorkspaceScope(scope)
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
