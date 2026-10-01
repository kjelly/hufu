package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/decisionrt/backend"
	"github.com/kjelly/hufu/internal/decisionrt/backend/systemone"
	"github.com/kjelly/hufu/internal/decisionrt/catalog"
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Service decides the enabled points against one systemone transport. A nil
// or empty Service reports every point as off. It is safe for concurrent use.
type Service struct {
	runtime decisionrt.Runtime
	points  map[Point]resolvedPoint
	model   string
	redact  func(string) string
	hash    string
}

// New validates config and binds the systemone backend without network I/O.
// redact is applied to every context string before it leaves the process.
// When every point is off, New does not validate or bind the transport.
func New(config Config, redact func(string) string) (*Service, error) {
	if redact == nil {
		return nil, fmt.Errorf("control-decisions: a redactor is required")
	}
	points, err := config.resolvePoints()
	if err != nil {
		return nil, err
	}
	service := &Service{points: make(map[Point]resolvedPoint), redact: redact}
	for point, resolved := range points {
		if resolved.Mode != ModeOff {
			service.points[point] = resolved
		}
	}
	if len(service.points) == 0 {
		return service, nil
	}
	timeout, err := validateTimeout(config.Timeout)
	if err != nil {
		return nil, err
	}
	endpoint := config.Endpoint
	if endpoint == "" {
		endpoint = catalog.DefaultEndpoint
	}
	key := ""
	if config.APIKeyEnv != "" {
		if !envName.MatchString(config.APIKeyEnv) {
			return nil, fmt.Errorf("control-decisions.api-key-env: invalid variable name")
		}
		var present bool
		key, present = os.LookupEnv(config.APIKeyEnv)
		if !present || key == "" {
			return nil, fmt.Errorf("control-decisions.api-key-env: credential environment unavailable")
		}
	}
	primary, err := backend.New(backend.Config{Name: "systemone", Endpoint: endpoint, Model: config.Model, APIKey: key})
	if err != nil {
		return nil, fmt.Errorf("control-decisions: %s: %w", transportProblem(err), err)
	}
	// The threshold is applied by Decide, not by the runtime policy, so a
	// shadow record keeps the raw confidence of an answer it would reject.
	runtime, err := decisionrt.NewRuntime(decisionrt.RuntimeConfig{Primary: primary, Timeout: timeout})
	if err != nil {
		return nil, fmt.Errorf("control-decisions: %w", err)
	}
	service.runtime = runtime
	service.model = config.Model
	service.hash, err = activeHash(endpoint, config.Model, config.APIKeyEnv, key, timeout, service.points)
	return service, err
}

// transportProblem names the configuration field a systemone constructor
// error is about, without repeating the configured value.
func transportProblem(err error) string {
	switch {
	case errors.Is(err, systemone.ErrMissingModel):
		return "model is required"
	case errors.Is(err, systemone.ErrInvalidModel):
		return "invalid model"
	case errors.Is(err, systemone.ErrInvalidEndpoint):
		return "invalid endpoint"
	case errors.Is(err, systemone.ErrInvalidAPIKey):
		return "invalid API key"
	default:
		return "invalid systemone configuration"
	}
}

