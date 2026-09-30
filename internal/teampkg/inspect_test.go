package teampkg

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestInspectPackageReportsPortableAndExternalRequirements(t *testing.T) {
	project := t.TempDir()
	t.Chdir(project)
	dir := basicTeam(t, `name: inspect-team
skills: cp203-owned, cp203-external
requires:
  paths: [project/config.yaml]
required-resources:
  - name: project-rules
    kind: project_rules
    path: ../AGENTS.md
    required: true
action-providers:
  formatter:
    runtime: command
    command: [example-formatter, --check]
  normalize:
    runtime: golang
    source: actions/normalize
    mode: trusted-static
mcp-servers:
  docs:
    type: local
    command: [example-mcp, serve]
`)
	writeFixture(t, dir, "worker.md", "---\nname: worker\nrole: reviewer\nskills: cp203-external\nrequires:\n  paths: [review/policy.yaml]\nresult-contract:\n  schema: schemas/result.json\n  require-structured: true\n---\nReview the work.\n")
	writeFixture(t, dir, "schemas/result.json", testResultSchema)
	writeFixture(t, dir, "skills/cp203-owned/SKILL.md", "---\nname: cp203-owned\ndescription: Owned test skill.\n---\nInspect safely.\n")
	writeFixture(t, dir, "actions/normalize/main.go", "package main\nimport (\"context\"; \"io\")\nfunc Run(_ context.Context, _ io.Reader, _ io.Writer) error { return nil }\n")
	output := filepath.Join(t.TempDir(), "inspect-team.hufu")
	if _, err := Pack(PackOptions{TeamDir: dir, Version: "v1", Output: output}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	report, err := inspectPackageBytes(before, scratch)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("inspection changed the package")
	}
	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("inspection scratch was not removed: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(project, "workspace")); !os.IsNotExist(err) {
		t.Fatalf("inspection created a workspace: %v", err)
	}
	if report.PackageSchema != SchemaVersion || report.Name != "inspect-team" || report.Version != "v1" {
		t.Fatalf("package identity = %#v", report)
	}
	if report.Authenticity != AuthenticityUnverified || report.Integrity != "verified" || report.Compile.Status != "passed" {
		t.Fatalf("verification status = authenticity %q, integrity %q, compile %#v", report.Authenticity, report.Integrity, report.Compile)
	}
	if report.ArchiveSHA256 == "" || report.TeamDigest == "" || report.ManifestSchema == "" || report.FileCount != len(report.Files) {
		t.Fatalf("integrity metadata incomplete: %#v", report)
	}
	if report.NormalizedTeamName != "inspect-team" || !slices.ContainsFunc(report.Agents, func(item InspectedAgent) bool {
		return item.Name == "worker" && item.Role == "reviewer"
	}) {
		t.Fatalf("compiled team projection = %q, agents %#v", report.NormalizedTeamName, report.Agents)
	}
	if !slices.Contains(report.Included.ResultSchemas, "schemas/result.json") || !slices.ContainsFunc(report.Included.Skills, func(item IncludedSkill) bool {
		return item.Name == "cp203-owned"
	}) || !slices.ContainsFunc(report.Included.GoActions, func(item IncludedGoAction) bool {
		return item.Capability == "normalize" && item.Source == "actions/normalize"
	}) {
		t.Fatalf("included assets = %#v", report.Included)
	}
	if !slices.ContainsFunc(report.External.Executables, func(item ExternalExecutable) bool {
		return item.Owner == "action-provider:formatter" && item.Command == "example-formatter"
	}) || !slices.ContainsFunc(report.External.MCP, func(item ExternalMCP) bool {
		return item.Name == "docs" && item.Command == "example-mcp"
	}) || len(report.External.ProjectResources) != 1 {
		t.Fatalf("external runtime requirements = %#v", report.External)
	}
	if !slices.ContainsFunc(report.External.Skills, func(item ExternalSkillRequirement) bool {
		return item.Name == "cp203-external" && !item.Available
	}) || !slices.ContainsFunc(report.External.Paths, func(item ExternalPathRequirement) bool {
		return item.Owner == "agent:worker" && item.Path == "review/policy.yaml"
	}) {
		t.Fatalf("external skill/path requirements = %#v", report.External)
	}
}

