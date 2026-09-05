package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/utils"
)

const RequestContractSchemaVersion = 1

type RequestContractProjection struct {
	DecisionID       string      `json:"decision_id"`
	TaskID           string      `json:"task_id,omitempty"`
	ContractRef      string      `json:"contract_ref"`
	ContractRevision uint64      `json:"contract_revision"`
	ContractArtifact ArtifactRef `json:"contract_artifact,omitempty"`
}

// RequestContractMaterial is the metadata-free identity input. Envelope
// identifiers and timestamps are deliberately excluded from its hash.
type RequestContractMaterial struct {
	RawRequest      string                            `json:"raw_request,omitempty"`
	DirectQuestion  string                            `json:"direct_question,omitempty"`
	Objective       string                            `json:"objective"`
	SuccessCriteria []agent.RequestSuccessCriterion   `json:"success_criteria"`
	Constraints     []agent.RequestConstraint         `json:"constraints"`
	Assumptions     []agent.RequestContractAssumption `json:"assumptions"`
}

type RequestContractEnvelope struct {
	SchemaVersion int                     `json:"schema_version"`
	ID            string                  `json:"id"`
	Revision      uint64                  `json:"revision"`
	CreatedAt     time.Time               `json:"created_at"`
	Material      RequestContractMaterial `json:"material"`
	MaterialHash  string                  `json:"material_hash"`
}

func (e RequestContractEnvelope) RequestContract() RequestContract {
	criteria := make([]SuccessCriterion, 0, len(e.Material.SuccessCriteria))
	for _, criterion := range e.Material.SuccessCriteria {
		criteria = append(criteria, SuccessCriterion{ID: criterion.ID, Statement: criterion.Statement})
	}
	constraints := make([]Constraint, 0, len(e.Material.Constraints))
	for _, constraint := range e.Material.Constraints {
		constraints = append(constraints, Constraint{ID: constraint.ID, Statement: constraint.Statement})
	}
	assumptions := make([]DecisionAssumption, 0, len(e.Material.Assumptions))
	for _, assumption := range e.Material.Assumptions {
		assumptions = append(assumptions, DecisionAssumption{ID: assumption.ID, Statement: assumption.Statement, Critical: assumption.Critical})
	}
	return RequestContract{ID: e.ID, RawRequest: e.Material.RawRequest, DirectQuestion: e.Material.DirectQuestion,
		Objective: e.Material.Objective, SuccessCriteria: criteria, Constraints: constraints, Assumptions: assumptions,
		Revision: e.Revision, CreatedAt: e.CreatedAt}
}

// BuildRequestContract copies only explicit configuration and runtime prompt
// fields; no model output participates in contract identity.
func BuildRequestContract(rawRequest, directQuestion string, cfg agent.RequestContractConfig, revision uint64, now time.Time) (RequestContractEnvelope, []byte, error) {
	if err := cfg.Validate(); err != nil {
		return RequestContractEnvelope{}, nil, err
	}
	material := RequestContractMaterial{
		RawRequest:      strings.TrimSpace(utils.RedactSecrets(rawRequest)),
		DirectQuestion:  strings.TrimSpace(utils.RedactSecrets(directQuestion)),
		Objective:       strings.TrimSpace(cfg.Objective),
		SuccessCriteria: append([]agent.RequestSuccessCriterion(nil), cfg.SuccessCriteria...),
		Constraints:     append([]agent.RequestConstraint(nil), cfg.Constraints...),
		Assumptions:     append([]agent.RequestContractAssumption(nil), cfg.Assumptions...),
	}
	sort.Slice(material.SuccessCriteria, func(i, j int) bool { return material.SuccessCriteria[i].ID < material.SuccessCriteria[j].ID })
	sort.Slice(material.Constraints, func(i, j int) bool { return material.Constraints[i].ID < material.Constraints[j].ID })
	sort.Slice(material.Assumptions, func(i, j int) bool { return material.Assumptions[i].ID < material.Assumptions[j].ID })
	materialBytes, err := json.Marshal(material)
	if err != nil {
		return RequestContractEnvelope{}, nil, fmt.Errorf("encode request contract material: %w", err)
	}
	hash := sha256.Sum256(materialBytes)
	materialHash := "sha256:" + hex.EncodeToString(hash[:])
	if now.IsZero() {
		now = time.Now().UTC()
	}
	envelope := RequestContractEnvelope{
		SchemaVersion: RequestContractSchemaVersion,
		ID:            materialHash, Revision: revision, CreatedAt: now.UTC(),
		Material: material, MaterialHash: materialHash,
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return RequestContractEnvelope{}, nil, fmt.Errorf("encode request contract: %w", err)
	}
	return envelope, data, nil
}

func PersistRequestContract(ctx context.Context, store ArtifactStore, envelope RequestContractEnvelope, data []byte) (ArtifactRef, error) {
	if store == nil {
		return ArtifactRef{}, fmt.Errorf("request contract artifact store is unavailable")
	}
	result, err := store.Put(ctx, PutArtifactRequest{Content: data, Path: "decisions/contracts/" + envelope.ID + ".json", Kind: "request_contract", Role: "decision", Description: "request contract " + envelope.ID, MediaType: "application/json"})
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("persisting request contract: %w", err)
	}
	return result.ArtifactRef, nil
}

func (e RequestContractEnvelope) Validate() error {
	if e.SchemaVersion != RequestContractSchemaVersion {
		return fmt.Errorf("unsupported request contract schema version %d", e.SchemaVersion)
	}
	if strings.TrimSpace(e.ID) == "" || strings.TrimSpace(e.MaterialHash) == "" || e.ID != e.MaterialHash {
		return fmt.Errorf("request contract identity is invalid")
	}
	material, err := json.Marshal(e.Material)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(material)
	if e.MaterialHash != "sha256:"+hex.EncodeToString(hash[:]) {
		return fmt.Errorf("request contract material hash mismatch")
	}
	return nil
}
