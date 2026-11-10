package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileContextRejectsBinaryContentWithoutInferringFromExtension(t *testing.T) {
	for _, test := range []struct {
		name string
		data []byte
		text bool
	}{
		{"UTF8 text", []byte("## 題目\nRésumé context."), true},
		{"JSON text", []byte(`{"message":"ordinary context"}`), true},
		{"PNG bytes", []byte("\x89PNG\r\n\x1a\nprivate pixels"), false},
		{"GIF ASCII header", []byte("GIF89aASCII encoded fixture"), false},
		{"WebP ASCII header", []byte("RIFFabcdWEBPVP8 fixture"), false},
		{"invalid UTF8", []byte{'a', 0xff, 'z'}, false},
		{"NUL beyond initial sniff", append(bytes.Repeat([]byte("a"), 2048), 0), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			// The extension deliberately remains .png even for ordinary text.
			path := filepath.Join(t.TempDir(), "asset.png")
			if err := os.WriteFile(path, test.data, 0o600); err != nil {
				t.Fatal(err)
			}
			prompt := "Inspect #" + path
			result, original := injectFileContexts(prompt)
			if test.text {
				if !strings.Contains(result, string(test.data)) || original == "" {
					t.Fatal("ordinary UTF8 text was rejected based on its filename")
				}
			} else if result != prompt || original != "" {
				t.Fatal("binary content was injected into a text request")
			}
		})
	}
}
