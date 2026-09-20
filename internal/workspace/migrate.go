package workspace

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/eventchain"
)

const (
	legacyContextFilename = "context.sqlite"
	operationMigrate      = "migrate"
)

type inventoryEntry struct {
	Path       string
	Type       string
	Mode       os.FileMode
	Size       int64
	LinkTarget string
	Hash       string
}

func (m *WorkspaceManager) Migrate(ctx context.Context, request MigrateRequest) (MigrationResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if request.AllTeams && strings.TrimSpace(request.TeamName) != "" {
		return MigrationResult{}, errors.New("--team and --all-teams are mutually exclusive")
	}
	subjectRoot, project, err := m.migrationSubject(ctx, request)
	if err != nil {
		return MigrationResult{}, err
	}
	sources, err := m.resolveMigrationSources(subjectRoot, request)
	if err != nil {
		return MigrationResult{}, err
	}
	result := MigrationResult{Outcome: "complete", Completed: []MigrationItem{}, Failed: []string{}, Pending: []string{}}
	for index, source := range sources {
		item, itemErr := m.migrateOne(ctx, project, subjectRoot, source.team, source.path)
		if itemErr == nil {
			result.Completed = append(result.Completed, item)
			continue
		}
		if item.OperationID == "" && len(result.Completed) == 0 {
			return MigrationResult{}, itemErr
		}
		result.Outcome = "failed"
		result.Failed = append(result.Failed, source.team)
		for _, pending := range sources[index+1:] {
			result.Pending = append(result.Pending, pending.team)
		}
		return result, itemErr
	}
	return result, nil
}

type migrationSource struct {
	team string
	path string
}

func (m *WorkspaceManager) migrationSubject(ctx context.Context, request MigrateRequest) (string, Project, error) {
	start := request.StartDir
	if start == "" {
		start = "."
	}
	subjectRoot, err := DiscoverSubjectRoot(start)
	if err != nil {
		return "", Project{}, err
	}
	registry, openErr := OpenReadOnly(m.stateRoot)
	if openErr == nil {
		defer func() { _ = registry.Close() }()
		selector := strings.TrimSpace(request.Selector)
		var project Project
		if selector == "" {
			project, err = registry.ResolveProjectByRoot(ctx, subjectRoot)
		} else {
			project, err = registry.ResolveProject(ctx, selector)
		}
		if err == nil {
			return project.SubjectRoot, project, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return "", Project{}, err
		}
	} else if !errors.Is(openErr, ErrNotFound) {
		return "", Project{}, openErr
	}
	if request.Selector != "" {
		if !filepath.IsAbs(request.Selector) {
			return "", Project{}, fmt.Errorf("%w: project selector %q", ErrNotFound, request.Selector)
		}
		subjectRoot, err = DiscoverSubjectRoot(request.Selector)
		if err != nil {
			return "", Project{}, err
		}
	}
	return subjectRoot, Project{}, nil
}

func (m *WorkspaceManager) resolveMigrationSources(subjectRoot string, request MigrateRequest) ([]migrationSource, error) {
	if request.AllTeams {
		root, err := m.resolveLegacyRoot(subjectRoot, request.StartDir, request.LegacyRoot, "")
		if err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, fmt.Errorf("read legacy root: %w", err)
		}
		var sources []migrationSource
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			teamName, normalizeErr := NormalizeTeamName(entry.Name())
			if normalizeErr != nil {
				return nil, fmt.Errorf("invalid legacy team directory %q: %w", entry.Name(), normalizeErr)
			}
			sources = append(sources, migrationSource{team: teamName, path: filepath.Join(root, entry.Name())})
		}
		sort.Slice(sources, func(i, j int) bool { return sources[i].team < sources[j].team })
		if len(sources) == 0 {
			return nil, fmt.Errorf("legacy_source_not_found: no team directories under %q", root)
		}
		return sources, nil
	}
	teamName := request.TeamName
	if strings.TrimSpace(teamName) == "" {
		teamName = "default"
	}
	teamName, err := NormalizeTeamName(teamName)
	if err != nil {
		return nil, err
	}
	root, err := m.resolveLegacyRoot(subjectRoot, request.StartDir, request.LegacyRoot, teamName)
	if err != nil {
		return nil, err
	}
	return []migrationSource{{team: teamName, path: filepath.Join(root, teamName)}}, nil
}

