package team

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/runtimestore"
)

func derivedForkIntentFixture(t *testing.T) (*SessionTree, *SessionBranch) {
	t.Helper()
	tree := NewSessionTree()
	child := &SessionBranch{ID: "investigation", Name: "investigation", ParentID: "main", ForkEventID: "source-cutoff", CreatedAt: tree.Branches["main"].CreatedAt}
	child.Derivation = &DerivedForkIntent{
		SchemaVersion: derivedForkIntentVersion, WorkspaceID: "managed-workspace", Workspace: t.TempDir(), Team: "review",
		BranchID: child.ID, SourceBranchID: child.ParentID, SourceEventID: child.ForkEventID,
		SourceEventHash: strings.Repeat("a", 64), SourceSnapshotID: "snapshot-source", SourceSnapshotEventID: "snapshot-event",
		SourceSnapshotEventHash: strings.Repeat("b", 64), SourceRootTreeHash: strings.Repeat("c", 64),
		CaptureOrigin: "known", CaptureSnapshotID: "snapshot-source", CaptureEventID: "snapshot-event", CaptureEventHash: strings.Repeat("b", 64),
		TaskIDFloor: 7,
	}
	tree.Branches[child.ID] = child
	tree.Branches["ordinary"] = &SessionBranch{ID: "ordinary", Name: "ordinary", ParentID: child.ParentID,
		ForkEventID: child.ForkEventID, CreatedAt: child.CreatedAt}
	return tree, child
}

func TestDerivedForkIntentPersistsWithItsBranch(t *testing.T) {
	tree, child := derivedForkIntentFixture(t)
	root := child.Derivation.Workspace
	if err := SaveSessionTree(root, tree); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSessionTree(root)
	if err != nil || !reflect.DeepEqual(loaded.Branches[child.ID].Derivation, child.Derivation) {
		t.Fatalf("branch lost its planned derivation: %v", err)
	}
	clone := cloneSessionTree(loaded)
	clone.Branches[child.ID].Derivation.TaskIDFloor++
	if clone.Branches[child.ID].Derivation.TaskIDFloor == loaded.Branches[child.ID].Derivation.TaskIDFloor {
		t.Fatal("tree clone retained the mutable intent pointer")
	}
	for _, change := range []string{"replace", "remove", "attach_to_existing"} {
		t.Run(change, func(t *testing.T) {
			changed := cloneSessionTree(loaded)
			switch change {
			case "replace":
				changed.Branches[child.ID].Derivation.TaskIDFloor++
			case "remove":
				changed.Branches[child.ID].Derivation = nil
			case "attach_to_existing":
				changed.Branches["ordinary"].Derivation = child.Derivation.clone()
				changed.Branches["ordinary"].Derivation.BranchID = "ordinary"
			}
			if err := SaveSessionTree(root, changed); !errors.Is(err, ErrDerivedForkIntent) {
				t.Fatalf("existing branch changed derivation: %v", err)
			}
			after, err := LoadSessionTree(root)
			if err != nil || !reflect.DeepEqual(after, loaded) {
				t.Fatalf("failed branch update changed durable state: %v", err)
			}
		})
	}
}

func TestDerivedForkIntentRejectsMalformedMetadata(t *testing.T) {
	_, branch := derivedForkIntentFixture(t)
	encoded, err := encodeDerivedForkIntent(branch.Derivation)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"version", "branch", "source", "hash", "capture", "floor", "unknown_field", "duplicate_field", "null"} {
		t.Run(change, func(t *testing.T) {
			intent := branch.Derivation.clone()
			switch change {
			case "version":
				intent.SchemaVersion++
			case "branch":
				intent.BranchID = "other"
			case "source":
				intent.SourceEventID = "other"
			case "hash":
				intent.SourceRootTreeHash = "invalid"
			case "capture":
				intent.CaptureOrigin = "unavailable"
			case "floor":
				intent.TaskIDFloor = -1
			}
			data, err := json.Marshal(intent)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "unknown_field":
				data = append(append([]byte(nil), encoded[:len(encoded)-1]...), []byte(`,"instruction":"run old tasks"}`)...)
			case "duplicate_field":
				data = append(append([]byte(nil), encoded[:len(encoded)-1]...), []byte(`,"schema_version":1}`)...)
			case "null":
				data = []byte("null")
			}
			if _, err := decodeDerivedForkIntent(data, branch); !errors.Is(err, ErrDerivedForkIntent) {
				t.Fatalf("malformed planned intent accepted: %v", err)
			}
		})
	}
}

func TestSessionTreeReadOnlySchemaBeforeDerivation(t *testing.T) {
	root := t.TempDir()
	tree := NewSessionTree()
	if err := SaveSessionTree(root, tree); err != nil {
		t.Fatal(err)
	}
	store, err := OpenEventStore(root)
	if err != nil {
		t.Fatal(err)
	}
	// Build the released schema-5 layout without editing any migration's
	// contents or checksum. Only this disposable fixture is downgraded.
	_, err = store.store.DB().ExecContext(t.Context(), `DROP INDEX events_derived_fork; ALTER TABLE session_branches DROP COLUMN derivation; DELETE FROM schema_migrations WHERE version=6`)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(runtimestore.Path(root))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSessionTreeContext(t.Context(), root)
	if err != nil || loaded.Branches["main"].Derivation != nil {
		t.Fatalf("legacy branch acquired a derivation: %v", err)
	}
	after, err := os.ReadFile(runtimestore.Path(root))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("read-only legacy tree lookup changed its database: %v", err)
	}
}
