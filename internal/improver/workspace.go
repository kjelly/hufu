package improver

import (
	"crypto/rand"
	json "encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/workspace"
)

// WorkspaceMetadata is the durable identity of this explicit control area.
// It is not a managed-workspace registry record or a child runtime identity.
type WorkspaceMetadata struct {
	SchemaVersion int    `json:"schema_version"`
	WorkspaceID   string `json:"workspace_id"`
	SessionID     string `json:"session_id"`
	ControlRoot   string `json:"control_root"`
}

type ProbeWorkspace struct {
	Root     string
	BranchID string
	Metadata WorkspaceMetadata
	locks    io.Closer
	readOnly bool
}

// OpenProbeWorkspace never resolves ambient/default workspaces. A writer
// holds an OS lock until all children, callbacks and stores have drained.
// A reader neither locks nor creates/migrates any file or database.
func OpenProbeWorkspace(path string, readOnly bool) (result *ProbeWorkspace, retErr error) {
	if path == "" {
		return nil, errors.New("explicit probe workspace is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if !readOnly {
		parent, err := openSourceRoot(filepath.Dir(absolute))
		if err != nil {
			return nil, err
		}
		err = parent.Mkdir(filepath.Base(absolute), 0o700)
		closeErr := parent.Close()
		if err != nil && !errors.Is(err, os.ErrExist) {
			return nil, errors.Join(err, closeErr)
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	root, err := openSourceRoot(absolute)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	info, err := root.Stat(".")
	if err != nil || info.Mode().Perm() != 0o700 {
		return nil, errors.Join(err, errors.New("probe workspace must be a private directory (0700)"))
	}
	namespace, err := openObjectDirectory(root, "improvement", readOnly)
	if err != nil {
		return nil, err
	}
	pairedRoot, openErr := openObjectDirectory(namespace, "paired", readOnly)
	if err := errors.Join(openErr, namespace.Close()); err != nil {
		if pairedRoot != nil {
			_ = pairedRoot.Close()
		}
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, pairedRoot.Close()) }()
	owned := &ProbeWorkspace{Root: absolute, readOnly: readOnly}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, owned.Close())
		}
	}()
	if !readOnly {
		// This lock key identifies the physical control area before it has a
		// persisted workspace ID. The metadata ID remains independently minted.
		lockDir, err := openObjectDirectory(pairedRoot, "locks", false)
		if err != nil {
			return nil, err
		}
		if err := lockDir.Close(); err != nil {
			return nil, err
		}
		lockID := "ws_" + paired.BytesDigest([]byte(absolute))[:32]
		owned.locks, err = workspace.AcquireWorkspaceLocks(filepath.Join(absolute, "improvement", "paired"), []string{lockID})
		if err != nil {
			return nil, err
		}
	}
	raw, err := readRegular(pairedRoot, "workspace.json", paired.MaxDataBytes)
	if errors.Is(err, os.ErrNotExist) && !readOnly {
		id, err := workspace.NewIDGenerator(nil).New(workspace.WorkspaceIDKind)
		if err != nil {
			return nil, err
		}
		owned.Metadata = WorkspaceMetadata{1, id, "paired-session-" + rand.Text(), absolute}
		raw, err = paired.CanonicalJSON(owned.Metadata)
		if err != nil {
			return nil, err
		}
		if err := team.AtomicCreateFile(filepath.Join(absolute, "improvement", "paired", "workspace.json"), raw, 0o600); err != nil {
			return nil, err
		}
		if err := syncObjectDirectory(pairedRoot); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &owned.Metadata, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	if owned.Metadata.SchemaVersion != 1 || !opaqueID(owned.Metadata.WorkspaceID) ||
		!opaqueID(owned.Metadata.SessionID) || owned.Metadata.ControlRoot != absolute {
		return nil, errors.New("probe workspace identity mismatch")
	}
	tree, exists, err := team.LoadSavedSessionTree(absolute)
	if err != nil {
		return nil, err
	}
	if !exists {
		if readOnly {
			return nil, errors.New("probe workspace has no durable branch")
		}
		tree = team.NewSessionTree()
		if err := team.SaveSessionTree(absolute, tree); err != nil {
			return nil, err
		}
	}
	if tree.ActiveBranch == "" || tree.Branches[tree.ActiveBranch] == nil {
		return nil, errors.New("probe workspace branch is invalid")
	}
	owned.BranchID = tree.ActiveBranch
	return owned, nil
}

func (w *ProbeWorkspace) Close() error {
	if w == nil || w.locks == nil {
		return nil
	}
	err := w.locks.Close()
	w.locks = nil
	return err
}

func (w *ProbeWorkspace) ChildArea(experiment string) (string, error) {
	if !paired.ValidID(experiment) || w.readOnly {
		return "", errors.New("child area requires a writable experiment workspace")
	}
	root, err := openSourceRoot(filepath.Join(w.Root, "improvement", "paired", experiment))
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	child, err := openObjectDirectory(root, "children", false)
	if err != nil {
		return "", err
	}
	if err := child.Close(); err != nil {
		return "", err
	}
	return filepath.Join(w.Root, "improvement", "paired", experiment, "children"), nil
}