func (m *WorkspaceManager) resolveLegacyRoot(subjectRoot, startDir, explicit, teamName string) (string, error) {
	if explicit != "" {
		root, err := CanonicalExistingDirectory(explicit)
		if err != nil {
			return "", fmt.Errorf("legacy_source_invalid: %w", err)
		}
		if err = validateLegacyRootCandidate(root, teamName); err != nil {
			return "", err
		}
		return root, nil
	}
	start, err := canonicalPath(startDir)
	if err != nil {
		return "", err
	}
	candidates := []string{filepath.Join(start, "workspace"), filepath.Join(subjectRoot, "workspace")}
	canonical := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		root, candidateErr := CanonicalExistingDirectory(candidate)
		if candidateErr != nil {
			continue
		}
		if slices.Contains(canonical, root) {
			continue
		}
		if validateLegacyRootCandidate(root, teamName) == nil {
			canonical = append(canonical, root)
		}
	}
	sort.Strings(canonical)
	switch len(canonical) {
	case 0:
		return "", fmt.Errorf("legacy_source_not_found: no legacy workspace found")
	case 1:
		return canonical[0], nil
	default:
		return "", fmt.Errorf("legacy_source_ambiguous: %s; specify --legacy-root", strings.Join(canonical, ", "))
	}
}

func validateLegacyRootCandidate(root, teamName string) error {
	if teamName == "" {
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				return nil
			}
		}
		return errors.New("legacy_source_not_found: no team directories")
	}
	path := filepath.Join(root, teamName)
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("legacy_source_not_found: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("legacy_source_invalid: %q must be a directory and not a symlink", path)
	}
	return nil
}

