package memory

import (
	"encoding/json"
	"fmt"
	"time"
)

// Memory lifecycle statuses as defined in HF-MEM-001 (§20.2).
const (
	StatusCandidate  = "candidate"
	StatusConfirmed  = "confirmed"
	StatusSuperseded = "superseded"
	StatusExpired    = "expired"
	StatusRejected   = "rejected"
)

// MemoryRecord represents a single memory unit with full provenance, confidence,
// lifecycle status, and supersession tracking (§20.1).
type MemoryRecord struct {
	ID              string            `json:"id"`
	Content         string            `json:"content"`
	Category        string            `json:"category"`
	Project         string            `json:"project"`
	Team            string            `json:"team"`
	SourceEventIDs  []string          `json:"source_event_ids,omitempty"`
	SourceTaskID    string            `json:"source_task_id,omitempty"`
	SourceAgent     string            `json:"source_agent,omitempty"`
	FilePaths       []string          `json:"file_paths,omitempty"`
	CommitHash      string            `json:"commit_hash,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	LastConfirmedAt time.Time         `json:"last_confirmed_at,omitempty"`
	Confidence      float64           `json:"confidence"`
	ExpiresAt       *time.Time        `json:"expires_at,omitempty"`
	Supersedes      []string          `json:"supersedes,omitempty"`
	Status          string            `json:"status"`
	ExtraMeta       map[string]string `json:"extra_meta,omitempty"` // preserve non-schema metadata for backward compat (R1)
}

// IsExpired checks whether the record has passed its expiration time.
func (r *MemoryRecord) IsExpired() bool {
	if r.ExpiresAt != nil && !r.ExpiresAt.IsZero() && time.Now().After(*r.ExpiresAt) {
		return true
	}
	return r.Status == StatusExpired
}

// EffectiveStatus returns the current status, taking expiration into account.
func (r *MemoryRecord) EffectiveStatus() string {
	if r.IsExpired() {
		return StatusExpired
	}
	if r.Status == "" {
		return StatusConfirmed
	}
	return r.Status
}

// recordToMetadata converts a MemoryRecord to a chromem.Document metadata map.
func recordToMetadata(rec MemoryRecord) (map[string]string, error) {
	data, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal memory record: %w", err)
	}

	meta := map[string]string{
		"_record_json":   string(data),
		"category":       rec.Category,
		"status":         rec.EffectiveStatus(),
		"project":        rec.Project,
		"team":           rec.Team,
		"saved_at":       rec.CreatedAt.Format(time.RFC3339),
		"source_task_id": rec.SourceTaskID,
		"source_agent":   rec.SourceAgent,
		"commit_hash":    rec.CommitHash,
		"confidence":     fmt.Sprintf("%.2f", rec.Confidence),
	}
	// Spread extra metadata keys for chromem filter compatibility (R1)
	for k, v := range rec.ExtraMeta {
		if _, exists := meta[k]; !exists {
			meta[k] = v
		}
	}
	return meta, nil
}

// metadataToRecord converts a chromem.Document ID, content, and metadata map back into a MemoryRecord.
func metadataToRecord(docID, content string, metadata map[string]string) (MemoryRecord, error) {
	if metadata != nil {
		if jsonStr, ok := metadata["_record_json"]; ok && jsonStr != "" {
			var rec MemoryRecord
			if err := json.Unmarshal([]byte(jsonStr), &rec); err == nil {
				if rec.ID == "" {
					rec.ID = docID
				}
				if rec.Content == "" {
					rec.Content = content
				}
				return rec, nil
			}
		}
	}

	// Fallback for legacy memory documents without _record_json
	rec := MemoryRecord{
		ID:         docID,
		Content:    content,
		Status:     StatusConfirmed,
		Confidence: 1.0,
	}

	if metadata != nil {
		if cat, ok := metadata["category"]; ok {
			rec.Category = cat
		}
		if st, ok := metadata["status"]; ok && st != "" {
			rec.Status = st
		}
		if proj, ok := metadata["project"]; ok {
			rec.Project = proj
		}
		if team, ok := metadata["team"]; ok {
			rec.Team = team
		}
		if taskID, ok := metadata["source_task_id"]; ok {
			rec.SourceTaskID = taskID
		}
		if agent, ok := metadata["source_agent"]; ok {
			rec.SourceAgent = agent
		}
		if hash, ok := metadata["commit_hash"]; ok {
			rec.CommitHash = hash
		}
		if savedAtStr, ok := metadata["saved_at"]; ok && savedAtStr != "" {
			if t, err := time.Parse(time.RFC3339, savedAtStr); err == nil {
				rec.CreatedAt = t
			}
		}
	}

	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}

	return rec, nil
}
