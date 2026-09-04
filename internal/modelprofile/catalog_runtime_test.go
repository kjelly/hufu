package modelprofile

import (
	"testing"

	"github.com/kjelly/hufu/internal/modelcatalog"
	"github.com/kjelly/hufu/internal/providerintrospection"
)

func TestRuntimeResolverUsesExactCatalogEvidenceBeforeFallback(t *testing.T) {
	falseValue := false
	catalog, err := modelcatalog.NewCatalog("test", []modelcatalog.CatalogModel{{
		Provider: "ollama", ID: "qwen3:8b", Family: "qwen3", Context: 131_072, Output: 2_048,
		ToolCall: &falseValue,
	}})
	if err != nil {
		t.Fatal(err)
	}
	resolver := NewRuntimeResolverWithCatalog(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
		return &splitRuntimeIntrospector{process: providerintrospection.RuntimeModelInfo{RuntimeContext: 32_768}}
	}, ProfileCacheOptions{}, catalog)
	request := testRuntimeRequest(testRuntimeProvider("catalog-secret"))
	profile, err := resolver.Resolve(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if profile.ModelMaxContext != 131_072 || profile.Sources.ModelMaxContext.Source != SourceCatalog {
		t.Fatalf("catalog model max context = %#v", profile.Sources.ModelMaxContext)
	}
	if profile.Family != "qwen3" || profile.Estimator != "qwen" || profile.EstimatorProvenance != "catalog_family_derived" {
		t.Fatalf("catalog family estimator = %#v", profile)
	}
	if profile.EffectiveContext != 32_768 || profile.Sources.EffectiveContext.Source != SourceProviderRuntime {
		t.Fatalf("runtime did not override catalog context = %#v", profile.Sources.EffectiveContext)
	}
	if profile.MaxOutputTokens != 2_048 || profile.Sources.MaxOutputTokens.Source != SourceCatalog {
		t.Fatalf("catalog output evidence = %#v", profile.Sources.MaxOutputTokens)
	}
	if profile.SupportsTools != CapabilityNo || profile.Sources.Capabilities.Tools.Source != SourceCatalog {
		t.Fatalf("explicit catalog false was not preserved = %#v", profile.Sources.Capabilities.Tools)
	}

	request.ModelID = "ollama/qwen3:72b"
	request.Profile.ModelID = request.ModelID
	unknown, err := resolver.Resolve(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Sources.CatalogContext.Value != 0 || unknown.EffectiveContext != 32_768 || unknown.Sources.EffectiveContext.Source != SourceProviderRuntime {
		t.Fatalf("unknown model used non-exact catalog data = %#v", unknown)
	}
}

func TestCatalogLookupIdentityCanonicalizesOnlyBoundOllamaAliases(t *testing.T) {
	tests := []struct {
		name      string
		provider  providerintrospection.ProviderRef
		modelID   string
		wantFound bool
		wantID    string
	}{
		{name: "bare", provider: testRuntimeProvider("secret"), modelID: "qwen3:8b", wantFound: true, wantID: "qwen3:8b"},
		{name: "local qualifier", provider: testRuntimeProvider("secret"), modelID: "local/qwen3:8b", wantFound: true, wantID: "qwen3:8b"},
		{name: "ollama qualifier", provider: testRuntimeProvider("secret"), modelID: "ollama/qwen3:8b", wantFound: true, wantID: "qwen3:8b"},
		{
			name:      "configured Ollama gateway qualifier",
			provider:  providerintrospection.NewProviderRef("ollama-gateway", "ollama-gateway", "ollama", "http://gateway.example/v1", "", false),
			modelID:   "ollama-gateway/qwen3:8b",
			wantFound: true,
			wantID:    "qwen3:8b",
		},
		{name: "foreign namespace", provider: testRuntimeProvider("secret"), modelID: "other/qwen3:8b", wantFound: false, wantID: "other/qwen3:8b"},
	}
	catalog, err := modelcatalog.NewCatalog("test", []modelcatalog.CatalogModel{{Provider: "ollama", ID: "qwen3:8b", Context: 131_072}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			catalogProvider, catalogModelID := catalogLookupIdentity(test.provider, test.modelID)
			if catalogModelID != test.wantID {
				t.Fatalf("catalog model ID = %q, want %q", catalogModelID, test.wantID)
			}
			_, found := catalog.Lookup(catalogProvider, catalogModelID)
			if found != test.wantFound {
				t.Fatalf("catalog lookup %s/%s found=%t, want %t", catalogProvider, catalogModelID, found, test.wantFound)
			}
			input := ModelProfileInput{ModelID: test.modelID, Provider: test.provider.Provider}
			applyCatalogEvidence(catalog, &input, test.provider, test.modelID)
			if test.wantFound && input.Context.CatalogContext != 131_072 {
				t.Fatalf("catalog evidence context = %d, want 131072", input.Context.CatalogContext)
			}
			if !test.wantFound && input.Context.CatalogContext != 0 {
				t.Fatalf("foreign namespace received catalog evidence: %d", input.Context.CatalogContext)
			}
		})
	}
}
