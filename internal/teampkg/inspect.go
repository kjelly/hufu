package teampkg

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/skill"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/utils"
)

// InspectionReport is the stable, content-free package inspection result.
// It reports declarations and integrity metadata, never secret values or MCP
// environment contents.
type InspectionReport struct {
	PackageSchema          int                  `json:"package_schema"`
	Name                   string               `json:"name"`
	Version                string               `json:"version"`
	Authenticity           string               `json:"authenticity"`
	TeamManifest           string               `json:"team_manifest"`
	ManifestSchema         string               `json:"manifest_schema"`
	ArchiveSHA256          string               `json:"archive_sha256"`
	TeamDigest             string               `json:"team_digest"`
	Integrity              string               `json:"integrity"`
	ArchiveBytes           int64                `json:"archive_bytes"`
	TotalUncompressedBytes int64                `json:"total_uncompressed_bytes"`
	FileCount              int                  `json:"file_count"`
	Files                  []PackageFile        `json:"files"`
	NormalizedTeamName     string               `json:"normalized_team_name"`
	Agents                 []InspectedAgent     `json:"agents"`
	Included               IncludedAssets       `json:"included"`
	External               ExternalRequirements `json:"external"`
	Findings               []Finding            `json:"findings"`
	Compile                CompileResult        `json:"compile"`
}

type PackageFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type InspectedAgent struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

type IncludedAssets struct {
	Skills        []IncludedSkill    `json:"skills"`
	ResultSchemas []string           `json:"result_schemas"`
	GoActions     []IncludedGoAction `json:"go_actions"`
}

type IncludedSkill struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type IncludedGoAction struct {
	Capability string `json:"capability"`
	Source     string `json:"source"`
}

type ExternalRequirements struct {
	Executables      []ExternalExecutable         `json:"executables"`
	MCP              []ExternalMCP                `json:"mcp"`
	ProjectResources []agent.RequiredResourceSpec `json:"project_resources"`
	Skills           []ExternalSkillRequirement   `json:"skills"`
	Paths            []ExternalPathRequirement    `json:"paths"`
}

type ExternalExecutable struct {
	Owner   string `json:"owner"`
	Command string `json:"command"`
}

type ExternalMCP struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Command string `json:"command,omitzero"`
	URL     string `json:"url,omitzero"`
}

type ExternalSkillRequirement struct {
	Name      string `json:"name,omitzero"`
	Reference string `json:"reference,omitzero"`
	Available bool   `json:"available"`
}

type ExternalPathRequirement struct {
	Owner string `json:"owner"`
	Path  string `json:"path"`
}

type Finding struct {
	Path     string `json:"path,omitzero"`
	Field    string `json:"field,omitzero"`
	Category string `json:"category"`
}

type CompileResult struct {
	Status        string `json:"status"`
	ErrorCategory string `json:"error_category,omitzero"`
	Message       string `json:"message,omitzero"`
}

// InspectPackage validates and compiles a package without changing the
// package, workspace, or configured team search paths.
func InspectPackage(filename string) (InspectionReport, error) {
	data, err := readArchiveData(filename)
	if err != nil {
		return InspectionReport{Integrity: "failed", Compile: CompileResult{Status: "not_run"}}, err
	}
	return inspectPackageBytes(data, "")
}

