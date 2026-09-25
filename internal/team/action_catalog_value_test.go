package team

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func catalogTestInputSchema() RunInputSchema {
	closed := false
	maxLength := 16
	return RunInputSchema{
		Type: "object", AdditionalProperties: &closed, RequiredProperties: []string{"service"},
		Properties: map[string]RunInputSchema{
			"service": {Type: "string", MaxLength: &maxLength},
			"count":   {Type: "integer"},
			"tags":    {Type: "array", Items: &RunInputSchema{Type: "integer"}},
		},
	}
}

func TestCanonicalizeCatalogArguments(t *testing.T) {
	schema := catalogTestInputSchema()
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "canonical ordering", raw: `{"count":2,"service":"api"}`, want: `{"count":2,"service":"api"}`},
		{name: "reordered keys hash the same", raw: `{"service":"api","count":2}`, want: `{"count":2,"service":"api"}`},
		{name: "negative zero integer", raw: `{"service":"api","count":-0,"tags":[-0,3]}`, want: `{"count":0,"service":"api","tags":[0,3]}`},
		{name: "duplicate key", raw: `{"service":"api","service":"db"}`, wantErr: "duplicate JSON object key"},
		{name: "case-variant keys stay distinct and fail the closed schema", raw: `{"service":"api","Service":"db"}`, wantErr: "arguments"},
		{name: "not an object", raw: `["api"]`, wantErr: "must be a JSON object"},
		{name: "two values", raw: `{"service":"api"} {}`, wantErr: "multiple JSON values"},
		{name: "fractional integer", raw: `{"service":"api","count":1.0}`, wantErr: "expected integer"},
		{name: "unknown property", raw: `{"service":"api","extra":true}`, wantErr: "arguments"},
		{name: "missing required", raw: `{"count":1}`, wantErr: "arguments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			canonical, hash, err := canonicalizeCatalogArguments(schema, []byte(tt.raw))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(canonical) != tt.want || hash != runInputHash([]byte(tt.want)) {
				t.Fatalf("canonical = %s hash %s, want %s", canonical, hash, tt.want)
			}
		})
	}
}

func TestCanonicalizeCatalogArgumentsRejectsRedactionUnstableValues(t *testing.T) {
	closed := false
	schema := RunInputSchema{Type: "object", AdditionalProperties: &closed, Properties: map[string]RunInputSchema{"note": {Type: "string"}}}
	_, _, err := canonicalizeCatalogArguments(schema, []byte(`{"note":"api_key=sk-abcdefghijklmnopqrstuvwxyz0123456789"}`))
	if !errors.Is(err, errCatalogArgumentsNotRedactionStable) {
		t.Fatalf("error = %v, want redaction-unstable rejection", err)
	}
}

func TestValidateCatalogOutputs(t *testing.T) {
	schema := RunInputSchema{Type: "object", RequiredProperties: []string{"summary"}, Properties: map[string]RunInputSchema{
		"summary": {Type: "string"}, "score": {Type: "number"},
	}}
	tests := []struct {
		name    string
		outputs map[string]any
		wantErr bool
	}{
		{name: "valid", outputs: map[string]any{"summary": "ok", "score": json.Number("0.5")}},
		{name: "extra keys allowed when additional-properties is unset", outputs: map[string]any{"summary": "ok", "other": true}},
		{name: "missing required", outputs: map[string]any{"score": json.Number("1")}, wantErr: true},
		{name: "nil outputs", outputs: nil, wantErr: true},
		{name: "wrong type", outputs: map[string]any{"summary": json.Number("1")}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateCatalogOutputs(schema, tt.outputs); (err != nil) != tt.wantErr {
				t.Fatalf("validateCatalogOutputs() error = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}

func TestMigrateTeamManifestKeepsActionCatalog(t *testing.T) {
	dir := writeActionCatalogTeam(t, actionCatalogTestManifest("", actionCatalogTestEntry))
	original, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	migrated, _, err := MigrateTeamManifestToV1Alpha1(dir)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !strings.Contains(string(migrated), "action-catalog:") {
		t.Fatalf("migrated manifest lost action-catalog:\n%s", migrated)
	}
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), migrated, 0o644); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("load migrated team: %v", err)
	}
	if reloaded.ActionCatalog.Hash != original.ActionCatalog.Hash {
		t.Fatalf("migrated catalog hash %q, want %q", reloaded.ActionCatalog.Hash, original.ActionCatalog.Hash)
	}

	plain := writeActionCatalogTeam(t, "name: plain-team\n"+actionCatalogTestProviders)
	out, _, err := MigrateTeamManifestToV1Alpha1(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "action-catalog") {
		t.Fatalf("migration of a team without a catalog emitted action-catalog:\n%s", out)
	}
}
