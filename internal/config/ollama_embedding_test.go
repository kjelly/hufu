package config

import "testing"

func TestOllamaEmbeddingModelName(t *testing.T) {
	cases := []struct{ in, want string }{
		{in: "ollama/nomic-embed-text:latest", want: "nomic-embed-text:latest"},
		{in: "nomic-embed-text:latest", want: "nomic-embed-text:latest"},
		{in: "ollama/", want: ""},
		{in: "", want: ""},
		{in: "openai/text-embedding-3-small", want: "openai/text-embedding-3-small"},
		{in: DefaultEmbeddingModel, want: "nomic-embed-text:latest"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := OllamaEmbeddingModelName(tc.in); got != tc.want {
				t.Fatalf("OllamaEmbeddingModelName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
