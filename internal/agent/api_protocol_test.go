package agent

import (
	"errors"
	"testing"

	"github.com/kjelly/hufu/internal/config"
)

func TestResolveAPIProtocol(t *testing.T) {
	for _, tc := range []struct {
		raw, effective, source string
		invalid                bool
	}{
		{"", config.APIProtocolChatCompletions, "legacy", false},
		{" \n", config.APIProtocolChatCompletions, "legacy", false},
		{" CHAT-COMPLETIONS ", config.APIProtocolChatCompletions, "explicit", false},
		{" ReSpOnSeS ", config.APIProtocolResponses, "explicit", false},
		{"auto", "", "", true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := ResolveAPIProtocol(tc.raw)
			if (err != nil) != tc.invalid {
				t.Fatalf("resolution=%+v err=%v", got, err)
			}
			if tc.invalid {
				return
			}
			requested, _ := config.NormalizeAPIProtocol(tc.raw)
			if got.Requested != requested || got.Effective != tc.effective || got.Source != tc.source || got.Version != 1 {
				t.Fatalf("resolution=%+v", got)
			}
		})
	}
}

func TestProviderManagerAPIProtocol(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		providers            map[string]config.ProviderConfig
		want                 string
		unavailable, invalid bool
	}{
		{name: "legacy", providers: map[string]config.ProviderConfig{"remote": {}}, want: ""},
		{name: "normalized explicit", providers: map[string]config.ProviderConfig{"remote": {APIProtocol: " CHAT-COMPLETIONS "}}, want: config.APIProtocolChatCompletions},
		{name: "responses gated", providers: map[string]config.ProviderConfig{"remote": {APIProtocol: " RESPONSES "}}, unavailable: true},
		{name: "auto rejected", providers: map[string]config.ProviderConfig{"remote": {APIProtocol: "auto"}}, invalid: true},
		{name: "equivalent aliases", providers: map[string]config.ProviderConfig{"local": {APIProtocol: " CHAT-COMPLETIONS "}, "LOCAL": {APIProtocol: "chat-completions"}}, want: config.APIProtocolChatCompletions},
		{name: "legacy explicit aliases conflict", providers: map[string]config.ProviderConfig{"local": {}, "LOCAL": {APIProtocol: "chat-completions"}}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pm, err := NewProviderManager("https://fixture.invalid/v1", "", tc.providers)
			if tc.unavailable {
				if !errors.Is(err, ErrResponsesNotEnabled) {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if tc.invalid {
				if err == nil {
					t.Fatal("accepted invalid configuration")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			name := "remote"
			if _, ok := tc.providers["local"]; ok {
				name = "local"
			}
			if got := pm.configs[name].APIProtocol; got != tc.want {
				t.Fatalf("protocol=%q want=%q", got, tc.want)
			}
		})
	}
}
