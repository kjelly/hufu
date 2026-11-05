package config

import (
	json "encoding/json/v2"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestNormalizeAPIProtocol(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		invalid   bool
	}{
		{"", "", false}, {" \n\t", "", false},
		{" CHAT-COMPLETIONS ", APIProtocolChatCompletions, false},
		{" ReSpOnSeS ", APIProtocolResponses, false},
		{"auto", "", true}, {"chat", "", true}, {"response", "", true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := NormalizeAPIProtocol(tc.raw)
			if got != tc.want || (err != nil) != tc.invalid {
				t.Fatalf("got=%q err=%v", got, err)
			}
			if err := (ProviderConfig{APIProtocol: tc.raw}).Validate(); (err != nil) != tc.invalid {
				t.Fatalf("Validate=%v", err)
			}
		})
	}
}

func TestAPIProtocolConfigurationRoundTrip(t *testing.T) {
	for _, protocol := range []string{"", APIProtocolChatCompletions, APIProtocolResponses} {
		t.Run(protocol, func(t *testing.T) {
			original := ProviderConfig{ProviderURL: "https://fixture.invalid/v1", APIProtocol: protocol}
			data, err := yaml.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			var got ProviderConfig
			if err = yaml.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got != original {
				t.Fatalf("got=%+v want=%+v", got, original)
			}
			data, err = json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "api_protocol") != (protocol != "") {
				t.Fatalf("legacy JSON shape=%s", data)
			}
		})
	}
}

func TestAPIProtocolProviderFieldOverrides(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"", APIProtocolResponses}, {" ", APIProtocolResponses},
		{APIProtocolChatCompletions, APIProtocolChatCompletions}, {" ReSpOnSeS ", " ReSpOnSeS "},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			original := ProviderConfig{ProviderURL: "https://fixture.invalid/v1", APIProtocol: APIProtocolResponses}
			global := map[string]ProviderConfig{"remote": original}
			team := map[string]ProviderConfig{"remote": {APIProtocol: tc.raw}}
			got := MergeProviderConfigs(global, team)["remote"]
			if got.APIProtocol != tc.want || got.ProviderURL != original.ProviderURL {
				t.Fatalf("override=%+v", got)
			}
			loaded := &Config{Providers: map[string]ProviderConfig{"remote": original}}
			loaded.mergeProviders(team)
			if loaded.Providers["remote"] != got {
				t.Fatalf("file merge=%+v helper merge=%+v", loaded.Providers["remote"], got)
			}
			if global["remote"] != original {
				t.Fatal("mutated global configuration")
			}
		})
	}
}

func TestBackendAPIProtocolPresence(t *testing.T) {
	for _, tc := range []struct {
		yaml    string
		present bool
	}{
		{"kind: agent\n", false},
		{"kind: agent\napi-protocol: ''\n", true},
		{"kind: llm\napi-protocol: responses\n", true},
	} {
		t.Run(tc.yaml, func(t *testing.T) {
			var got BackendConfig
			if err := yaml.Unmarshal([]byte(tc.yaml), &got); err != nil {
				t.Fatal(err)
			}
			if got.Has("api-protocol") != tc.present {
				t.Fatalf("presence=%v", got.Has("api-protocol"))
			}
		})
	}
	if !(BackendConfig{APIProtocol: APIProtocolResponses}).Has("api-protocol") {
		t.Fatal("programmatic field not present")
	}
}
