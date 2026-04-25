package mcp

import (
	"encoding/json"
)

type MCPServerConfig struct {
	Type          string            `json:"type"                       yaml:"type"`
	Command       []string          `json:"command,omitempty"           yaml:"command,omitempty"`
	Environment   map[string]string `json:"environment,omitempty"       yaml:"environment,omitempty"`
	URL           string            `json:"url,omitempty"              yaml:"url,omitempty"`
	AllowedTools  []string          `json:"allowedTools,omitempty"      yaml:"allowedTools,omitempty"`
	ExcludedTools []string          `json:"excludedTools,omitempty"    yaml:"excludedTools,omitempty"`
	NoOAuth       bool              `json:"noOAuth,omitempty"           yaml:"noOAuth,omitempty"`
}

func (s *MCPServerConfig) UnmarshalJSON(data []byte) error {
	type newFormat struct {
		Type          string            `json:"type"`
		Command       []string          `json:"command,omitempty"`
		Environment   map[string]string `json:"environment,omitempty"`
		URL           string            `json:"url,omitempty"`
		AllowedTools  []string          `json:"allowedTools,omitempty"`
		ExcludedTools []string          `json:"excludedTools,omitempty"`
		NoOAuth       bool              `json:"noOAuth,omitempty"`
	}

	var nc newFormat
	if err := json.Unmarshal(data, &nc); err != nil {
		return err
	}
	s.Type = nc.Type
	s.Command = nc.Command
	s.Environment = nc.Environment
	s.URL = nc.URL
	s.AllowedTools = nc.AllowedTools
	s.ExcludedTools = nc.ExcludedTools
	s.NoOAuth = nc.NoOAuth

	if s.Type == "" && len(s.Command) > 0 {
		s.Type = "local"
	}
	if s.Type == "" && s.URL != "" {
		s.Type = "remote"
	}

	return nil
}