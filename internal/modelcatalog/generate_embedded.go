//go:build ignore

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/kjelly/hufu/internal/modelcatalog"
)

type snapshotFile struct {
	Version string                      `json:"version"`
	Models  []modelcatalog.CatalogModel `json:"models"`
}

type manifestFile struct {
	SourceURL       string `json:"source_url"`
	RawSHA256       string `json:"raw_sha256"`
	SourceCount     int    `json:"source_count"`
	NormalizedCount int    `json:"normalized_count"`
	ArtifactSHA256  string `json:"artifact_sha256"`
}

func main() {
	inputPath := flag.String("input", "../../.models.dev-api.json", "models.dev source JSON")
	outputPath := flag.String("output", "embedded_models.json", "normalized snapshot output")
	manifestPath := flag.String("manifest", "embedded_manifest.json", "snapshot provenance manifest output")
	flag.Parse()

	raw, err := os.ReadFile(*inputPath)
	if err != nil {
		fail("read source", err)
	}
	catalog, err := modelcatalog.ParseJSON(raw)
	if err != nil {
		fail("parse source", err)
	}
	snapshot, err := json.Marshal(snapshotFile{Version: catalog.Version, Models: catalog.ModelsList()})
	if err != nil {
		fail("marshal snapshot", err)
	}
	manifest, err := json.Marshal(manifestFile{
		SourceURL:       modelcatalog.DefaultSourceURL,
		RawSHA256:       sha256Hex(raw),
		SourceCount:     len(catalog.Models),
		NormalizedCount: len(catalog.Models),
		ArtifactSHA256:  sha256Hex(snapshot),
	})
	if err != nil {
		fail("marshal manifest", err)
	}
	if err := os.WriteFile(*outputPath, snapshot, 0o600); err != nil {
		fail("write snapshot", err)
	}
	if err := os.WriteFile(*manifestPath, manifest, 0o600); err != nil {
		fail("write manifest", err)
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func fail(action string, err error) {
	_, _ = fmt.Fprintf(os.Stderr, "modelcatalog generator: %s: %v\n", action, err)
	os.Exit(1)
}
