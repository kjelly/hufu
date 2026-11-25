package versionstore

import (
	"errors"
	"fmt"
	"os"
)

// subjectDirectory keeps discovery and file I/O on the same admitted directory
// across the whole capture or materialization.
type subjectDirectory struct {
	root      *os.Root
	directory *os.File
	canonical string
	identity  os.FileInfo
}

func openSubjectDirectory(path string, expected os.FileInfo) (*subjectDirectory, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	identity, err := directory.Stat()
	subject := &subjectDirectory{root: root, directory: directory, canonical: path, identity: identity}
	if err != nil {
		return nil, errors.Join(err, subject.close())
	}
	if expected != nil && !os.SameFile(identity, expected) {
		return nil, errors.Join(subjectChanged(), subject.close())
	}
	if err := subject.verify(); err != nil {
		return nil, errors.Join(err, subject.close())
	}
	return subject, nil
}

func subjectChanged() error {
	return fmt.Errorf("%w: the admitted workspace directory changed", ErrVersionedWorkspaceUnresolved)
}

func (d *subjectDirectory) verify() error {
	path, err := CanonicalRoot(d.canonical)
	if err != nil || path != d.canonical {
		return subjectChanged()
	}
	info, err := os.Stat(d.canonical)
	if err != nil || !os.SameFile(d.identity, info) {
		return subjectChanged()
	}
	return nil
}

func (d *subjectDirectory) discoveryPath() string {
	// The trailing slash and dot cause a tree walk to start in the held
	// directory instead of treating the FD link itself as a leaf.
	return fmt.Sprintf("/proc/self/fd/%d/.", d.directory.Fd())
}

func (d *subjectDirectory) close() error {
	return errors.Join(d.directory.Close(), d.root.Close())
}

func operationSubjectRoot(path string, expected os.FileInfo) (string, error) {
	canonical, err := CanonicalRoot(path)
	if err != nil {
		if expected != nil {
			return "", errors.Join(subjectChanged(), err)
		}
		return "", err
	}
	if expected != nil {
		current, statErr := os.Stat(canonical)
		if canonical != path || statErr != nil || !os.SameFile(current, expected) {
			return "", subjectChanged()
		}
	}
	return canonical, nil
}

// CheckSubjectIdentity checks a managed binding before operation planning.
// Capture and materialization check again after opening their own handles.
func CheckSubjectIdentity(path string, expected os.FileInfo) error {
	_, err := operationSubjectRoot(path, expected)
	return err
}
