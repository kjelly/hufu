package workspace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const registryFilename = "registry.sqlite"

type SQLiteRegistry struct {
	db          *sql.DB
	stateRoot   string
	idGenerator IDGenerator
	now         func() time.Time
	createHook  func(CreateStage) error
	readOnly    bool
}

func OpenReadWrite(stateRoot string, options ...RegistryOption) (*SQLiteRegistry, error) {
	root, err := canonicalPath(stateRoot)
	if err != nil {
		return nil, fmt.Errorf("open workspace registry: %w", err)
	}
	if err = os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create state root: %w", err)
	}
	if err = os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("secure state root: %w", err)
	}
	registryPath := filepath.Join(root, registryFilename)
	if err = ensureRegistryFile(registryPath); err != nil {
		return nil, err
	}
	registry, err := openRegistry(registryPath, root, false, options...)
	if err != nil {
		return nil, err
	}
	if err = migrateRegistry(context.Background(), registry.db, registry.now); err != nil {
		return nil, errors.Join(err, registry.Close())
	}
	return registry, nil
}

func OpenReadOnly(stateRoot string, options ...RegistryOption) (*SQLiteRegistry, error) {
	root, err := canonicalPath(stateRoot)
	if err != nil {
		return nil, fmt.Errorf("open read-only workspace registry: %w", err)
	}
	path := filepath.Join(root, registryFilename)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: registry %q", ErrNotFound, path)
		}
		return nil, fmt.Errorf("open read-only workspace registry: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("open read-only workspace registry: %q is not a regular file", path)
	}
	registry, err := openRegistry(path, root, true, options...)
	if err != nil {
		return nil, err
	}
	if err = verifyRegistryMigrations(context.Background(), registry.db); err != nil {
		return nil, errors.Join(err, registry.Close())
	}
	return registry, nil
}

func ensureRegistryFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf("close new workspace registry: %w", closeErr)
		}
		return nil
	}
	if !os.IsExist(err) {
		return fmt.Errorf("create workspace registry: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect workspace registry: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("workspace registry %q is not a regular file", path)
	}
	if err = os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure workspace registry: %w", err)
	}
	return nil
}

func openRegistry(path, stateRoot string, readOnly bool, options ...RegistryOption) (*SQLiteRegistry, error) {
	configuration := registryOptions{idGenerator: NewIDGenerator(nil), now: time.Now}
	for _, option := range options {
		option(&configuration)
	}
	if configuration.now == nil {
		configuration.now = time.Now
	}
	var dsn string
	if readOnly {
		uri := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
		query := uri.Query()
		query.Set("mode", "ro")
		uri.RawQuery = query.Encode()
		dsn = uri.String()
	} else {
		dsn = path
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open workspace registry: %w", err)
	}
	db.SetMaxOpenConns(1)
	pragma := "PRAGMA busy_timeout=5000; PRAGMA journal_mode=DELETE; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON"
	if readOnly {
		pragma = "PRAGMA busy_timeout=5000; PRAGMA query_only=ON; PRAGMA foreign_keys=ON"
	}
	if _, err = db.Exec(pragma); err != nil {
		return nil, errors.Join(fmt.Errorf("configure workspace registry: %w", err), db.Close())
	}
	return &SQLiteRegistry{
		db:          db,
		stateRoot:   stateRoot,
		idGenerator: configuration.idGenerator,
		now:         configuration.now,
		createHook:  configuration.createHook,
		readOnly:    readOnly,
	}, nil
}

func (r *SQLiteRegistry) Close() error { return r.db.Close() }

