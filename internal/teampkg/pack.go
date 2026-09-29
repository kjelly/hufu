package teampkg

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/skill"
	"github.com/kjelly/hufu/internal/team"
)

// PackOptions defines one local, offline team packaging operation.
type PackOptions struct {
	TeamDir string
	Version string
	Output  string
}

// PackResult describes the archive that was written.
type PackResult struct {
	Manifest  Manifest
	Lock      Lock
	Output    string
	FileCount int
}

// Pack compiles a team, stages only its structurally owned sources, recompiles
// that staged copy, and writes a deterministic ZIP without overwriting output.
func Pack(options PackOptions) (PackResult, error) {
	if strings.TrimSpace(options.TeamDir) == "" {
		return PackResult{}, fmt.Errorf("team directory is required")
	}
	if strings.TrimSpace(options.Output) == "" {
		return PackResult{}, fmt.Errorf("output path is required")
	}
	if err := validateVersion(options.Version); err != nil {
		return PackResult{}, err
	}
	source, err := team.CompileTeam(options.TeamDir, nil, nil, team.DefaultProviderRegistry)
	if err != nil {
		return PackResult{}, fmt.Errorf("compile source team: %w", err)
	}
	inventory, err := BuildInventory(options.TeamDir, source)
	if err != nil {
		return PackResult{}, err
	}
	name := source.RuntimeSession().Config.Name
	if err := validateManifestIdentity(name, inventory.TeamManifest, options.Version); err != nil {
		return PackResult{}, err
	}
	stagingParent, err := os.MkdirTemp("", "hufu-team-package-")
	if err != nil {
		return PackResult{}, fmt.Errorf("create package staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stagingParent) }()
	sourceRoot, err := filepath.Abs(options.TeamDir)
	if err != nil {
		return PackResult{}, fmt.Errorf("resolve source team directory: %w", err)
	}
	stagedDir := filepath.Join(stagingParent, filepath.Base(sourceRoot))
	if err := os.Mkdir(stagedDir, 0o755); err != nil {
		return PackResult{}, fmt.Errorf("create staged team directory: %w", err)
	}
	if err := stageInventory(stagedDir, inventory); err != nil {
		return PackResult{}, err
	}
	staged, err := team.CompileTeam(stagedDir, nil, nil, team.DefaultProviderRegistry)
	if err != nil {
		return PackResult{}, fmt.Errorf("unsupported package dependency: staged team does not compile: %w", err)
	}
	if err := compareCompiledTeams(source.RuntimeSession(), staged.RuntimeSession()); err != nil {
		return PackResult{}, err
	}
	manifest := Manifest{
		SchemaVersion: SchemaVersion,
		Name:          name,
		Version:       options.Version,
		TeamManifest:  inventory.TeamManifest,
		Authenticity:  AuthenticityUnverified,
	}
	manifestData, err := encodeManifest(manifest)
	if err != nil {
		return PackResult{}, err
	}
	archiveFiles := make(map[string][]byte, len(inventory.Files)+2)
	archiveFiles[ManifestFilename] = manifestData
	for _, file := range inventory.Files {
		if _, reserved := archiveFiles[file.Path]; reserved || file.Path == LockFilename {
			return PackResult{}, fmt.Errorf("package source path %q conflicts with generated metadata", file.Path)
		}
		archiveFiles[file.Path] = file.Data
	}
	lock, lockData, err := buildLock(archiveFiles)
	if err != nil {
		return PackResult{}, err
	}
	archiveFiles[LockFilename] = lockData
	archive, err := encodeArchive(archiveFiles)
	if err != nil {
		return PackResult{}, err
	}
	output, err := filepath.Abs(options.Output)
	if err != nil {
		return PackResult{}, fmt.Errorf("resolve output path: %w", err)
	}
	if err := writeExclusive(output, archive); err != nil {
		return PackResult{}, err
	}
	return PackResult{Manifest: manifest, Lock: lock, Output: output, FileCount: len(archiveFiles)}, nil
}

func stageInventory(root string, inventory Inventory) error {
	for _, file := range inventory.Files {
		target := filepath.Join(root, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create staging directory for %q: %w", file.Path, err)
		}
		if err := os.WriteFile(target, file.Data, 0o644); err != nil {
			return fmt.Errorf("stage package source %q: %w", file.Path, err)
		}
	}
	return nil
}

type skillSemantics struct {
	Name         string
	Description  string
	AllowedTools string
	Content      string
}

type resultContractSemantics struct {
	ID              string
	SchemaSHA256    string
	CanonicalSchema string
}

type actionProviderSemantics struct {
	Capability string
	Identity   string
}

func compareCompiledTeams(source, staged *team.TeamSession) error {
	checks := []struct {
		name  string
		left  any
		right any
	}{
		{"normalized team config", source.Config, staged.Config},
		{"agent definitions", source.Agents, staged.Agents},
		{"MCP declarations", source.MCPServers, staged.MCPServers},
		{"static task contracts", source.ContractTasks, staged.ContractTasks},
		{"run input definitions", source.RunInputDefinitions, staged.RunInputDefinitions},
		{"invariant catalog", source.InvariantCatalog, staged.InvariantCatalog},
		{"action catalog", source.ActionCatalog, staged.ActionCatalog},
		{"action provider identities", actionProviderView(source), actionProviderView(staged)},
		{"result contracts", resultContractView(source), resultContractView(staged)},
		{"skill definitions", skillView(source.Skills), skillView(staged.Skills)},
		{"decision authoring", source.DecisionAuthoring, staged.DecisionAuthoring},
	}
	for _, check := range checks {
		if !reflect.DeepEqual(check.left, check.right) {
			return fmt.Errorf("unsupported package dependency: staged %s differs from source compilation", check.name)
		}
	}
	return nil
}

func actionProviderView(session *team.TeamSession) []actionProviderSemantics {
	capabilities := make([]string, 0, len(session.Config.ActionProviders))
	for capability := range session.Config.ActionProviders {
		capabilities = append(capabilities, capability)
	}
	slices.Sort(capabilities)
	result := make([]actionProviderSemantics, 0, len(capabilities))
	for _, capability := range capabilities {
		result = append(result, actionProviderSemantics{
			Capability: capability,
			Identity:   session.ProviderRegistry.ProviderName(capability),
		})
	}
	return result
}

func resultContractView(session *team.TeamSession) []resultContractSemantics {
	result := make([]resultContractSemantics, 0, len(session.ResultContracts))
	for _, contract := range session.ResultContracts {
		result = append(result, resultContractSemantics{
			ID: contract.ID, SchemaSHA256: contract.SchemaSHA256,
			CanonicalSchema: string(contract.CanonicalSchema),
		})
	}
	slices.SortFunc(result, func(a, b resultContractSemantics) int { return strings.Compare(a.ID, b.ID) })
	return result
}

func skillView(skills []*skill.SkillDef) []skillSemantics {
	result := make([]skillSemantics, 0, len(skills))
	for _, definition := range skills {
		if definition == nil {
			continue
		}
		result = append(result, skillSemantics{
			Name: definition.Name, Description: definition.Description,
			AllowedTools: definition.AllowedTools, Content: definition.Content,
		})
	}
	slices.SortFunc(result, func(a, b skillSemantics) int {
		if byName := strings.Compare(a.Name, b.Name); byName != 0 {
			return byName
		}
		return strings.Compare(a.Content, b.Content)
	})
	return result
}

func encodeArchive(files map[string][]byte) ([]byte, error) {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, path := range paths {
		header := &zip.FileHeader{Name: path, Method: zip.Store}
		header.SetMode(0o644)
		// ZIP's DOS epoch is 1980-01-01 00:00:00. Set the legacy fields
		// directly so archive/zip does not synthesize an extended timestamp
		// extra field from FileHeader.Modified.
		header.ModifiedDate = 1<<5 | 1 //nolint:staticcheck // Modified would add a nondeterministic-for-format extra field.
		header.ModifiedTime = 0        //nolint:staticcheck // DOS midnight is required with an empty ZIP extra field.
		header.Extra = nil
		header.Comment = ""
		entry, err := writer.CreateHeader(header)
		if err != nil {
			_ = writer.Close()
			return nil, fmt.Errorf("create archive entry %q: %w", path, err)
		}
		if _, err := entry.Write(files[path]); err != nil {
			_ = writer.Close()
			return nil, fmt.Errorf("write archive entry %q: %w", path, err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("finish package archive: %w", err)
	}
	return output.Bytes(), nil
}

func writeExclusive(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create package output %q without overwrite: %w", path, err)
	}
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write package output %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close package output %q: %w", path, err)
	}
	complete = true
	return nil
}