// activeHash pins the policy that can change behavior: the transport, the
// credential revision, and every active point. It is empty when no point is
// active, so shadow-only configuration never affects resume admission.
func activeHash(endpoint, model, keyEnv, key string, timeout time.Duration, points map[Point]resolvedPoint) (string, error) {
	active := make(map[Point]resolvedPoint)
	for point, resolved := range points {
		if resolved.Mode == ModeActive {
			active[point] = resolved
		}
	}
	if len(active) == 0 {
		return "", nil
	}
	credential := sha256.Sum256([]byte(key))
	encoded, err := json.Marshal(struct {
		Endpoint       string                  `json:"endpoint"`
		Model          string                  `json:"model"`
		APIKeyEnv      string                  `json:"api_key_env"`
		CredentialHash string                  `json:"credential_hash"`
		TimeoutNS      int64                   `json:"timeout_ns"`
		Points         map[Point]resolvedPoint `json:"points"`
	}{endpoint, model, keyEnv, hex.EncodeToString(credential[:]), int64(timeout), active})
	if err != nil {
		return "", fmt.Errorf("control-decisions: hash policy: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// Mode returns the effective mode of point.
func (s *Service) Mode(point Point) Mode {
	if s == nil {
		return ModeOff
	}
	if resolved, ok := s.points[point]; ok {
		return resolved.Mode
	}
	return ModeOff
}

// Enabled reports whether any point is shadow or active.
func (s *Service) Enabled() bool { return s != nil && len(s.points) > 0 }

// Hash pins the active policy; it is empty when no point is active.
func (s *Service) Hash() string {
	if s == nil {
		return ""
	}
	return s.hash
}

// MinConfidence returns the effective threshold of point.
func (s *Service) MinConfidence(point Point) float64 {
	if s != nil {
		if resolved, ok := s.points[point]; ok {
			return resolved.MinConfidence
		}
	}
	return defaultMinConfidence[point]
}

// EnabledPoints returns the shadow and active points in a stable order.
func (s *Service) EnabledPoints() []Point {
	if s == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(s.points))
}

// Status is the outcome class of one control decision.
type Status string

const (
	StatusDecided   Status = "decided"
	StatusAbstained Status = "abstained"
	StatusError     Status = "error"
)

// Outcome is the content-free result of one decision. Value encodes a
// boolean as "true"/"false" and a choice as its 0-based option index.
type Outcome struct {
	Status     Status
	Value      string
	Confidence float64
	// Accepted reports a decided answer that meets the point's threshold.
	Accepted   bool
	ReasonCode string
	ErrorCode  string
	Model      string
	Duration   time.Duration
}

// Bool returns a decided boolean value.
func (o Outcome) Bool() (bool, bool) {
	if o.Status != StatusDecided {
		return false, false
	}
	value, err := strconv.ParseBool(o.Value)
	return value, err == nil
}

// Index returns a decided choice index.
func (o Outcome) Index() (int, bool) {
	if o.Status != StatusDecided {
		return 0, false
	}
	index, err := strconv.Atoi(o.Value)
	return index, err == nil && index >= 0
}

// Decide asks the decision model for point. It never returns an error: a
// technical failure is an Outcome with StatusError, which callers treat as
// "run the existing path".
func (s *Service) Decide(ctx context.Context, point Point, request decisionrt.Request) Outcome {
	started := time.Now()
	if s.Mode(point) == ModeOff || s.runtime == nil {
		return Outcome{Status: StatusError, ErrorCode: "point_off"}
	}
	redacted := request
	redacted.Context = make(map[string]any, len(request.Context))
	for key, value := range request.Context {
		if text, ok := value.(string); ok {
			value = s.redact(text)
		}
		redacted.Context[key] = value
	}
	result, _, err := s.runtime.Decide(ctx, redacted)
	outcome := Outcome{Model: s.model, Duration: time.Since(started)}
	if err != nil {
		outcome.Status, outcome.ErrorCode = StatusError, errorCode(err)
		return outcome
	}
	if result.Status != decisionrt.StatusDecided {
		outcome.Status, outcome.ReasonCode = StatusAbstained, result.ReasonCode
		return outcome
	}
	value, ok := encodeValue(request.Spec, result.Value)
	if !ok {
		outcome.Status, outcome.ErrorCode = StatusError, "invalid_backend_output"
		return outcome
	}
	outcome.Status, outcome.Value, outcome.Confidence = StatusDecided, value, result.Confidence
	outcome.Accepted = result.ConfidenceSemantics != decisionrt.ConfidenceNone && result.Confidence >= s.MinConfidence(point)
	return outcome
}

func encodeValue(spec decisionrt.Spec, value decisionrt.Value) (string, bool) {
	switch spec.Kind {
	case decisionrt.KindBoolean:
		if value.Boolean == nil {
			return "", false
		}
		return strconv.FormatBool(*value.Boolean), true
	case decisionrt.KindChoice:
		for index, option := range spec.Options {
			if option.ID == value.Choice {
				return strconv.Itoa(index), true
			}
		}
	}
	return "", false
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	if typed, ok := errors.AsType[*decisionrt.RuntimeError](err); ok {
		return string(typed.Kind)
	}
	return "backend_failure"
}