func inspectPackageBytes(data []byte, scratchBase string) (report InspectionReport, resultErr error) {
	report = InspectionReport{
		ArchiveBytes: int64(len(data)), Integrity: "failed",
		Compile: CompileResult{Status: "not_run"},
	}
	sum := sha256.Sum256(data)
	report.ArchiveSHA256 = hex.EncodeToString(sum[:])
	archive, err := ReadArchiveBytes(data)
	if err != nil {
		report.Findings = findingsFromError(err)
		return report, err
	}
	report = reportFromArchive(archive)
	if err := ValidatePackageName(archive.Manifest.Name); err != nil {
		report.Findings = findingsFromError(err)
		return report, err
	}

	scratch, err := os.MkdirTemp(scratchBase, "hufu-team-package-inspect-")
	if err != nil {
		return inspectionFailure(report, "scratch", fmt.Errorf("create inspection scratch directory: %w", err))
	}
	defer func() {
		if cleanupErr := os.RemoveAll(scratch); cleanupErr != nil {
			cleanupErr = fmt.Errorf("remove inspection scratch directory: %w", cleanupErr)
			if resultErr == nil {
				report, resultErr = inspectionFailure(report, "cleanup", cleanupErr)
				return
			}
			resultErr = errors.Join(resultErr, cleanupErr)
		}
	}()
	root := filepath.Join(scratch, archive.Manifest.Name)
	if err := stageArchiveSources(root, archive); err != nil {
		return inspectionFailure(report, "staging", err)
	}
	spec, manifestSchema, err := validateStagedArchive(root, archive)
	if err != nil {
		if staged, ok := errors.AsType[*stagedValidationError](err); ok {
			return inspectionFailure(report, staged.category, staged.err)
		}
		return inspectionFailure(report, "compile", err)
	}

	report.ManifestSchema = manifestSchema
	report.NormalizedTeamName = spec.Name.Value
	report.Agents = inspectAgents(spec.RuntimeSession())
	report.Included = inspectIncluded(archive, spec.RuntimeSession())
	report.External = inspectExternal(root, report.Included, spec.RuntimeSession())
	report.Integrity = "verified"
	report.Compile = CompileResult{Status: "passed"}
	return report, nil
}

func reportFromArchive(archive *Archive) InspectionReport {
	report := InspectionReport{
		PackageSchema: archive.Manifest.SchemaVersion,
		Name:          archive.Manifest.Name, Version: archive.Manifest.Version,
		Authenticity: archive.Manifest.Authenticity, TeamManifest: archive.Manifest.TeamManifest,
		ArchiveSHA256: archive.ArchiveSHA256, TeamDigest: archive.Lock.TeamDigest,
		ArchiveBytes: archive.ArchiveSize, Integrity: "verified",
		Compile: CompileResult{Status: "not_run"},
	}
	paths := make([]string, 0, len(archive.Files))
	for path := range archive.Files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		data := archive.Files[path]
		sum := sha256.Sum256(data)
		report.Files = append(report.Files, PackageFile{Path: path, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))})
		report.TotalUncompressedBytes += int64(len(data))
	}
	report.FileCount = len(report.Files)
	return report
}

func stageArchiveSources(root string, archive *Archive) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create staged team: %w", err)
	}
	paths := make([]string, 0, len(archive.Files))
	for path := range archive.Files {
		if path != ManifestFilename && path != LockFilename {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	for _, path := range paths {
		target := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create staged package directory: %w", err)
		}
		if err := os.WriteFile(target, archive.Files[path], 0o644); err != nil {
			return fmt.Errorf("write staged package entry %q: %w", path, err)
		}
	}
	return nil
}

func compareArchiveInventory(archive *Archive, inventory Inventory) error {
	want := make(map[string][]byte, len(inventory.Files))
	for _, file := range inventory.Files {
		want[file.Path] = file.Data
	}
	if len(want) != len(archive.Files)-2 {
		return errors.New("package source file set does not match compiler-owned inventory")
	}
	for path, expected := range want {
		actual, ok := archive.Files[path]
		if !ok || !bytes.Equal(actual, expected) {
			return fmt.Errorf("package source %q does not match compiler-owned inventory", path)
		}
	}
	return nil
}

type stagedValidationError struct {
	category string
	err      error
}

func (e *stagedValidationError) Error() string { return e.err.Error() }
func (e *stagedValidationError) Unwrap() error { return e.err }

func validateStagedArchive(root string, archive *Archive) (*team.EffectiveTeamSpec, string, error) {
	spec, err := team.CompileTeam(root, nil, nil, nil)
	if err != nil {
		return nil, "", &stagedValidationError{category: "compile", err: err}
	}
	if spec.Name.Value != archive.Manifest.Name {
		return nil, "", &stagedValidationError{category: "identity", err: errors.New("package name does not match compiled team name")}
	}
	manifestSchema, err := team.DetectTeamSchemaVersion(root, nil)
	if err != nil {
		return nil, "", &stagedValidationError{category: "manifest_schema", err: err}
	}
	inventory, err := BuildInventory(root, spec)
	if err != nil {
		return nil, "", &stagedValidationError{category: "structural_inventory", err: err}
	}
	if err := compareArchiveInventory(archive, inventory); err != nil {
		return nil, "", &stagedValidationError{category: "structural_inventory", err: err}
	}
	return spec, manifestSchema, nil
}

