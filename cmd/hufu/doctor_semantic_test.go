package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/semantic"
)

// fakeDoctorOllama serves model metadata for embeddinggemma when present.
func fakeDoctorOllama(t *testing.T, present bool) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			models := []map[string]any{}
			if present {
				models = append(models, map[string]any{"name": "embeddinggemma:latest", "digest": "85462619ee721b466c5927d109d4cb765861907d5417b9109caebc4e614679f1", "details": map[string]string{"family": "gemma3", "quantization_level": "BF16"}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
		case "/api/show":
			_ = json.NewEncoder(w).Encode(map[string]any{"model_info": map[string]any{"general.architecture": "gemma3", "gemma3.context_length": 2048, "gemma3.embedding_length": 768}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestCollectDoctorSemantic(t *testing.T) {
	served := fakeDoctorOllama(t, true)
	missing := fakeDoctorOllama(t, false)
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	tests := []struct {
		name        string
		global      semantic.Config
		teams       map[string]semantic.Config
		wantConfig  map[string]string // subject -> status
		wantEncoder string            // status, or "" for no encoder check
		wantReason  string
		wantMessage string
	}{
		{name: "off", wantConfig: map[string]string{"hufu.yaml": "pass"}},
		{
			name: "auto served", global: semantic.Config{Mode: semantic.ModeAuto, Endpoint: served},
			wantConfig: map[string]string{"hufu.yaml": "pass"}, wantEncoder: "pass", wantMessage: "digest 85462619ee72, 768 dimensions, input limit 2048 tokens); used by hufu.yaml",
		},
		{
			name: "team override shares the encoder", global: semantic.Config{Endpoint: served},
			teams:      map[string]semantic.Config{"research": {Mode: semantic.ModeActive}, "review": {Mode: semantic.ModeAuto}},
			wantConfig: map[string]string{"hufu.yaml": "pass", "team research": "pass", "team review": "pass"}, wantEncoder: "pass",
			wantMessage: "used by team research, team review",
		},
		{
			name: "model missing", global: semantic.Config{Mode: semantic.ModeAuto, Endpoint: missing},
			wantConfig: map[string]string{"hufu.yaml": "pass"}, wantEncoder: "warning", wantReason: "semantic_model_missing", wantMessage: "ollama pull embeddinggemma:latest",
		},
		{
			name: "endpoint unreachable", global: semantic.Config{Mode: semantic.ModeAuto, Endpoint: closedURL},
			wantConfig: map[string]string{"hufu.yaml": "pass"}, wantEncoder: "warning", wantReason: "semantic_endpoint_unreachable",
		},
		{
			name: "invalid team block", teams: map[string]semantic.Config{"broken": {Mode: "always"}},
			wantConfig: map[string]string{"hufu.yaml": "pass", "team broken": "fail"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := doctorReport{Checks: []doctorCheck{}}
			collectDoctorSemantic(t.Context(), &report, &config.Config{SemanticRetrieval: tc.global}, tc.teams)
			configs, encoders := map[string]string{}, []doctorCheck{}
			legacy := false
			for _, check := range report.Checks {
				switch check.ID {
				case "semantic.config":
					configs[check.Subject] = check.Status
				case "semantic.encoder":
					encoders = append(encoders, check)
				case "semantic.legacy-embedding-model":
					legacy = strings.Contains(check.Message, "does not affect semantic-retrieval")
				}
			}
			if len(configs) != len(tc.wantConfig) {
				t.Fatalf("config checks = %v, want %v", configs, tc.wantConfig)
			}
			for subject, status := range tc.wantConfig {
				if configs[subject] != status {
					t.Fatalf("config checks = %v, want %v", configs, tc.wantConfig)
				}
			}
			if !legacy {
				t.Fatalf("legacy embedding-model check missing: %+v", report.Checks)
			}
			if tc.wantEncoder == "" {
				if len(encoders) != 0 {
					t.Fatalf("unexpected encoder checks: %+v", encoders)
				}
				return
			}
			if len(encoders) != 1 || encoders[0].Status != tc.wantEncoder || encoders[0].ReasonCode != tc.wantReason || !strings.Contains(encoders[0].Message, tc.wantMessage) {
				t.Fatalf("encoder checks = %+v", encoders)
			}
		})
	}
}
