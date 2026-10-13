package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/embedding"
	"github.com/kjelly/hufu/internal/semantic"
)

// doctorSemanticProbeTimeout bounds each encoder check; doctor reads model
// metadata only and never loads or pulls a model.
const doctorSemanticProbeTimeout = 5 * time.Second

// doctorSemanticEncoder is one distinct enabled encoder and the settings
// layers that use it.
type doctorSemanticEncoder struct {
	settings semantic.Settings
	users    []string
}

// collectDoctorSemantic checks the semantic-retrieval settings of hufu.yaml
// and of every team that overrides them, separates them from the legacy
// embedding-model, and, for each enabled encoder, that Ollama serves the
// model with a context long enough for the input limit. Encoder problems
// are warnings: retrieval then falls back to lexical.
func collectDoctorSemantic(ctx context.Context, report *doctorReport, cfg *config.Config, teamBlocks map[string]semantic.Config) {
	encoders := map[string]*doctorSemanticEncoder{}
	check := func(subject string, block semantic.Config) {
		settings, err := block.Resolve()
		if err != nil {
			report.add("semantic.config", subject, "fail", "semantic-retrieval is invalid: "+err.Error())
			return
		}
		if !settings.Enabled() {
			report.add("semantic.config", subject, "pass", "semantic retrieval is off")
			return
		}
		report.add("semantic.config", subject, "pass", fmt.Sprintf("mode %s, model %s, fusion %s, endpoint %s",
			settings.Mode, settings.Model, settings.Fusion, embedding.RedactEndpoint(settings.Endpoint)))
		key := fmt.Sprintf("%s\x00%s\x00%d\x00%d", settings.Endpoint, settings.Model, settings.MaxInputTokens, settings.QueryMaxTokens)
		if encoders[key] == nil {
			encoders[key] = &doctorSemanticEncoder{settings: settings}
		}
		encoders[key].users = append(encoders[key].users, subject)
	}
	check("hufu.yaml", cfg.SemanticRetrieval)
	for _, name := range slices.Sorted(maps.Keys(teamBlocks)) {
		check("team "+name, semantic.Merge(cfg.SemanticRetrieval, teamBlocks[name]))
	}
	if legacy := strings.TrimSpace(cfg.EmbeddingModel); legacy != "" {
		report.Checks = append(report.Checks, doctorCheck{
			ID: "semantic.legacy-embedding-model", Status: "warning", ReasonCode: "legacy_embedding_model_set",
			Message: fmt.Sprintf("embedding-model %s is deprecated and has no effect; remove it from hufu.yaml (semantic memory retrieval uses semantic-retrieval)", legacy),
		})
	}
	for _, key := range slices.Sorted(maps.Keys(encoders)) {
		doctorCheckSemanticEncoder(ctx, report, encoders[key])
	}
}

func doctorCheckSemanticEncoder(ctx context.Context, report *doctorReport, encoder *doctorSemanticEncoder) {
	settings := encoder.settings
	subject := settings.Model + " @ " + embedding.RedactEndpoint(settings.Endpoint)
	users := "used by " + strings.Join(encoder.users, ", ")
	client, err := embedding.NewOllamaClient(settings.Endpoint, nil)
	if err == nil {
		probeCtx, cancel := context.WithTimeout(ctx, doctorSemanticProbeTimeout)
		var profile embedding.EncoderProfile
		profile, _, err = embedding.ResolveProfile(probeCtx, client, embedding.ProfileSettings{
			Model: settings.Model, MaxInputTokens: settings.MaxInputTokens, QueryMaxTokens: settings.QueryMaxTokens,
		})
		cancel()
		if err == nil {
			report.add("semantic.encoder", subject, "pass", fmt.Sprintf("Ollama serves the model (digest %s, %d dimensions, input limit %d tokens); %s",
				shortDigest(profile.ArtifactDigest), profile.Dimensions, profile.MaxInputTokens, users))
			return
		}
	}
	check := doctorCheck{ID: "semantic.encoder", Subject: subject, Status: "warning"}
	switch {
	case errors.Is(err, embedding.ErrOllamaUnavailable):
		check.ReasonCode = "semantic_endpoint_unreachable"
		check.Message = "Ollama is unreachable; semantic retrieval falls back to lexical; " + users
	case errors.Is(err, embedding.ErrModelNotFound):
		check.ReasonCode = "semantic_model_missing"
		check.Message = fmt.Sprintf("model is not pulled; run `ollama pull %s` (hufu never pulls); %s", settings.Model, users)
	default:
		check.ReasonCode = "semantic_profile_invalid"
		check.Message = fmt.Sprintf("encoder profile check failed: %v; %s", err, users)
	}
	report.Checks = append(report.Checks, check)
}

func shortDigest(digest string) string {
	digest = strings.TrimPrefix(digest, "sha256:")
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}