func inspectAgents(session *team.TeamSession) []InspectedAgent {
	if session == nil {
		return nil
	}
	seen := make(map[*agent.AgentDef]struct{}, len(session.Agents))
	var result []InspectedAgent
	for _, definition := range session.Agents {
		if definition == nil {
			continue
		}
		if _, ok := seen[definition]; ok {
			continue
		}
		seen[definition] = struct{}{}
		result = append(result, InspectedAgent{Name: definition.Name, Role: definition.Role})
	}
	slices.SortFunc(result, func(a, b InspectedAgent) int { return strings.Compare(a.Name, b.Name) })
	return result
}

func inspectIncluded(archive *Archive, session *team.TeamSession) IncludedAssets {
	result := IncludedAssets{}
	for path, data := range archive.Files {
		if !strings.HasPrefix(path, "skills/") || !strings.HasSuffix(path, "/SKILL.md") {
			continue
		}
		definition, err := skill.ValidateSkill(data)
		if err == nil {
			result.Skills = append(result.Skills, IncludedSkill{Name: definition.Name, Path: path})
		}
	}
	slices.SortFunc(result.Skills, func(a, b IncludedSkill) int { return strings.Compare(a.Name, b.Name) })
	if session == nil {
		return result
	}
	for path := range session.ResultContracts {
		result.ResultSchemas = append(result.ResultSchemas, path)
	}
	slices.Sort(result.ResultSchemas)
	for capability, config := range session.Config.ActionProviders {
		if strings.EqualFold(strings.TrimSpace(config.Runtime), "golang") {
			result.GoActions = append(result.GoActions, IncludedGoAction{Capability: capability, Source: config.Source})
		}
	}
	slices.SortFunc(result.GoActions, func(a, b IncludedGoAction) int { return strings.Compare(a.Capability, b.Capability) })
	return result
}

func inspectExternal(root string, included IncludedAssets, session *team.TeamSession) ExternalRequirements {
	if session == nil {
		return ExternalRequirements{}
	}
	result := ExternalRequirements{ProjectResources: slices.Clone(session.Config.RequiredResources)}
	for capability, config := range session.Config.ActionProviders {
		if len(config.Command) > 0 {
			result.Executables = append(result.Executables, ExternalExecutable{Owner: "action-provider:" + capability, Command: config.Command[0]})
		}
	}
	for name, config := range session.Config.SubagentProviders {
		if len(config.Command) > 0 {
			result.Executables = append(result.Executables, ExternalExecutable{Owner: "subagent-provider:" + name, Command: config.Command[0]})
		}
	}
	for name, config := range session.Config.Backends {
		if len(config.Command) > 0 {
			result.Executables = append(result.Executables, ExternalExecutable{Owner: "backend:" + name, Command: config.Command[0]})
		}
	}
	for name, config := range session.MCPServers {
		requirement := ExternalMCP{Name: name, Type: config.Type, URL: redactExternalURL(config.URL)}
		if len(config.Command) > 0 {
			requirement.Command = config.Command[0]
		}
		result.MCP = append(result.MCP, requirement)
	}
	result.Paths = appendPathRequirements(result.Paths, "team", session.Config.Requirements.Paths)
	seenAgents := make(map[*agent.AgentDef]struct{}, len(session.Agents))
	for _, definition := range session.Agents {
		if definition == nil {
			continue
		}
		if _, ok := seenAgents[definition]; ok {
			continue
		}
		seenAgents[definition] = struct{}{}
		result.Paths = appendPathRequirements(result.Paths, "agent:"+definition.Name, definition.Requirements.Paths)
	}
	result.Skills = inspectExternalSkills(root, included, session)
	deduplicateAndSortExternal(&result)
	return result
}

