package teampkg

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/golangruntime"
	"github.com/kjelly/hufu/internal/skill"
	"github.com/kjelly/hufu/internal/team"
)

// InventoryFile is one compiler-owned source file selected for packaging.
// Data preserves the exact authored bytes used to create the staged copy and
// final archive.
type InventoryFile struct {
	Path string
	Data []byte
}

// Inventory is the closed structural source set of a portable team package.
type Inventory struct {
	TeamManifest string
	Files        []InventoryFile
}

// BuildInventory selects only source classes owned by a team package. It does
// not recursively copy the team directory and deliberately ignores project or
// global resources that merely happen to be visible to compilation.
func BuildInventory(teamDir string, spec *team.EffectiveTeamSpec) (Inventory, error) {
	if spec == nil || spec.RuntimeSession() == nil {
		return Inventory{}, errors.New("compiled team specification is required")
	}
	root, err := filepath.Abs(teamDir)
	if err != nil {
		return Inventory{}, fmt.Errorf("resolve team directory: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return Inventory{}, fmt.Errorf("resolve team directory symlinks: %w", err)
	}
	builder := inventoryBuilder{root: root, files: make(map[string]InventoryFile)}
	manifest, err := builder.addTeamManifest()
	if err != nil {
		return Inventory{}, err
	}
	if err := builder.addTopLevelMarkdown(); err != nil {
		return Inventory{}, err
	}
	if err := builder.addTeamSkills(); err != nil {
		return Inventory{}, err
	}
	if err := builder.addResultContracts(spec.RuntimeSession()); err != nil {
		return Inventory{}, err
	}
	if err := builder.addGoActions(spec.RuntimeSession()); err != nil {
		return Inventory{}, err
	}
	paths := make([]string, 0, len(builder.files))
	for path := range builder.files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	files := make([]InventoryFile, 0, len(paths))
	for _, path := range paths {
		files = append(files, builder.files[path])
	}
	return Inventory{TeamManifest: manifest, Files: files}, nil
}

type inventoryBuilder struct {
	root  string
	files map[string]InventoryFile
}

func (b *inventoryBuilder) addTeamManifest() (string, error) {
	var found []string
	for _, name := range []string{"team.yaml", "team.yml"} {
		_, err := os.Lstat(filepath.Join(b.root, name))
		switch {
		case err == nil:
			found = append(found, name)
		case errors.Is(err, fs.ErrNotExist):
		default:
			return "", fmt.Errorf("inspect %s: %w", name, err)
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("portable team package requires exactly one team.yaml or team.yml; found %d", len(found))
	}
	if err := b.addRelative(found[0]); err != nil {
		return "", err
	}
	return found[0], nil
}

func (b *inventoryBuilder) addTopLevelMarkdown() error {
	entries, err := os.ReadDir(b.root)
	if err != nil {
		return fmt.Errorf("read team directory: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		if err := b.addRelative(entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

func (b *inventoryBuilder) addTeamSkills() error {
	root := filepath.Join(b.root, "skills")
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read team skills: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillRoot := filepath.Join(root, entry.Name())
		raw, readErr := os.ReadFile(filepath.Join(skillRoot, "SKILL.md"))
		if readErr != nil {
			continue
		}
		if _, validateErr := skill.ValidateSkill(raw); validateErr != nil {
			continue
		}
		if walkErr := filepath.WalkDir(skillRoot, func(path string, item fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if item.IsDir() {
				return nil
			}
			return b.addSource(path)
		}); walkErr != nil {
			return fmt.Errorf("inventory skill %q: %w", entry.Name(), walkErr)
		}
	}
	return nil
}

func (b *inventoryBuilder) addResultContracts(session *team.TeamSession) error {
	paths := make([]string, 0, len(session.ResultContracts))
	for id := range session.ResultContracts {
		paths = append(paths, id)
	}
	slices.Sort(paths)
	for _, path := range paths {
		if err := b.addRelative(filepath.FromSlash(path)); err != nil {
			return fmt.Errorf("inventory result contract %q: %w", path, err)
		}
	}
	return nil
}

func (b *inventoryBuilder) addGoActions(session *team.TeamSession) error {
	capabilities := make([]string, 0, len(session.Config.ActionProviders))
	for capability := range session.Config.ActionProviders {
		capabilities = append(capabilities, capability)
	}
	slices.Sort(capabilities)
	for _, capability := range capabilities {
		config := session.Config.ActionProviders[capability]
		if !strings.EqualFold(strings.TrimSpace(config.Runtime), "golang") {
			continue
		}
		program, err := golangruntime.Prepare(b.root, config.Source)
		if err != nil {
			return fmt.Errorf("inventory Go action %q: %w", capability, err)
		}
		files, err := golangruntime.SourceFiles(program)
		if err != nil {
			return fmt.Errorf("inventory Go action %q: %w", capability, err)
		}
		for _, name := range files {
			if err := b.addSource(filepath.Join(program.Source, name)); err != nil {
				return fmt.Errorf("inventory Go action %q: %w", capability, err)
			}
		}
	}
	return nil
}

func (b *inventoryBuilder) addRelative(path string) error {
	return b.addSource(filepath.Join(b.root, path))
}

func (b *inventoryBuilder) addSource(source string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("inspect package source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("package source %q is not a regular file", source)
	}
	relative, err := filepath.Rel(b.root, source)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("package source %q is outside the team directory", source)
	}
	path := filepath.ToSlash(relative)
	if _, exists := b.files[path]; exists {
		return nil
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read package source %q: %w", path, err)
	}
	b.files[path] = InventoryFile{Path: path, Data: data}
	return nil
}
