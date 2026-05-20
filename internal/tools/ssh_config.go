package tools

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SSHConfig represents parsed SSH configuration
type SSHConfig struct {
	Host         string
	User         string
	HostName     string
	Port         int
	IdentityFile string
	ProxyJump    string
	ForwardAgent bool
}

// parseSSHConfigFile parses an SSH config file for a specific host
func parseSSHConfigFile(configPath, host string) (*SSHConfig, error) {
	file, err := os.Open(configPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	config := &SSHConfig{}
	scanner := bufio.NewScanner(file)

	currentHostPattern := ""
	matchFound := false

	hostPattern := regexp.MustCompile(`(?i)^Host\s+(.+)$`)
	keywordPattern := regexp.MustCompile(`(?i)^\s*(\w+)\s+(.+)$`)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip comments and empty lines
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Check for Host directive
		if matches := hostPattern.FindStringSubmatch(line); matches != nil {
			currentHostPattern = matches[1]
			matchFound = false // Reset match for new host block
			continue
		}

		// Check if this host block matches
		if !matchFound && matchHostPattern(currentHostPattern, host) {
			matchFound = true
		}

		// Parse directives in matching block
		if matchFound {
			if matches := keywordPattern.FindStringSubmatch(line); matches != nil {
				keyword := strings.ToLower(matches[1])
				value := matches[2]

				switch keyword {
				case "user":
					config.User = value
				case "hostname":
					config.HostName = value
				case "port":
					fmt.Sscanf(value, "%d", &config.Port)
				case "identityfile":
					config.IdentityFile = value
				case "proxyjump":
					config.ProxyJump = value
				case "forwardagent":
					config.ForwardAgent = strings.ToLower(value) == "yes"
				}
			}
		}
	}

	return config, scanner.Err()
}

// matchHostPattern checks if a host matches a pattern (supports * wildcard)
func matchHostPattern(pattern, host string) bool {
	if pattern == "" {
		return false
	}

	// Convert SSH wildcard to regex
	regexPattern := "^" + strings.ReplaceAll(pattern, "*", ".*") + "$"
	matched, _ := regexp.MatchString(regexPattern, host)
	return matched
}

// GetSSHConfig parses ~/.ssh/config for a host
func GetSSHConfig(host string) (*SSHConfig, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	configPath := filepath.Join(homeDir, ".ssh", "config")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return &SSHConfig{}, nil // No config file, return empty config
	}

	return parseSSHConfigFile(configPath, host)
}
