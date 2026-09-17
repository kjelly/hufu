package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const markerSchemaVersion = 1

type WorkspaceMarker struct {
	SchemaVersion  int       `json:"schema_version"`
	WorkspaceID    string    `json:"workspace_id"`
	ProjectID      string    `json:"project_id"`
	ContextScopeID string    `json:"context_scope_id"`
	Team           string    `json:"team"`
	ManagedBy      string    `json:"managed_by"`
	CreatedAt      time.Time `json:"created_at"`
}

type OperationMarker struct {
	SchemaVersion    int    `json:"schema_version"`
	OperationID      string `json:"operation_id"`
	Kind             string `json:"kind"`
	WorkspaceID      string `json:"workspace_id"`
	FinalControlRoot string `json:"final_control_root"`
}

func writeJSONMarker(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode marker %q: %w", path, err)
	}
	encoded = append(encoded, '\n')
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create marker temporary file %q: %w", temporary, err)
	}
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = os.Remove(temporary)
		}
	}()
	if _, err = file.Write(encoded); err != nil {
		return fmt.Errorf("write marker %q: %w", path, err)
	}
	if err = file.Sync(); err != nil {
		return fmt.Errorf("sync marker %q: %w", path, err)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("close marker %q: %w", path, err)
	}
	if _, err = os.Lstat(path); err == nil {
		return fmt.Errorf("%w: marker already exists at %q", ErrConflict, path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect marker %q: %w", path, err)
	}
	if err = os.Link(temporary, path); err != nil {
		return fmt.Errorf("publish marker %q: %w", path, err)
	}
	if err = os.Remove(temporary); err != nil {
		return fmt.Errorf("remove marker temporary file %q: %w", temporary, err)
	}
	cleanup = false
	return syncDirectory(directoryOf(path))
}

func directoryOf(path string) string {
	return filepath.Dir(path)
}