func appendPathRequirements(dst []ExternalPathRequirement, owner string, paths []string) []ExternalPathRequirement {
	for _, path := range paths {
		dst = append(dst, ExternalPathRequirement{Owner: owner, Path: portableRequirementPath(path)})
	}
	return dst
}

func portableRequirementPath(path string) string {
	if !filepath.IsAbs(path) {
		return filepath.ToSlash(path)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "<absolute-path>"
	}
	relative, err := filepath.Rel(cwd, path)
	if err != nil {
		return "<absolute-path>"
	}
	return filepath.ToSlash(relative)
}

func redactExternalURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		if raw == "" {
			return ""
		}
		return "<configured-url>"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func inspectExternalSkills(root string, included IncludedAssets, session *team.TeamSession) []ExternalSkillRequirement {
	includedNames := make(map[string]bool, len(included.Skills))
	for _, item := range included.Skills {
		includedNames[strings.ToLower(item.Name)] = true
	}
	available := make(map[string]bool)
	ownedSkillsRoot := filepath.Join(root, "skills") + string(filepath.Separator)
	for _, definition := range session.Skills {
		if definition == nil {
			continue
		}
		path, err := filepath.Abs(definition.Path)
		if err == nil && !strings.HasPrefix(path, ownedSkillsRoot) {
			available[strings.ToLower(definition.Name)] = true
		}
	}
	var result []ExternalSkillRequirement
	addNamed := func(name string) {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" || includedNames[key] {
			return
		}
		result = append(result, ExternalSkillRequirement{Name: name, Available: available[key]})
	}
	for _, name := range skill.ParseSkillList(session.Config.Skills) {
		addNamed(name)
	}
	seenAgents := make(map[*agent.AgentDef]struct{}, len(session.Agents))
	for _, definition := range session.Agents {
		if definition == nil {
			continue
		}
		if _, ok := seenAgents[definition]; ok {
			continue
		}
		seenAgents[definition] = struct{}{}
		for _, name := range skill.ParseSkillList(definition.Skills) {
			addNamed(name)
		}
	}
	for _, item := range included.Skills {
		for _, reference := range skill.UnresolvedSkillDependencies(findSkill(session.Skills, item.Name), session.Skills) {
			result = append(result, ExternalSkillRequirement{Reference: reference})
		}
	}
	return result
}

func findSkill(catalog []*skill.SkillDef, name string) *skill.SkillDef {
	for _, definition := range catalog {
		if definition != nil && strings.EqualFold(definition.Name, name) {
			return definition
		}
	}
	return nil
}

func deduplicateAndSortExternal(result *ExternalRequirements) {
	slices.SortFunc(result.Executables, func(a, b ExternalExecutable) int {
		if compared := strings.Compare(a.Owner, b.Owner); compared != 0 {
			return compared
		}
		return strings.Compare(a.Command, b.Command)
	})
	slices.SortFunc(result.MCP, func(a, b ExternalMCP) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(result.ProjectResources, func(a, b agent.RequiredResourceSpec) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(result.Paths, func(a, b ExternalPathRequirement) int {
		if compared := strings.Compare(a.Owner, b.Owner); compared != 0 {
			return compared
		}
		return strings.Compare(a.Path, b.Path)
	})
	slices.SortFunc(result.Skills, func(a, b ExternalSkillRequirement) int {
		if compared := strings.Compare(a.Name, b.Name); compared != 0 {
			return compared
		}
		return strings.Compare(a.Reference, b.Reference)
	})
	result.Executables = slices.Compact(result.Executables)
	result.MCP = slices.Compact(result.MCP)
	result.Paths = slices.Compact(result.Paths)
	result.Skills = slices.Compact(result.Skills)
}

func findingsFromError(err error) []Finding {
	if validation, ok := errors.AsType[*ValidationError](err); ok {
		return []Finding{{Path: validation.Path, Field: validation.Field, Category: validation.Category}}
	}
	return nil
}

func inspectionFailure(report InspectionReport, category string, err error) (InspectionReport, error) {
	message := utils.RedactSecrets(err.Error())
	report.Compile = CompileResult{Status: "failed", ErrorCategory: category, Message: message}
	return report, fmt.Errorf("inspect team package: %s: %s", category, message)
}
