package yamlutil

import (
	"testing"
)

func TestFlattenYAML(t *testing.T) {
	tests := []struct {
		name     string
		input    map[string]interface{}
		prefix   string
		want     map[string]string
		wantErr  bool
		errMsg   string
	}{
		{
			name:  "empty map",
			input: map[string]interface{}{},
			want:  map[string]string{},
		},
		{
			name: "flat map with scalars",
			input: map[string]interface{}{
				"key1": "value1",
				"key2": 42,
				"key3": true,
			},
			want: map[string]string{
				"key1": "value1",
				"key2": "42",
				"key3": "true",
			},
		},
		{
			name: "nested map",
			input: map[string]interface{}{
				"level1": map[string]interface{}{
					"level2": map[string]interface{}{
						"key": "deep",
					},
				},
			},
			want: map[string]string{
				"level1.level2.key": "deep",
			},
		},
		{
			name: "mixed nested and flat",
			input: map[string]interface{}{
				"flat":   "value",
				"nested": map[string]interface{}{
					"key": "nested_value",
				},
			},
			want: map[string]string{
				"flat":           "value",
				"nested.key":     "nested_value",
			},
		},
		{
			name: "with prefix",
			input: map[string]interface{}{
				"key": "value",
			},
			prefix: "prefix",
			want: map[string]string{
				"prefix.key": "value",
			},
		},
		{
			name: "list returns error",
			input: map[string]interface{}{
				"items": []interface{}{"a", "b", "c"},
			},
			wantErr: true,
			errMsg:  "is a list",
		},
		{
			name: "nested list returns error",
			input: map[string]interface{}{
				"outer": map[string]interface{}{
					"inner": []interface{}{1, 2, 3},
				},
			},
			wantErr: true,
			errMsg:  "is a list",
		},
		{
			name: "map with interface keys",
			input: map[string]interface{}{
				"parent": map[interface{}]interface{}{
					"child": "value",
				},
			},
			want: map[string]string{
				"parent.child": "value",
			},
		},
		{
			name: "multiple keys at same level",
			input: map[string]interface{}{
				"a": "1",
				"b": "2",
				"c": "3",
			},
			want: map[string]string{
				"a": "1",
				"b": "2",
				"c": "3",
			},
		},
		{
			name: "float values",
			input: map[string]interface{}{
				"pi":    3.14159,
				"zero":  0.0,
				"neg":   -1.5,
			},
			want: map[string]string{
				"pi":   "3.14159",
				"zero": "0",
				"neg":  "-1.5",
			},
		},
		{
			name: "nil value",
			input: map[string]interface{}{
				"key": nil,
			},
			want: map[string]string{
				"key": "<nil>",
			},
		},
		{
			name: "deeply nested",
			input: map[string]interface{}{
				"a": map[string]interface{}{
					"b": map[string]interface{}{
						"c": map[string]interface{}{
							"d": "deep",
						},
					},
				},
			},
			want: map[string]string{
				"a.b.c.d": "deep",
			},
		},
		{
			name: "empty string value",
			input: map[string]interface{}{
				"empty": "",
			},
			want: map[string]string{
				"empty": "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := make(map[string]string)
			err := FlattenYAML(tt.input, tt.prefix, result)

			if tt.wantErr {
				if err == nil {
					t.Errorf("FlattenYAML() expected error, got nil")
					return
				}
				if tt.errMsg != "" && !contains(err.Error(), tt.errMsg) {
					t.Errorf("FlattenYAML() error = %v, want contains %q", err, tt.errMsg)
				}
				return
			}

			if err != nil {
				t.Errorf("FlattenYAML() unexpected error = %v", err)
				return
			}

			if len(result) != len(tt.want) {
				t.Errorf("FlattenYAML() result length = %d, want %d", len(result), len(tt.want))
			}

			for k, v := range tt.want {
				if result[k] != v {
					t.Errorf("FlattenYAML()[%q] = %q, want %q", k, result[k], v)
				}
			}
		})
	}
}

func TestFlattenYAMLBytes(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		path    string
		want    map[string]string
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid flat yaml",
			data: []byte(`
key1: value1
key2: 42
key3: true
`),
			want: map[string]string{
				"key1": "value1",
				"key2": "42",
				"key3": "true",
			},
		},
		{
			name: "valid nested yaml",
			data: []byte(`
database:
  host: localhost
  port: 5432
  credentials:
    user: admin
    pass: secret
`),
			want: map[string]string{
				"database.host":           "localhost",
				"database.port":           "5432",
				"database.credentials.user": "admin",
				"database.credentials.pass": "secret",
			},
		},
		{
			name: "invalid yaml",
			data: []byte(`
key: value
  bad indent: here
`),
			wantErr: true,
			errMsg:  "failed to parse YAML",
		},
		{
			name: "yaml with list",
			data: []byte(`
items:
  - a
  - b
  - c
`),
			wantErr: true,
			errMsg:  "is a list",
		},
		{
			name: "empty yaml",
			data: []byte(``),
			want: map[string]string{},
		},
		{
			name: "yaml with null",
			data: []byte(`
key: null
`),
			want: map[string]string{
				"key": "<nil>",
			},
		},
		{
			name: "yaml with special characters in values",
			data: []byte(`
url: https://example.com/path?query=1
message: "Hello, World!"
`),
			want: map[string]string{
				"url":     "https://example.com/path?query=1",
				"message": "Hello, World!",
			},
		},
		{
			name: "yaml with numeric types",
			data: []byte(`
int: 42
float: 3.14
negative: -100
`),
			want: map[string]string{
				"int":      "42",
				"float":    "3.14",
				"negative": "-100",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := FlattenYAMLBytes(tt.data, tt.path)

			if tt.wantErr {
				if err == nil {
					t.Errorf("FlattenYAMLBytes() expected error, got nil")
					return
				}
				if tt.errMsg != "" && !contains(err.Error(), tt.errMsg) {
					t.Errorf("FlattenYAMLBytes() error = %v, want contains %q", err, tt.errMsg)
				}
				return
			}

			if err != nil {
				t.Errorf("FlattenYAMLBytes() unexpected error = %v", err)
				return
			}

			if len(result) != len(tt.want) {
				t.Errorf("FlattenYAMLBytes() result length = %d, want %d", len(result), len(tt.want))
			}

			for k, v := range tt.want {
				if result[k] != v {
					t.Errorf("FlattenYAMLBytes()[%q] = %q, want %q", k, result[k], v)
				}
			}
		})
	}
}

func TestFlattenYAMLBytesWithPath(t *testing.T) {
	data := []byte(`invalid: [`)
	_, err := FlattenYAMLBytes(data, "/test/path/config.yaml")
	if err == nil {
		t.Errorf("FlattenYAMLBytes() expected error for invalid YAML")
	}
	if !contains(err.Error(), "/test/path/config.yaml") {
		t.Errorf("FlattenYAMLBytes() error should contain path, got %v", err)
	}
}

// Helper function to check if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && findSubstring(s, substr))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
