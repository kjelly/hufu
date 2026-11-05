package providerintrospection

import "testing"

func TestImageInputEvidenceRequiresExplicitModality(t *testing.T) {
	for _, test := range []struct {
		name   string
		names  []string
		fields map[string]any
		want   CapabilityState
	}{
		{name: "absent"},
		{name: "generic attachments", names: []string{"attachments", "multimodal", "multimodal_input"}},
		{name: "attachment fields", fields: map[string]any{"supports_attachments": true, "multimodal": true}},
		{name: "vision list", names: []string{"vision"}, want: CapabilityYes},
		{name: "image alias", names: []string{"SUPPORTS-IMAGE-INPUT"}, want: CapabilityYes},
		{name: "negative list", names: []string{"vision", "no-vision"}, want: CapabilityNo},
		{name: "positive field", fields: map[string]any{"supports_vision": true}, want: CapabilityYes},
		{name: "negative field", names: []string{"vision"}, fields: map[string]any{"supports_vision": false}, want: CapabilityNo},
		{name: "unknown field", names: []string{"vision"}, fields: map[string]any{"image_input": "unknown"}, want: CapabilityUnknown},
		{name: "malformed field", names: []string{"vision"}, fields: map[string]any{"supports_vision": 1}, want: CapabilityUnknown},
		{name: "conflicting aliases", fields: map[string]any{"vision": true, "supports_image_input": false}, want: CapabilityNo},
		{name: "negative claim cannot enable", names: []string{"no-vision"}, fields: map[string]any{"supports_vision": true}, want: CapabilityNo},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ImageInputEvidence(test.names, test.fields); got != test.want {
				t.Fatalf("image evidence=%q want %q", got, test.want)
			}
		})
	}
}

func TestPublicParsersPreserveImageEvidenceSeparately(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  map[string]any
		want CapabilityState
	}{
		{"vision", map[string]any{"capabilities": []any{"vision"}}, CapabilityYes},
		{"generic attachment", map[string]any{"capabilities": []any{"attachments"}}, ""},
		{"explicit unknown", map[string]any{"capabilities": []any{"vision"}, "supports_vision": "unknown"}, CapabilityUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := cloneMap(test.raw)
			raw["model"], raw["id"] = "fixture", "fixture"
			show, err := ParseOllamaShow(raw)
			if err != nil {
				t.Fatal(err)
			}
			process, found, err := ParseOllamaPS(map[string]any{"models": []any{raw}}, "fixture")
			if err != nil || !found {
				t.Fatalf("process fixture: %v found=%v", err, found)
			}
			openAI, err := ParseOpenAIModels(map[string]any{"data": []any{raw}}, "fixture")
			if err != nil {
				t.Fatal(err)
			}
			for name, info := range map[string]RuntimeModelInfo{"show": show, "process": process, "openai": openAI} {
				if info.ImageInput != test.want {
					t.Fatalf("%s image evidence=%q want %q", name, info.ImageInput, test.want)
				}
				if info.CapabilityEvidence["attachments"] == "" {
					t.Fatalf("%s lost legacy attachment evidence", name)
				}
			}
		})
	}
}

func TestRuntimeImageEvidenceOverridesShow(t *testing.T) {
	for _, test := range []struct {
		name    string
		runtime RuntimeModelInfo
		want    CapabilityState
	}{
		{"absent", RuntimeModelInfo{Capabilities: []string{"attachments"}}, CapabilityYes},
		{"explicit no", RuntimeModelInfo{ImageInput: CapabilityNo}, CapabilityNo},
		{"explicit unknown", RuntimeModelInfo{ImageInput: CapabilityUnknown}, CapabilityUnknown},
		{"raw negative", RuntimeModelInfo{Capabilities: []string{"no-vision"}}, CapabilityNo},
	} {
		t.Run(test.name, func(t *testing.T) {
			info := RuntimeModelInfo{ImageInput: CapabilityYes}
			mergeRuntimeCapabilities(&info, test.runtime)
			if info.ImageInput != test.want {
				t.Fatalf("merged image evidence=%q want %q", info.ImageInput, test.want)
			}
		})
	}
}
