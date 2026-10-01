// Package backend constructs DecisionPrimitive adapters without CLI or team dependencies.
package backend

import (
	"fmt"
	"net/http"

	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/decisionrt/backend/rule"
	"github.com/kjelly/hufu/internal/decisionrt/backend/sidecar"
	"github.com/kjelly/hufu/internal/decisionrt/backend/systemone"
)

type Config struct {
	Name       string
	Endpoint   string
	Model      string
	APIKey     string
	HTTPClient *http.Client
	Generator  sidecar.Generator
}

func New(config Config) (decisionrt.Backend, error) {
	switch config.Name {
	case "rule":
		return rule.AlwaysAbstain(), nil
	case "sidecar":
		return sidecar.New(config.Generator)
	case "systemone":
		return systemone.New(systemone.Config{Endpoint: config.Endpoint, Model: config.Model, APIKey: config.APIKey, HTTPClient: config.HTTPClient})
	default:
		return nil, &decisionrt.RuntimeError{Kind: decisionrt.ErrorConfiguration, Err: fmt.Errorf("unknown decision backend")}
	}
}
