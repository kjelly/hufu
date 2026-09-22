package decisionrt

import (
	"context"
	"time"
)

type Kind string

const (
	KindChoice       Kind = "choice"
	KindBoolean      Kind = "boolean"
	KindIntegerRange Kind = "integer_range"
)

type Option struct {
	ID          string `json:"id"`
	Description string `json:"description,omitempty"`
}

type IntegerRange struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
}

type Spec struct {
	ID       string        `json:"id"`
	Version  string        `json:"version"`
	Kind     Kind          `json:"kind"`
	Question string        `json:"question"`
	Options  []Option      `json:"options,omitempty"`
	Range    *IntegerRange `json:"range,omitempty"`
}

type Request struct {
	Purpose string         `json:"purpose"`
	Spec    Spec           `json:"spec"`
	Context map[string]any `json:"context,omitempty"`
}

type Value struct {
	Choice  string `json:"choice,omitempty"`
	Boolean *bool  `json:"boolean,omitempty"`
	Integer *int64 `json:"integer,omitempty"`
}

type Candidate struct {
	Value       string  `json:"value"`
	Probability float64 `json:"probability"`
}

type ConfidenceSemantics string

const (
	ConfidenceNone       ConfidenceSemantics = "none"
	ConfidenceRaw        ConfidenceSemantics = "raw"
	ConfidenceCalibrated ConfidenceSemantics = "calibrated"
)

type Status string

const (
	StatusDecided   Status = "decided"
	StatusAbstained Status = "abstained"
)

type Result struct {
	Status              Status              `json:"status"`
	Value               Value               `json:"value"`
	Candidates          []Candidate         `json:"candidates,omitempty"`
	Confidence          float64             `json:"confidence,omitzero"`
	ConfidenceSemantics ConfidenceSemantics `json:"confidence_semantics"`
	Backend             string              `json:"backend"`
	Model               string              `json:"model,omitempty"`
	FallbackUsed        bool                `json:"fallback_used"`
	ReasonCode          string              `json:"reason_code,omitempty"`
}

type BackendResult struct {
	Status              Status
	Value               Value
	Candidates          []Candidate
	Confidence          float64
	ConfidenceSemantics ConfidenceSemantics
	Model               string
}

type Backend interface {
	Name() string
	Decide(context.Context, Request) (BackendResult, error)
}

type AcceptancePolicy struct {
	MinConfidence               *float64
	RequireCalibratedConfidence bool
}

type Runtime interface {
	Decide(context.Context, Request) (Result, Receipt, error)
}

type RuntimeConfig struct {
	Primary  Backend
	Fallback Backend
	Policy   AcceptancePolicy
	Timeout  time.Duration
	Metrics  Metrics
}

type Receipt struct {
	SchemaVersion int `json:"schema_version"`

	Purpose       string `json:"purpose"`
	RequestDigest string `json:"request_digest"`

	SpecID      string `json:"spec_id"`
	SpecVersion string `json:"spec_version"`

	Backend string `json:"backend"`
	Model   string `json:"model,omitempty"`

	Status     Status `json:"status"`
	ReasonCode string `json:"reason_code,omitempty"`

	FallbackUsed bool   `json:"fallback_used"`
	DurationMS   uint64 `json:"duration_ms"`
}