func (m *WorkspaceManager) migrateOne(ctx context.Context, knownProject Project, subjectRoot, teamName, source string) (MigrationItem, error) {
	rawInfo, err := os.Lstat(source)
	if err != nil || rawInfo.Mode()&os.ModeSymlink != 0 || !rawInfo.IsDir() {
		return MigrationItem{}, fmt.Errorf("legacy_source_invalid: top-level source must be a directory and not a symlink")
	}
	source, err = CanonicalExistingDirectory(source)
	if err != nil {
		return MigrationItem{}, fmt.Errorf("legacy_source_invalid: %w", err)
	}
	if pathsOverlap(source, m.stateRoot) {
		return MigrationItem{}, fmt.Errorf("scope_overlap: legacy source %q overlaps state root %q", source, m.stateRoot)
	}
	if pathsOverlap(subjectRoot, m.stateRoot) {
		return MigrationItem{}, fmt.Errorf("%w: subject root %q overlaps state root %q", ErrConflict, subjectRoot, m.stateRoot)
	}
	contextScopeID, sourceFacts, hasContext, err := inspectLegacyContext(ctx, source)
	if err != nil {
		return MigrationItem{}, err
	}
	before, err := buildInventory(source)
	if err != nil {
		return MigrationItem{}, err
	}
	registry, err := OpenReadWrite(m.stateRoot, m.registryOptions...)
	if err != nil {
		return MigrationItem{}, err
	}
	defer func() { _ = registry.Close() }()
	project := knownProject
	if project.ID == "" {
		project, err = registry.RegisterProject(ctx, subjectRoot)
		if err != nil {
			return MigrationItem{}, err
		}
	}
	if existing, lookupErr := registry.GetWorkspace(ctx, project.ID, teamName); lookupErr == nil {
		return MigrationItem{}, fmt.Errorf("%w: workspace %s/%s already exists in state %s", ErrConflict, project.ID, teamName, existing.State)
	} else if !errors.Is(lookupErr, ErrNotFound) {
		return MigrationItem{}, lookupErr
	}
	workspaceID, err := registry.idGenerator.New(WorkspaceIDKind)
	if err != nil {
		return MigrationItem{}, err
	}
	operationID, err := registry.idGenerator.New(OperationIDKind)
	if err != nil {
		return MigrationItem{}, err
	}
	now := registry.now().UTC()
	workspace := Workspace{
		ID: workspaceID, ProjectID: project.ID, TeamName: teamName, ContextScopeID: contextScopeID,
		ControlRoot: filepath.Join(project.StateDir, "teams", teamName), State: "creating",
		OperationID: operationID, PendingPath: filepath.Join(m.stateRoot, "staging", operationID),
		CreatedAt: now, UpdatedAt: now,
	}
	locks, err := acquireMigrationLocks(m.stateRoot, source, workspaceID)
	if err != nil {
		return MigrationItem{}, err
	}
	defer func() { _ = locks.Close() }()
	if err = registry.reserveMigratedWorkspace(ctx, workspace); err != nil {
		return MigrationItem{}, err
	}
	item := MigrationItem{TeamName: teamName, Source: source, Workspace: workspace, OperationID: operationID, Warnings: []string{}}
	failed := true
	defer func() {
		if failed {
			_ = registry.failMigration(context.Background(), workspace, "migration_failed")
		}
	}()
	if err = registry.runMigrationHook(MigrationStageReserved); err != nil {
		failed = false
		return item, err
	}
	if err = registry.prepareMigrationStaging(workspace); err != nil {
		return item, err
	}
	if err = copyLegacyTree(source, workspace.PendingPath); err != nil {
		return item, err
	}
	contextDestination := filepath.Join(workspace.PendingPath, legacyContextFilename)
	if hasContext {
		if err = contextstore.BackupSQLiteReadOnly(ctx, filepath.Join(source, legacyContextFilename), contextDestination); err != nil {
			return item, err
		}
	} else {
		repository, openErr := contextstore.OpenSQLite(contextDestination)
		if openErr != nil {
			return item, openErr
		}
		if closeErr := repository.Close(); closeErr != nil {
			return item, closeErr
		}
		item.Warnings = append(item.Warnings, "context_store_created")
	}
	if err = registry.runMigrationHook(MigrationStageCopied); err != nil {
		failed = false
		return item, err
	}
	after, err := buildInventory(source)
	if err != nil {
		return item, err
	}
	if !slices.Equal(before, after) {
		return item, errors.New("legacy_source_mutated: source changed during migration")
	}
	if err = verifyCopiedTree(source, workspace.PendingPath); err != nil {
		return item, err
	}
	destinationFacts, err := contextstore.InspectSQLiteSnapshot(ctx, contextDestination)
	if err != nil {
		return item, err
	}
	if hasContext && !equalSQLiteFacts(sourceFacts, destinationFacts) {
		return item, errors.New("context_snapshot_mismatch: destination facts differ from source")
	}
	if err = verifyLegacySession(workspace.PendingPath); err != nil {
		return item, err
	}
	if err = writeJSONMarker(filepath.Join(workspace.PendingPath, "workspace.json"), WorkspaceMarker{
		SchemaVersion: markerSchemaVersion, WorkspaceID: workspace.ID, ProjectID: workspace.ProjectID,
		ContextScopeID: workspace.ContextScopeID, Team: workspace.TeamName, ManagedBy: "hufu", CreatedAt: workspace.CreatedAt,
	}); err != nil {
		return item, err
	}
	if err = registry.runMigrationHook(MigrationStageVerified); err != nil {
		failed = false
		return item, err
	}
	if err = registry.publishWorkspace(workspace); err != nil {
		return item, err
	}
	// Once final publication succeeds, preserve the creating row on later
	// failures so doctor can verify the exact marker and finalize activation.
	failed = false
	if err = registry.runMigrationHook(MigrationStageRenamed); err != nil {
		return item, err
	}
	if err = registry.runMigrationHook(MigrationStageBeforeActivate); err != nil {
		return item, err
	}
	if err = registry.activateWorkspace(ctx, workspace); err != nil {
		return item, err
	}
	workspace.State, workspace.OperationID, workspace.PendingPath = "active", "", ""
	workspace.UpdatedAt = registry.now().UTC()
	item.Workspace = workspace
	return item, nil
}

