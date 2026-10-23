// Package settings resolves inference transport independently of decision contracts.
package settings

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"maps"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/kjelly/hufu/internal/decisionrt"
)

const DefaultEndpoint = "http://127.0.0.1:11434/v1/systemone"

type SystemOne struct {
	Model     string        `yaml:"model,omitempty" json:"model,omitempty"`
	Endpoint  string        `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Timeout   time.Duration `yaml:"timeout,omitempty" json:"timeout,omitzero"`
	APIKeyEnv string        `yaml:"api-key-env,omitempty" json:"api_key_env,omitempty"`
}

type Config struct {
	Backend   string            `yaml:"backend,omitempty" json:"backend,omitempty"`
	SystemOne SystemOne         `yaml:"systemone,omitempty" json:"systemone"`
	Sources   map[string]string `yaml:"-" json:"-"`
}

func (c Config) Clone() Config { c.Sources = maps.Clone(c.Sources); return c }

// Merge preserves every independently configured field, including inactive
// backend settings. Resolve never applies those settings to another backend.
func Merge(base, override Config) Config {
	result := base.Clone()
	if result.Sources == nil {
		result.Sources = make(map[string]string)
	}
	fields := []struct {
		key   string
		dst   *string
		value string
	}{
		{"backend", &result.Backend, override.Backend},
		{"systemone.model", &result.SystemOne.Model, override.SystemOne.Model},
		{"systemone.endpoint", &result.SystemOne.Endpoint, override.SystemOne.Endpoint},
		{"systemone.api-key-env", &result.SystemOne.APIKeyEnv, override.SystemOne.APIKeyEnv},
	}
	for _, f := range fields {
		if f.value != "" {
			*f.dst = f.value
			result.Sources[f.key] = override.Sources[f.key]
		}
	}
	if override.SystemOne.Timeout != 0 {
		result.SystemOne.Timeout = override.SystemOne.Timeout
		result.Sources["systemone.timeout"] = override.Sources["systemone.timeout"]
	}
	return result
}

func (c Config) WithSource(source string) Config {
	c = c.Clone()
	c.Sources = map[string]string{}
	for _, key := range []string{"backend", "systemone.model", "systemone.endpoint", "systemone.timeout", "systemone.api-key-env"} {
		c.Sources[key] = source
	}
	return c
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate validates a partial configuration without requiring inherited fields.
func (c Config) Validate() error {
	if c.Backend != "" && c.Backend != "systemone" && c.Backend != "sidecar" {
		return fmt.Errorf("decision-runtime.backend must be systemone or sidecar")
	}
	if c.SystemOne.Timeout < 0 || c.SystemOne.Timeout > decisionrt.MaxAllowedTimeout() {
		return fmt.Errorf("decision-runtime.systemone.timeout is out of range")
	}
	if c.SystemOne.APIKeyEnv != "" && !envName.MatchString(c.SystemOne.APIKeyEnv) {
		return fmt.Errorf("decision-runtime.systemone.api-key-env is invalid")
	}
	if c.SystemOne.Endpoint != "" {
		u, err := url.Parse(c.SystemOne.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("decision-runtime.systemone.endpoint is invalid")
		}
	}
	return nil
}

// Transport holds only inference settings. It cannot change a question, grant,
// schema, confidence policy, call limit, or fallback policy.
type Transport struct {
	Backend   string            `json:"backend"`
	Model     string            `json:"model,omitempty"`
	Endpoint  string            `json:"endpoint,omitempty"`
	APIKeyEnv string            `json:"-"`
	Timeout   time.Duration     `json:"timeout"`
	Sources   map[string]string `json:"sources"`
}

// Resolve applies explicit entry/legacy values before defaults. defaultBackend
// is supplied by the owning entrypoint to preserve its historical behavior.
func Resolve(c Config, explicit Transport, defaultBackend string) (Transport, error) {
	if err := c.Validate(); err != nil {
		return Transport{}, err
	}
	r := explicit
	r.Sources = make(map[string]string)
	if r.Backend == "" {
		r.Backend = c.Backend
		r.Sources["backend"] = c.Sources["backend"]
	} else {
		r.Sources["backend"] = "entry"
	}
	if r.Backend == "" {
		r.Backend = defaultBackend
		r.Sources["backend"] = "default"
	}
	if r.Backend != "sidecar" && r.Backend != "systemone" && r.Backend != "rule" {
		return Transport{}, fmt.Errorf("invalid decision backend")
	}
	fields := []struct {
		key   string
		dst   *string
		value string
	}{
		{"model", &r.Model, c.SystemOne.Model}, {"endpoint", &r.Endpoint, c.SystemOne.Endpoint}, {"api-key-env", &r.APIKeyEnv, c.SystemOne.APIKeyEnv},
	}
	for _, f := range fields {
		if *f.dst != "" {
			r.Sources[f.key] = "entry"
		} else if r.Backend == "systemone" && f.value != "" {
			*f.dst = f.value
			r.Sources[f.key] = c.Sources["systemone."+f.key]
		}
	}
	if r.Timeout != 0 {
		r.Sources["timeout"] = "entry"
	} else if r.Backend == "systemone" {
		r.Timeout = c.SystemOne.Timeout
		r.Sources["timeout"] = c.Sources["systemone.timeout"]
	}
	if r.Timeout == 0 {
		r.Timeout = decisionrt.DefaultTimeout
		r.Sources["timeout"] = "default"
	}
	if r.Backend == "systemone" {
		if r.Model == "" {
			return Transport{}, fmt.Errorf("decision-runtime: systemone model is required")
		}
		if r.Endpoint == "" {
			r.Endpoint = DefaultEndpoint
			r.Sources["endpoint"] = "default"
		}
	}
	return r, nil
}

// Fingerprint freezes effective inference configuration and credential revision
// without persisting credential values. Empty legacy configuration stays empty.
func (c Config) Fingerprint(defaultBackend string) (string, error) {
	if c.Backend == "" && c.SystemOne == (SystemOne{}) {
		return "", nil
	}
	transport, err := Resolve(c, Transport{}, defaultBackend)
	if err != nil {
		return "", err
	}
	transport.Sources = nil
	credential := ""
	if transport.APIKeyEnv != "" {
		var present bool
		credential, present = os.LookupEnv(transport.APIKeyEnv)
		if !present || credential == "" {
			return "", fmt.Errorf("decision-runtime credential environment unavailable")
		}
	}
	keyHash := sha256.Sum256([]byte(credential))
	raw, err := json.Marshal(struct {
		Version            int
		Transport          Transport
		CredentialRevision string
	}{1, transport, hex.EncodeToString(keyHash[:])}, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

// Encode durations identically for the legacy journal JSON codec and JSON v2.
func (t Transport) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Backend  string            `json:"backend"`
		Model    string            `json:"model,omitempty"`
		Endpoint string            `json:"endpoint,omitempty"`
		Timeout  int64             `json:"timeout"`
		Sources  map[string]string `json:"sources"`
	}{t.Backend, t.Model, t.Endpoint, int64(t.Timeout), t.Sources}, json.Deterministic(true))
}
func (t *Transport) UnmarshalJSON(raw []byte) error {
	var value struct {
		Backend  string            `json:"backend"`
		Model    string            `json:"model,omitempty"`
		Endpoint string            `json:"endpoint,omitempty"`
		Timeout  int64             `json:"timeout"`
		Sources  map[string]string `json:"sources"`
	}
	if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	*t = Transport{Backend: value.Backend, Model: value.Model, Endpoint: value.Endpoint, Timeout: time.Duration(value.Timeout), Sources: value.Sources}
	return nil
}