func TestInspectPackageReportsValidationFindingWithoutStaging(t *testing.T) {
	data := rawZIP(t, []rawZipEntry{{name: "../escape", data: []byte("secret material"), method: 0}})
	scratch := t.TempDir()
	report, err := inspectPackageBytes(data, scratch)
	if err == nil {
		t.Fatal("unsafe package was accepted")
	}
	if report.Integrity != "failed" || report.Compile.Status != "not_run" || report.ArchiveSHA256 == "" {
		t.Fatalf("failure report = %#v", report)
	}
	if len(report.Findings) != 1 || !strings.HasPrefix(report.Findings[0].Category, "path_") {
		t.Fatalf("findings = %#v", report.Findings)
	}
	entries, readErr := os.ReadDir(scratch)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid package created scratch entries: %v", entries)
	}
}

func TestInspectPackageRejectsUnsafePackageNameBeforeStaging(t *testing.T) {
	files := validPackageFiles(t)
	manifest, err := encodeManifest(Manifest{
		SchemaVersion: SchemaVersion, Name: "../escape", Version: "v1",
		TeamManifest: "team.yaml", Authenticity: AuthenticityUnverified,
	})
	if err != nil {
		t.Fatal(err)
	}
	files[ManifestFilename] = manifest
	lockInputs := make(map[string][]byte, len(files)-1)
	for path, data := range files {
		if path != LockFilename {
			lockInputs[path] = data
		}
	}
	_, lock, err := buildLock(lockInputs)
	if err != nil {
		t.Fatal(err)
	}
	files[LockFilename] = lock
	scratch := t.TempDir()
	report, err := inspectPackageBytes(archiveFromFiles(t, files), scratch)
	if err == nil {
		t.Fatal("unsafe package name was accepted")
	}
	if len(report.Findings) != 1 || report.Findings[0].Category != "package_name_invalid" {
		t.Fatalf("findings = %#v", report.Findings)
	}
	entries, readErr := os.ReadDir(scratch)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("unsafe name created scratch entries: %v", entries)
	}
}

func TestInspectPackageRejectsStructurallyUnownedSource(t *testing.T) {
	files := validPackageFiles(t)
	files["unowned.txt"] = []byte("not compiler-owned\n")
	lockInputs := make(map[string][]byte, len(files)-1)
	for path, data := range files {
		if path != LockFilename {
			lockInputs[path] = data
		}
	}
	_, lock, err := buildLock(lockInputs)
	if err != nil {
		t.Fatal(err)
	}
	files[LockFilename] = lock
	report, err := inspectPackageBytes(archiveFromFiles(t, files), t.TempDir())
	if err == nil {
		t.Fatal("structurally unowned source was accepted")
	}
	if report.Compile.Status != "failed" || report.Compile.ErrorCategory != "structural_inventory" {
		t.Fatalf("compile result = %#v", report.Compile)
	}
}

func TestInspectPackageReportsInvalidBundledSkill(t *testing.T) {
	files := validPackageFiles(t)
	files["skills/broken/SKILL.md"] = []byte("not valid skill frontmatter\n")
	lockInputs := make(map[string][]byte, len(files)-1)
	for path, data := range files {
		if path != LockFilename {
			lockInputs[path] = data
		}
	}
	_, lock, err := buildLock(lockInputs)
	if err != nil {
		t.Fatal(err)
	}
	files[LockFilename] = lock
	report, err := inspectPackageBytes(archiveFromFiles(t, files), t.TempDir())
	if err == nil {
		t.Fatal("package with invalid bundled skill was accepted")
	}
	if !slices.Contains(report.Findings, Finding{Path: "skills/broken/SKILL.md", Field: "skill", Category: "invalid_skill"}) {
		t.Fatalf("findings = %#v", report.Findings)
	}
}

func TestRedactExternalURLRemovesCredentialsAndQuery(t *testing.T) {
	got := redactExternalURL("https://user:password@example.test/mcp?token=secret#fragment")
	if got != "https://example.test/mcp" {
		t.Fatalf("redacted URL = %q", got)
	}
}