func (r *SQLiteRegistry) RegisterProject(ctx context.Context, start string) (Project, error) {
	if r.readOnly {
		return Project{}, errors.New("register project: registry is read-only")
	}
	subjectRoot, err := DiscoverSubjectRoot(start)
	if err != nil {
		return Project{}, err
	}
	project, err := r.ResolveProjectByRoot(ctx, subjectRoot)
	if err == nil {
		return project, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Project{}, err
	}

	slug := projectSlug(subjectRoot)
	for range 8 {
		projectID, idErr := r.idGenerator.New(ProjectIDKind)
		if idErr != nil {
			return Project{}, idErr
		}
		stateDir := filepath.Join(r.stateRoot, "projects", slug+"--"+strings.TrimPrefix(projectID, "prj_")[:16])
		if _, statErr := os.Lstat(stateDir); statErr == nil {
			continue
		} else if !os.IsNotExist(statErr) {
			return Project{}, fmt.Errorf("inspect project state directory: %w", statErr)
		}
		now := r.now().UTC()
		_, err = r.db.ExecContext(ctx, `INSERT INTO projects(id,slug,subject_root,state_dir,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, projectID, slug, subjectRoot, stateDir, "active", now.UnixMilli(), now.UnixMilli())
		if err == nil {
			return Project{ID: projectID, Slug: slug, SubjectRoot: subjectRoot, StateDir: stateDir, Status: "active", CreatedAt: now, UpdatedAt: now}, nil
		}
		if existing, lookupErr := r.ResolveProjectByRoot(ctx, subjectRoot); lookupErr == nil {
			return existing, nil
		}
		var conflicts int
		lookupErr := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM projects WHERE id=? OR state_dir=?", projectID, stateDir).Scan(&conflicts)
		if lookupErr != nil {
			return Project{}, errors.Join(fmt.Errorf("register project: %w", err), lookupErr)
		}
		if conflicts == 0 {
			return Project{}, fmt.Errorf("register project: %w", err)
		}
	}
	return Project{}, fmt.Errorf("%w: could not reserve a unique state directory after 8 attempts", ErrConflict)
}

func (r *SQLiteRegistry) ResolveProjectByRoot(ctx context.Context, root string) (Project, error) {
	canonical, err := canonicalPath(root)
	if err != nil {
		return Project{}, fmt.Errorf("resolve project root: %w", err)
	}
	return scanProject(r.db.QueryRowContext(ctx, projectSelect+" WHERE subject_root=?", canonical))
}

func (r *SQLiteRegistry) ResolveProject(ctx context.Context, selector string) (Project, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return Project{}, fmt.Errorf("%w: empty project selector", ErrNotFound)
	}
	if prefix, ok := validProjectIDPrefix(selector); ok && len(prefix) == 32 && strings.HasPrefix(selector, "prj_") {
		return scanProject(r.db.QueryRowContext(ctx, projectSelect+" WHERE id=?", selector))
	}
	if prefix, ok := validProjectIDPrefix(selector); ok {
		return r.resolveProjectCandidates(ctx, selector, "id LIKE ?", "prj_"+prefix+"%")
	}
	normalizedAlias := strings.ToLower(selector)
	project, err := scanProject(r.db.QueryRowContext(ctx, projectSelect+" WHERE alias=?", normalizedAlias))
	if err == nil {
		return project, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Project{}, err
	}
	if filepath.IsAbs(selector) {
		return r.ResolveProjectByRoot(ctx, selector)
	}
	return r.resolveProjectCandidates(ctx, selector, "slug=?", strings.ToLower(selector))
}

func validProjectIDPrefix(selector string) (string, bool) {
	prefix := strings.TrimPrefix(strings.ToLower(selector), "prj_")
	if len(prefix) < 8 || len(prefix) > 32 {
		return "", false
	}
	for _, value := range prefix {
		if !strings.ContainsRune("0123456789abcdef", value) {
			return "", false
		}
	}
	return prefix, true
}

func (r *SQLiteRegistry) resolveProjectCandidates(ctx context.Context, selector, predicate, value string) (Project, error) {
	rows, err := r.db.QueryContext(ctx, projectSelect+" WHERE "+predicate+" ORDER BY id", value)
	if err != nil {
		return Project{}, fmt.Errorf("resolve project %q: %w", selector, err)
	}
	defer func() { _ = rows.Close() }()
	projects, err := scanProjects(rows)
	if err != nil {
		return Project{}, err
	}
	switch len(projects) {
	case 0:
		return Project{}, fmt.Errorf("%w: project %q", ErrNotFound, selector)
	case 1:
		return projects[0], nil
	default:
		return Project{}, &AmbiguousSelectorError{Selector: selector, Candidates: projects}
	}
}

func (r *SQLiteRegistry) ListProjects(ctx context.Context, options ListOptions) ([]Project, error) {
	query := projectSelect
	arguments := []any(nil)
	if options.Selector != "" {
		query += " WHERE slug=? OR alias=?"
		arguments = append(arguments, strings.ToLower(options.Selector), strings.ToLower(options.Selector))
	}
	rows, err := r.db.QueryContext(ctx, query+" ORDER BY id", arguments...)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanProjects(rows)
}

func (r *SQLiteRegistry) SetAlias(ctx context.Context, selector, value string) error {
	if r.readOnly {
		return errors.New("set alias: registry is read-only")
	}
	alias, err := NormalizeAlias(value)
	if err != nil {
		return err
	}
	project, err := r.ResolveProject(ctx, selector)
	if err != nil {
		return err
	}
	result, err := r.db.ExecContext(ctx, "UPDATE projects SET alias=?, updated_at=? WHERE id=?", alias, r.now().UTC().UnixMilli(), project.ID)
	if err != nil {
		return fmt.Errorf("set project alias: %w", err)
	}
	return requireAffected(result, "project", project.ID)
}

func (r *SQLiteRegistry) ClearAlias(ctx context.Context, selector string) error {
	if r.readOnly {
		return errors.New("clear alias: registry is read-only")
	}
	project, err := r.ResolveProject(ctx, selector)
	if err != nil {
		return err
	}
	result, err := r.db.ExecContext(ctx, "UPDATE projects SET alias=NULL, updated_at=? WHERE id=?", r.now().UTC().UnixMilli(), project.ID)
	if err != nil {
		return fmt.Errorf("clear project alias: %w", err)
	}
	return requireAffected(result, "project", project.ID)
}

func (r *SQLiteRegistry) RebindProject(ctx context.Context, selector, newRoot string) error {
	if r.readOnly {
		return errors.New("rebind project: registry is read-only")
	}
	project, err := r.ResolveProject(ctx, selector)
	if err != nil {
		return err
	}
	canonical, err := CanonicalExistingDirectory(newRoot)
	if err != nil {
		return fmt.Errorf("rebind project root: %w", err)
	}
	if canonical == project.SubjectRoot {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin project rebind: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := r.now().UTC().UnixMilli()
	result, err := tx.ExecContext(ctx, "UPDATE projects SET subject_root=?,updated_at=? WHERE id=?", canonical, now, project.ID)
	if err != nil {
		return fmt.Errorf("update project subject root: %w", err)
	}
	if err = requireAffected(result, "project", project.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE workspaces SET requires_fresh_session=1,updated_at=? WHERE project_id=?", now, project.ID); err != nil {
		return fmt.Errorf("mark active workspaces for fresh session: %w", err)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE trash_workspaces SET requires_fresh_session=1 WHERE project_id=?", project.ID); err != nil {
		return fmt.Errorf("mark trash workspaces for fresh session: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit project rebind: %w", err)
	}
	return nil
}

const projectSelect = `SELECT id,slug,COALESCE(alias,''),subject_root,state_dir,status,created_at,updated_at,last_used_at FROM projects`

type rowScanner interface {
	Scan(...any) error
}

func scanProject(row rowScanner) (Project, error) {
	var project Project
	var createdAt, updatedAt int64
	var lastUsedAt sql.NullInt64
	err := row.Scan(&project.ID, &project.Slug, &project.Alias, &project.SubjectRoot, &project.StateDir, &project.Status, &createdAt, &updatedAt, &lastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("scan project: %w", err)
	}
	project.CreatedAt = time.UnixMilli(createdAt).UTC()
	project.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	project.LastUsedAt = nullableTime(lastUsedAt)
	return project, nil
}

func scanProjects(rows *sql.Rows) ([]Project, error) {
	var projects []Project
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan projects: %w", err)
	}
	return projects, nil
}

func nullableTime(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	parsed := time.UnixMilli(value.Int64).UTC()
	return &parsed
}

func requireAffected(result sql.Result, kind, id string) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("%w: %s %q", ErrNotFound, kind, id)
	}
	return nil
}
