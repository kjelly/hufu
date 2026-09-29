package teampkg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestValidateSourceFilesRejectsCredentialAndRuntimeStateNames(t *testing.T) {
	paths := []string{
		"skills/demo/.env", "skills/demo/.env.local", "skills/demo/id_rsa",
		"skills/demo/server.key", "skills/demo/workspace/logs/run.txt", "skills/demo/context.sqlite",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			files := map[string][]byte{"team.yaml": []byte("name: safe\n"), path: []byte("safe")}
			if err := validateSourceFiles("team.yaml", files); err == nil {
				t.Fatal("forbidden source filename was accepted")
			}
		})
	}
}

func TestValidateSourceFilesRejectsKnownSecretContentWithoutEcho(t *testing.T) {
	secrets := []string{
		"-----BEGIN PRIVATE KEY-----\nprivate-material\n-----END PRIVATE KEY-----",
		"sk-abcdefghijklmnopqrstuvwx",
		"ghp_abcdefghijklmnopqrstuvwxyz123456",
		"AKIAABCDEFGHIJKLMNOP",
	}
	for _, secret := range secrets {
		files := map[string][]byte{"team.yaml": []byte("name: safe\n"), "worker.md": []byte(secret)}
		err := validateSourceFiles("team.yaml", files)
		if err == nil {
			t.Fatalf("secret fixture was accepted")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("diagnostic leaked secret: %v", err)
		}
	}
}

func TestScanManifestFieldsRejectsTypedLiteralSecrets(t *testing.T) {
	tests := map[string]string{
		"legacy root":     "name: safe\nprovider-api-key: literal-value\n",
		"v1 spec":         "apiVersion: hufu.io/v1alpha1\nkind: AgentTeam\nmetadata: {name: safe}\nspec:\n  provider-api-key: literal-value\n",
		"named provider":  "name: safe\nproviders:\n  remote:\n    provider-api-key: literal-value\n",
		"backend":         "name: safe\nbackends:\n  remote:\n    provider-api-key: literal-value\n",
		"mcp environment": "name: safe\nmcp-servers:\n  local:\n    type: local\n    command: [server]\n    environment:\n      ACCESS_TOKEN: literal-value\n",
	}
	for name, manifest := range tests {
		t.Run(name, func(t *testing.T) {
			err := scanManifestFields("team.yaml", []byte(manifest))
			if err == nil {
				t.Fatal("literal secret was accepted")
			}
			if strings.Contains(err.Error(), "literal-value") {
				t.Fatalf("diagnostic leaked secret: %v", err)
			}
		})
	}
}

func TestScanManifestFieldsAllowsEnvironmentPlaceholders(t *testing.T) {
	manifest := "name: safe\nprovider-api-key: {@ TEAM_API_KEY @}\nmcp-servers:\n  local:\n    type: local\n    command: [server]\n    environment:\n      ACCESS_TOKEN: {@ MCP_TOKEN @}\n"
	if err := scanManifestFields("team.yaml", []byte(manifest)); err != nil {
		t.Fatal(err)
	}
}

func TestScanManifestFieldsDoesNotTreatArbitraryVarsAsCredentialConfig(t *testing.T) {
	manifest := "name: safe\nvars:\n  provider-api-key: documentation-label\n  mcp-servers:\n    example:\n      environment:\n        ACCESS_TOKEN: documentation-label\n"
	if err := scanManifestFields("team.yaml", []byte(manifest)); err != nil {
		t.Fatal(err)
	}
}

func TestScanManifestFieldsRejectsAbsolutePackageReferences(t *testing.T) {
	for _, source := range []string{"/tmp/action", "C:/action"} {
		manifest := "name: safe\naction-providers:\n  transform:\n    runtime: golang\n    mode: trusted-static\n    source: " + source + "\n"
		if err := scanManifestFields("team.yaml", []byte(manifest)); err == nil || !strings.Contains(err.Error(), "absolute_package_reference") {
			t.Fatalf("source %q error = %v", source, err)
		}
	}
}

func TestValidateInventoryPolicyEnforcesEntryAndSizeLimits(t *testing.T) {
	oversize := Inventory{TeamManifest: "team.yaml", Files: []InventoryFile{{Path: "team.yaml", Data: make([]byte, MaxFileBytes+1)}}}
	if err := validateInventoryPolicy(oversize, &team.TeamSession{}); err == nil || !strings.Contains(err.Error(), "file_too_large") {
		t.Fatalf("oversize error = %v", err)
	}
	shared := make([]byte, MaxFileBytes)
	var files []InventoryFile
	for index := range 9 {
		files = append(files, InventoryFile{Path: "skills/demo/file-" + string(rune('a'+index)), Data: shared})
	}
	total := Inventory{TeamManifest: "team.yaml", Files: files}
	if err := validateInventoryPolicy(total, &team.TeamSession{}); err == nil || !strings.Contains(err.Error(), "total_too_large") {
		t.Fatalf("total size error = %v", err)
	}
	files = make([]InventoryFile, MaxEntries-1)
	for index := range files {
		files[index] = InventoryFile{Path: "skills/demo/file-" + string(rune(0x1000+index))}
	}
	count := Inventory{TeamManifest: "team.yaml", Files: files}
	if err := validateInventoryPolicy(count, &team.TeamSession{}); err == nil || !strings.Contains(err.Error(), "too_many_entries") {
		t.Fatalf("entry count error = %v", err)
	}
}

func TestPackRejectsSymlinkedOwnedSources(t *testing.T) {
	dir := basicTeam(t, "name: portable-team\n")
	skillDir := filepath.Join(dir, "skills", "linked")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, skillDir, "SKILL.md", "---\nname: linked\ndescription: Linked.\n---\nBody.\n")
	target := filepath.Join(t.TempDir(), "target.txt")
	writeFixture(t, filepath.Dir(target), filepath.Base(target), "target")
	if err := os.Symlink(target, filepath.Join(skillDir, "secret.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := Pack(PackOptions{TeamDir: dir, Version: "v1", Output: filepath.Join(t.TempDir(), "team.hufu")})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestPackRunsRawSecretAndAbsoluteReferencePreflight(t *testing.T) {
	tests := map[string]struct {
		manifest string
		worker   string
		want     string
	}{
		"provider key": {
			manifest: "name: portable-team\nprovider-api-key: literal-value\n",
			want:     "literal_provider_api_key",
		},
		"known token": {
			manifest: "name: portable-team\n",
			worker:   "---\nrole: worker\n---\nsk-abcdefghijklmnopqrstuvwx\n",
			want:     "known_token_prefix",
		},
		"absolute action": {
			manifest: "name: portable-team\naction-providers:\n  transform:\n    runtime: golang\n    mode: trusted-static\n    source: /tmp/action\n",
			want:     "absolute_package_reference",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			dir := basicTeam(t, test.manifest)
			if test.worker != "" {
				writeFixture(t, dir, "worker.md", test.worker)
			}
			output := filepath.Join(t.TempDir(), "team.hufu")
			_, err := Pack(PackOptions{TeamDir: dir, Version: "v1", Output: output})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %s", err, test.want)
			}
			if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
				t.Fatalf("rejected pack left output: %v", statErr)
			}
		})
	}
}
