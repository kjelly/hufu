package semantic

import (
	"strings"
	"testing"
	"time"
)

func boolPtr(v bool) *bool { return &v }

func TestResolveDefaults(t *testing.T) {
	s, err := Config{}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{
		Mode: ModeOff, Fusion: FusionScoreNormalized, Model: "embeddinggemma:latest", Endpoint: DefaultEndpoint,
		MaxInputTokens: 2048, QueryMaxTokens: 512, QueryTimeout: 3 * time.Second, LoadTimeout: 30 * time.Second,
		IndexTimeout: 5 * time.Minute, KeepAlive: 10 * time.Minute, MaxConcurrent: 1, IndexBatchSize: 8,
	}
	if s != want {
		t.Fatalf("Resolve() = %+v\nwant %+v", s, want)
	}
	if s.Enabled() {
		t.Fatal("default mode must not be enabled")
	}
}

func TestResolveValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
		check   func(*testing.T, Settings)
	}{
		{name: "qwen preset", cfg: Config{Mode: ModeShadow, Model: "qwen3-embedding:0.6b"}, check: func(t *testing.T, s Settings) {
			if s.MaxInputTokens != 4096 || s.QueryMaxTokens != 128 || !s.Enabled() {
				t.Fatalf("settings = %+v", s)
			}
		}},
		{name: "unknown mode", cfg: Config{Mode: "always"}, wantErr: "mode must be"},
		{name: "auto mode", cfg: Config{Mode: ModeAuto}, check: func(t *testing.T, s Settings) {
			if !s.Enabled() || !s.ChangesSelection() {
				t.Fatalf("auto settings = %+v", s)
			}
		}},
		{name: "shadow keeps selection", cfg: Config{Mode: ModeShadow}, check: func(t *testing.T, s Settings) {
			if !s.Enabled() || s.ChangesSelection() {
				t.Fatalf("shadow settings = %+v", s)
			}
		}},
		{name: "explicit fusion", cfg: Config{Fusion: FusionRRFNormalized}, check: func(t *testing.T, s Settings) {
			if s.Fusion != FusionRRFNormalized {
				t.Fatalf("fusion = %q", s.Fusion)
			}
		}},
		{name: "unknown fusion", cfg: Config{Fusion: "bm25"}, wantErr: "fusion must be"},
		{name: "unsupported model", cfg: Config{Model: "nomic-embed-text"}, wantErr: "not supported"},
		{name: "namespaced model", cfg: Config{Model: "someone/embeddinggemma"}, wantErr: "ollama model name"},
		{name: "remote without allow", cfg: Config{Endpoint: "http://192.168.11.117:11434"}, wantErr: "not loopback"},
		{name: "remote allowed", cfg: Config{Endpoint: "http://192.168.11.117:11434", AllowRemoteEndpoint: boolPtr(true)}, check: func(t *testing.T, s Settings) {
			if s.EndpointIsLoopback() {
				t.Fatal("remote endpoint reported as loopback")
			}
		}},
		{name: "localhost", cfg: Config{Endpoint: "http://localhost:11434"}},
		{name: "ipv6 loopback", cfg: Config{Endpoint: "http://[::1]:11434"}},
		{name: "bad scheme", cfg: Config{Endpoint: "ftp://127.0.0.1"}, wantErr: "http or https"},
		{name: "negative timeout", cfg: Config{QueryTimeout: -time.Second}, wantErr: "query-timeout"},
		{name: "query over input", cfg: Config{MaxInputTokens: 256, QueryMaxTokens: 512}, wantErr: "exceeds max-input-tokens"},
		{name: "query under prefix", cfg: Config{QueryMaxTokens: 10}, wantErr: "no room"},
		{name: "too many concurrent", cfg: Config{MaxConcurrent: 32}, wantErr: "max-concurrent"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := tc.cfg.Resolve()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.check != nil {
				tc.check(t, s)
			}
		})
	}
}

func TestMergeOverridesFieldByField(t *testing.T) {
	base := Config{Mode: ModeShadow, Endpoint: "http://192.168.11.117:11434", AllowRemoteEndpoint: boolPtr(true), QueryTimeout: 2 * time.Second}
	override := Config{Mode: ModeActive, Fusion: FusionLegacy, QueryMaxTokens: 256}
	merged := Merge(base, override)
	if merged.Mode != ModeActive || merged.Fusion != FusionLegacy || merged.Endpoint != base.Endpoint || merged.QueryTimeout != 2*time.Second ||
		merged.QueryMaxTokens != 256 || merged.AllowRemoteEndpoint == nil || !*merged.AllowRemoteEndpoint {
		t.Fatalf("Merge() = %+v", merged)
	}
	*merged.AllowRemoteEndpoint = false
	if !*base.AllowRemoteEndpoint {
		t.Fatal("Merge shares the AllowRemoteEndpoint pointer with base")
	}
	if got := Merge(base, Config{AllowRemoteEndpoint: boolPtr(false)}); *got.AllowRemoteEndpoint {
		t.Fatal("explicit false override ignored")
	}
}

func TestHashPinsEffectiveSettings(t *testing.T) {
	a, _ := Config{Mode: ModeShadow}.Resolve()
	b, _ := Config{Mode: ModeActive}.Resolve()
	c, _ := Config{Mode: ModeShadow, Endpoint: "http://user:pw@127.0.0.1:11434"}.Resolve()
	d, _ := Config{Mode: ModeShadow, Endpoint: "http://127.0.0.1:11434"}.Resolve()
	e, _ := Config{Mode: ModeActive, Fusion: FusionLegacy}.Resolve()
	if a.Hash() == b.Hash() {
		t.Fatal("mode change did not change hash")
	}
	if b.Hash() == e.Hash() {
		t.Fatal("fusion change did not change hash")
	}
	if c.Hash() != d.Hash() {
		t.Fatal("hash depends on endpoint credentials")
	}
}

func TestRoutesSemantic(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{name: "english goal", text: "Review internal/team/worker_memory.go for scope leaks", want: false},
		{name: "chinese goal", text: "整理台灣半導體產業的最新研究資料", want: true},
		{name: "chinese with identifiers", text: "比較 qwen3-embedding 與 embeddinggemma 在 CPU 上的效能", want: true},
		{name: "english with one cjk word", text: "Summarize the release notes for the 2026 roadmap and list open risks 風險", want: false},
		{name: "latin with few accents", text: "Die Größe der Datei prüfen und Fehler melden", want: false},
		{name: "latin with many diacritics", text: "Tổng hợp các nghiên cứu mới nhất về trí tuệ nhân tạo", want: true},
		{name: "cyrillic", text: "Сравнить модели по скорости и качеству", want: true},
		{name: "empty", text: "", want: false},
		{name: "punctuation only", text: "？！。…", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RoutesSemantic(tc.text); got != tc.want {
				t.Fatalf("RoutesSemantic(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}
