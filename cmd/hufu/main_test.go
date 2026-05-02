package main

import (
	"testing"

	"github.com/anomalyco/hufu/internal/config"
)

func TestProviderURLToOllamaAPI(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"http://localhost:11434/v1", "http://localhost:11434/api"},
		{"http://localhost:11434/v1/", "http://localhost:11434/api"},
		{"http://localhost:11434/", "http://localhost:11434/api"},
		{"http://localhost:11434", "http://localhost:11434/api"},
		{"http://192.168.1.100:11434/v1", "http://192.168.1.100:11434/api"},
	}
	for _, tt := range tests {
		result := config.ProviderURLToOllamaAPI(tt.input)
		if result != tt.expected {
			t.Errorf("config.ProviderURLToOllamaAPI(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}
