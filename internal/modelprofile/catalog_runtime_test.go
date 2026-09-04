package modelprofile

import (
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/modelcatalog"
	"github.com/kjelly/hufu/internal/providerintrospection"
)

func TestDiagnosticCatalogIdentityUsesBoundProviderAndNoRuntime(t *testing.T) {
	catalog, err := modelcatalog.NewCatalog("test", []modelcatalog.CatalogModel{
		{Provider: "openai", ID: "gpt-4o", Family: "gpt", Context: 128_000},
		{Provider: "ollama", ID: "qwen3:8b", Family: "qwen3", Context: 131_072},
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name         string
		requested    string
		provider     providerintrospection.ProviderRef
		modelID      string
		wantProvider string
		wantModelID  string
		wantContext  int
	}{
		{
			name:         "unconfigured explicit openai remains openai",
			requested:    "openai",
			provider:     providerintrospection.NewProviderRef("local", "local", "ollama", "http://127.0.0.1:11434/v1", "", true),
			modelID:      "gpt-4o",
			wantProvider: "openai",
			wantModelID:  "gpt-4o",
			wantContext:  128_000,
		},
		{
			name:         "local ollama",
			requested:    "local",
			provider:     providerintrospection.NewProviderRef("local", "local", "ollama", "http://127.0.0.1:11434/v1", "", true),
			modelID:      "qwen3:8b",
			wantProvider: "ollama",
			wantModelID:  "qwen3:8b",
			wantContext:  131_072,
		},
		{
			name:         "configured ollama gateway",
			requested:    "ollama-gateway",
			provider:     providerintrospection.NewProviderRef("ollama-gateway", "ollama-gateway", "ollama", "http://gateway.example/v1", "", true),
			modelID:      "qwen3:8b",
			wantProvider: "ollama",
			wantModelID:  "qwen3:8b",
			wantContext:  131_072,
		},
	}
	var introspectionCalls atomic.Int32
	resolver := NewRuntimeResolverWithCatalog(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
		introspectionCalls.Add(1)
		return &splitRuntimeIntrospector{}
	}, ProfileCacheOptions{}, catalog)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			catalogProvider, catalogModelID := ResolveDiagnosticCatalogIdentity(test.requested, test.provider, test.modelID)
			if catalogProvider != test.wantProvider || catalogModelID != test.wantModelID {
				t.Fatalf("catalog identity = %s/%s, want %s/%s", catalogProvider, catalogModelID, test.wantProvider, test.wantModelID)
			}
			request := RuntimeResolutionRequest{
				Provider:        test.provider,
				ModelID:         test.modelID,
				Profile:         ModelProfileInput{ModelID: test.modelID, Provider: test.requested},
				NoRuntime:       true,
				CatalogProvider: test.requested,
			}
			profile, err := resolver.Resolve(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if profile.Provider != test.wantProvider || profile.ModelID != test.modelID {
				t.Fatalf("diagnostic identity = %s/%s, want %s/%s", profile.Provider, profile.ModelID, test.wantProvider, test.modelID)
			}
			if profile.Sources.CatalogContext.Value != test.wantContext || profile.Sources.CatalogContext.Source != SourceCatalog {
				t.Fatalf("catalog evidence = %#v, want %d/catalog", profile.Sources.CatalogContext, test.wantContext)
			}
		})
	}
	if got := introspectionCalls.Load(); got != 0 {
		t.Fatalf("no-runtime diagnostic invoked %d introspectors", got)
	}
}

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

func TestDiagnosticCatalogNamespaceDoesNotInheritFallbackAdapter(t *testing.T) {
	catalog, err := modelcatalog.NewCatalog("test", []modelcatalog.CatalogModel{{Provider: "openai", ID: "gpt-4o", Context: 131_072}})
	if err != nil {
		t.Fatal(err)
	}
	input := ModelProfileInput{ModelID: "gpt-4o", Provider: "openai"}
	fallback := providerintrospection.NewProviderRef("local", "local", "ollama", "http://127.0.0.1:11434/v1", "", true)
	applyCatalogEvidence(catalog, &input, fallback, "gpt-4o", "openai")
	if input.Context.CatalogContext != 131_072 {
		t.Fatalf("diagnostic catalog context = %d, want 131072", input.Context.CatalogContext)
	}
}
