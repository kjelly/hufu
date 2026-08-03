package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// SecretRef is safe to expose to a model or durable metadata. ExactValue is
// intentionally excluded from serialization and remains process-memory only.
type SecretRef struct {
	Name       string `json:"name" yaml:"name"`
	Source     string `json:"source,omitempty" yaml:"source,omitempty"`
	ExactValue string `json:"-" yaml:"-"`
}

type Redactor interface {
	RedactText(string) string
	RedactJSON([]byte) ([]byte, error)
}

type SecretRegistry struct {
	mu      sync.RWMutex
	secrets map[string]SecretRef
}

func NewSecretRegistry() *SecretRegistry {
	return &SecretRegistry{secrets: make(map[string]SecretRef)}
}

func (r *SecretRegistry) Register(ref SecretRef) error {
	if r == nil {
		return fmt.Errorf("secret registry is nil")
	}
	ref.Name = strings.TrimSpace(ref.Name)
	if ref.Name == "" || ref.ExactValue == "" {
		return fmt.Errorf("secret name and exact value are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.secrets[ref.Name]; ok && existing.ExactValue != ref.ExactValue {
		return fmt.Errorf("secret %q is already registered with a different value", ref.Name)
	}
	r.secrets[ref.Name] = ref
	return nil
}

func (r *SecretRegistry) Resolve(name string) (string, bool) {
	if r == nil {
		return "", false
	}
	r.mu.RLock()
	ref, ok := r.secrets[name]
	r.mu.RUnlock()
	if !ok {
		return "", false
	}
	return ref.ExactValue, true
}

func (r *SecretRegistry) References() []SecretRef {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	refs := make([]SecretRef, 0, len(r.secrets))
	for _, ref := range r.secrets {
		ref.ExactValue = ""
		refs = append(refs, ref)
	}
	r.mu.RUnlock()
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs
}

func (r *SecretRegistry) RedactText(value string) string {
	if r == nil {
		return value
	}
	r.mu.RLock()
	refs := make([]SecretRef, 0, len(r.secrets))
	for _, ref := range r.secrets {
		refs = append(refs, ref)
	}
	r.mu.RUnlock()
	// Replace longer values first so a short secret cannot partially expose a
	// longer credential that contains it.
	sort.Slice(refs, func(i, j int) bool { return len(refs[i].ExactValue) > len(refs[j].ExactValue) })
	for _, ref := range refs {
		if ref.ExactValue != "" {
			value = strings.ReplaceAll(value, ref.ExactValue, "[REDACTED:"+ref.Name+"]")
		}
	}
	return value
}

func (r *SecretRegistry) RedactJSON(data []byte) ([]byte, error) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, fmt.Errorf("redact JSON: invalid JSON: %w", err)
	}
	var redact func(any) any
	redact = func(v any) any {
		switch typed := v.(type) {
		case string:
			return r.RedactText(typed)
		case []any:
			for i := range typed {
				typed[i] = redact(typed[i])
			}
		case map[string]any:
			for key, item := range typed {
				typed[key] = redact(item)
			}
		}
		return v
	}
	redacted, err := json.Marshal(redact(value))
	if err != nil {
		return nil, fmt.Errorf("redact JSON: %w", err)
	}
	if len(bytes.TrimSpace(redacted)) == 0 {
		return nil, fmt.Errorf("redact JSON: empty result")
	}
	return redacted, nil
}