func inspectLegacyContext(ctx context.Context, source string) (string, contextstore.SQLiteSnapshotFacts, bool, error) {
	path := filepath.Join(source, legacyContextFilename)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return filepath.Dir(source), contextstore.SQLiteSnapshotFacts{}, false, nil
	} else if err != nil {
		return "", contextstore.SQLiteSnapshotFacts{}, false, err
	}
	facts, err := contextstore.InspectSQLiteSnapshot(ctx, path)
	if err != nil {
		return "", contextstore.SQLiteSnapshotFacts{}, false, err
	}
	switch len(facts.ProjectIDs) {
	case 0:
		return filepath.Dir(source), facts, true, nil
	case 1:
		return facts.ProjectIDs[0], facts, true, nil
	default:
		return "", contextstore.SQLiteSnapshotFacts{}, false, fmt.Errorf("legacy_context_scope_ambiguous: %s", strings.Join(facts.ProjectIDs, ", "))
	}
}

func (r *SQLiteRegistry) reserveMigratedWorkspace(ctx context.Context, workspace Workspace) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT INTO workspaces(id,project_id,team_name,context_scope_id,control_root,state,operation_id,pending_path,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, workspace.ID, workspace.ProjectID, workspace.TeamName, workspace.ContextScopeID, workspace.ControlRoot, workspace.State, workspace.OperationID, workspace.PendingPath, workspace.CreatedAt.UnixMilli(), workspace.UpdatedAt.UnixMilli()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO registry_operations(id,kind,project_id,workspace_id,state,started_at) VALUES(?,?,?,?,?,?)`, workspace.OperationID, operationMigrate, workspace.ProjectID, workspace.ID, "started", workspace.CreatedAt.UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *SQLiteRegistry) prepareMigrationStaging(workspace Workspace) error {
	if err := os.MkdirAll(filepath.Dir(workspace.PendingPath), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(workspace.PendingPath, 0o700); err != nil {
		return err
	}
	if err := writeJSONMarker(filepath.Join(workspace.PendingPath, "operation.json"), OperationMarker{
		SchemaVersion: markerSchemaVersion, OperationID: workspace.OperationID, Kind: operationMigrate,
		WorkspaceID: workspace.ID, FinalControlRoot: workspace.ControlRoot,
	}); err != nil {
		_ = os.RemoveAll(workspace.PendingPath)
		return err
	}
	return nil
}

func (r *SQLiteRegistry) failMigration(ctx context.Context, workspace Workspace, detail string) error {
	if _, err := os.Lstat(workspace.PendingPath); err == nil {
		if !markerMatchesOperation(workspace.PendingPath, workspace.OperationID, workspace.ID) {
			return errors.New("refusing to clean staging without an exact operation marker")
		}
		if err = os.RemoveAll(workspace.PendingPath); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, _ = tx.ExecContext(ctx, "DELETE FROM workspaces WHERE id=? AND state='creating' AND operation_id=?", workspace.ID, workspace.OperationID)
	_, _ = tx.ExecContext(ctx, "UPDATE registry_operations SET state='failed',detail_code=?,finished_at=? WHERE id=? AND state='started'", detail, r.now().UTC().UnixMilli(), workspace.OperationID)
	return tx.Commit()
}

func (r *SQLiteRegistry) runMigrationHook(stage MigrationStage) error {
	if r.migrateHook == nil {
		return nil
	}
	if err := r.migrateHook(stage); err != nil {
		return fmt.Errorf("simulated migration interruption at %s: %w", stage, err)
	}
	return nil
}

func buildInventory(root string) ([]inventoryEntry, error) {
	var result []inventoryEntry
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == legacyContextFilename+"-shm" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		item := inventoryEntry{Path: filepath.ToSlash(relative), Mode: info.Mode(), Size: info.Size()}
		switch {
		case info.Mode().IsRegular():
			item.Type = "file"
			hash, hashErr := hashFile(path)
			if hashErr != nil {
				return hashErr
			}
			item.Hash = hash
		case info.IsDir():
			item.Type = "directory"
		case info.Mode()&os.ModeSymlink != 0:
			item.Type = "symlink"
			item.LinkTarget, err = os.Readlink(path)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("legacy_source_invalid: unsupported entry %q", path)
		}
		result = append(result, item)
		return nil
	})
	return result, err
}

func copyLegacyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == legacyContextFilename || relative == legacyContextFilename+"-wal" || relative == legacyContextFilename+"-shm" {
			return nil
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			if err = os.Mkdir(target, info.Mode().Perm()); err != nil {
				return err
			}
			return os.Chmod(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, linkErr := os.Readlink(path)
			if linkErr != nil {
				return linkErr
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return atomicCopyFile(path, target, info.Mode().Perm())
		default:
			return fmt.Errorf("unsupported legacy entry %q", path)
		}
	})
}

func atomicCopyFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	temporary := destination + ".tmp"
	output, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		_ = output.Close()
		if cleanup {
			_ = os.Remove(temporary)
		}
	}()
	if _, err = io.Copy(output, input); err != nil {
		return err
	}
	if err = output.Chmod(mode); err != nil {
		return err
	}
	if err = output.Sync(); err != nil {
		return err
	}
	if err = output.Close(); err != nil {
		return err
	}
	if err = os.Rename(temporary, destination); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func verifyCopiedTree(source, destination string) error {
	sourceInventory, err := buildNonSQLiteInventory(source)
	if err != nil {
		return err
	}
	destinationInventory, err := buildNonSQLiteInventory(destination)
	if err != nil {
		return err
	}
	if !slices.Equal(sourceInventory, destinationInventory) {
		return errors.New("legacy_copy_mismatch: copied files differ from source")
	}
	return nil
}

func buildNonSQLiteInventory(root string) ([]inventoryEntry, error) {
	items, err := buildInventory(root)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(items, func(item inventoryEntry) bool {
		return item.Path == legacyContextFilename || item.Path == legacyContextFilename+"-wal" || item.Path == "operation.json" || item.Path == "workspace.json"
	}), nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func equalSQLiteFacts(first, second contextstore.SQLiteSnapshotFacts) bool {
	return first.Revision == second.Revision && first.SchemaLevel == second.SchemaLevel &&
		slices.Equal(first.ProjectIDs, second.ProjectIDs) && mapsEqual(first.TableRows, second.TableRows)
}

func mapsEqual(first, second map[string]int64) bool {
	if len(first) != len(second) {
		return false
	}
	for key, value := range first {
		if second[key] != value {
			return false
		}
	}
	return true
}

func verifyLegacySession(root string) error {
	sessionPath := filepath.Join(root, "session.json")
	if data, err := os.ReadFile(sessionPath); err == nil {
		var session json.RawMessage
		if jsonErr := json.Unmarshal(data, &session); jsonErr != nil {
			return fmt.Errorf("session_unreadable: %w", jsonErr)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("session_unreadable: %w", err)
	}
	eventPath := filepath.Join(root, "logs", "event_store.jsonl")
	if _, err := os.Stat(eventPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := verifyEventChain(eventPath); err != nil {
		return fmt.Errorf("%s: %w", IssueEventChainInvalid, err)
	}
	return nil
}

func verifyEventChain(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var chainVerifier eventchain.Verifier
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event eventchain.Entry
		if err = json.Unmarshal(line, &event); err != nil {
			return fmt.Errorf("decode event %d: %w", chainVerifier.Sequence()+1, err)
		}
		if err = chainVerifier.Verify(event); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func markerMatchesOperation(root, operationID, workspaceID string) bool {
	marker, err := decodeOperationMarker(filepath.Join(root, "operation.json"))
	return err == nil && marker.OperationID == operationID && marker.WorkspaceID == workspaceID
}

func decodeOperationMarker(path string) (OperationMarker, error) {
	var marker OperationMarker
	data, err := os.ReadFile(path)
	if err != nil {
		return marker, err
	}
	if err = jsonUnmarshalStrict(data, &marker); err != nil {
		return marker, err
	}
	return marker, nil
}

func jsonUnmarshalStrict(data []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
