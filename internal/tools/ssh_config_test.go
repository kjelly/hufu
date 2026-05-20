package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseSSHConfig_Basic(t *testing.T) {
	// Create temp SSH config
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "ssh_config")

	content := `
Host example.com
    User admin
    Port 2222
    IdentityFile ~/.ssh/id_ed25519
`
	os.WriteFile(configFile, []byte(content), 0644)

	config, err := parseSSHConfigFile(configFile, "example.com")
	if err != nil {
		t.Fatalf("parseSSHConfigFile() error: %v", err)
	}

	if config.User != "admin" {
		t.Errorf("User = %q, want admin", config.User)
	}
	if config.Port != 2222 {
		t.Errorf("Port = %d, want 2222", config.Port)
	}
	if config.IdentityFile != "~/.ssh/id_ed25519" {
		t.Errorf("IdentityFile = %q, want ~/.ssh/id_ed25519", config.IdentityFile)
	}
}

func TestParseSSHConfig_Wildcard(t *testing.T) {
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "ssh_config")

	content := `
Host *.example.com
    User webadmin
    Port 22
`
	os.WriteFile(configFile, []byte(content), 0644)

	config, err := parseSSHConfigFile(configFile, "server1.example.com")
	if err != nil {
		t.Fatalf("parseSSHConfigFile() error: %v", err)
	}

	if config.User != "webadmin" {
		t.Errorf("User = %q, want webadmin", config.User)
	}
}

func TestParseSSHConfig_NoMatch(t *testing.T) {
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "ssh_config")

	content := `
Host example.com
    User admin
`
	os.WriteFile(configFile, []byte(content), 0644)

	config, err := parseSSHConfigFile(configFile, "other.com")
	if err != nil {
		t.Fatalf("parseSSHConfigFile() error: %v", err)
	}

	if config.User != "" {
		t.Errorf("User should be empty for non-matching host, got %q", config.User)
	}
}

func TestGetSSHConfig_NoConfigFile(t *testing.T) {
	// This tests the case where ~/.ssh/config doesn't exist
	// Should return empty config, not error
	config, err := GetSSHConfig("example.com")
	if err != nil {
		t.Errorf("GetSSHConfig() should not error when config missing, got %v", err)
	}
	if config == nil {
		t.Error("GetSSHConfig() should return empty config, not nil")
	}
}

func TestMatchHostPattern(t *testing.T) {
	tests := []struct {
		pattern string
		host    string
		want    bool
	}{
		{"*.example.com", "server1.example.com", true},
		{"*.example.com", "example.com", false},
		{"example.com", "example.com", true},
		{"example.com", "other.com", false},
		{"server*", "server1", true},
		{"server*", "client1", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.host, func(t *testing.T) {
			got := matchHostPattern(tt.pattern, tt.host)
			if got != tt.want {
				t.Errorf("matchHostPattern(%q, %q) = %v, want %v", tt.pattern, tt.host, got, tt.want)
			}
		})
	}
}
