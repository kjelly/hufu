package agent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOllamaProviderModelExistsConfirmsUnlistedModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		switch request.URL.Path {
		case "/v1/models/glm-5.3-flash:cloud":
			_, _ = writer.Write([]byte(`{"id":"glm-5.3-flash","object":"model"}`))
		case "/v1/models/broken:cloud":
			writer.WriteHeader(http.StatusInternalServerError)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	provider, err := NewOllamaProvider(server.URL+"/v1", "test-key", "ollama")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		model   string
		exists  bool
		wantErr bool
	}{
		{model: "glm-5.3-flash:cloud", exists: true},
		{model: "glm-5.3-flsh:cloud", exists: false},
		{model: "broken:cloud", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.model, func(t *testing.T) {
			exists, err := provider.ModelExists(t.Context(), test.model)
			if (err != nil) != test.wantErr || exists != test.exists {
				t.Fatalf("ModelExists = %v, %v; want %v, error %v", exists, err, test.exists, test.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), test.model) {
				t.Fatalf("error does not name the model: %v", err)
			}
		})
	}
}
