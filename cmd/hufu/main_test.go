package main

import (
	"testing"
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
		result := providerURLToOllamaAPI(tt.input)
		if result != tt.expected {
			t.Errorf("providerURLToOllamaAPI(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}