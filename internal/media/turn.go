package media

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"slices"
	"strings"
)

type Part struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	MediaID string `json:"media_id,omitempty"`
}

type TurnInput struct {
	Version int    `json:"version"`
	Parts   []Part `json:"parts"`
}

type Occurrence struct {
	RunID        string `json:"run_id"`
	BranchID     string `json:"branch_id"`
	InvocationID string `json:"invocation_id"`
	TurnID       string `json:"turn_id"`
	PartIndex    int    `json:"part_index"`
	MediaID      string `json:"media_id"`
	Origin       string `json:"origin"`
	ToolCallID   string `json:"tool_call_id,omitempty"`
}

type InputSnapshot struct {
	Version      int          `json:"version"`
	ID           string       `json:"id"`
	Hash         string       `json:"hash"`
	RunID        string       `json:"run_id"`
	BranchID     string       `json:"branch_id"`
	InvocationID string       `json:"invocation_id"`
	TurnID       string       `json:"turn_id"`
	PolicyHash   string       `json:"policy_hash"`
	Input        TurnInput    `json:"input"`
	Refs         []Ref        `json:"refs"`
	Occurrences  []Occurrence `json:"occurrences"`
}

func Digest(value any) (string, error) {
	encoded, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(h[:]), nil
}

func (s InputSnapshot) Clone() InputSnapshot {
	s.Input.Parts = slices.Clone(s.Input.Parts)
	s.Refs = slices.Clone(s.Refs)
	s.Occurrences = slices.Clone(s.Occurrences)
	return s
}

func (s InputSnapshot) digest() (string, error) { s.ID = ""; s.Hash = ""; return Digest(s) }

func SealSnapshot(s InputSnapshot, limits Limits) (InputSnapshot, error) {
	s = s.Clone()
	s.Version = Version
	s.Input.Version = Version
	hash, err := s.digest()
	if err != nil {
		return InputSnapshot{}, err
	}
	s.Hash = hash
	s.ID = "media-input-v1-" + hash[len("sha256:"):]
	if err := s.Validate(limits); err != nil {
		return InputSnapshot{}, err
	}
	return s, nil
}

func (s InputSnapshot) Validate(limits Limits) error {
	if s.Version != Version || s.Input.Version != Version || !validIdentity(s.RunID) || !validIdentity(s.BranchID) || !validIdentity(s.InvocationID) || !validIdentity(s.TurnID) || !validHash(s.PolicyHash) || len(s.Input.Parts) > 128 {
		return Reject(BindingConflict, "incomplete media input identity")
	}
	refs := make(map[string]Ref, len(s.Refs))
	for _, r := range s.Refs {
		if err := r.Validate(); err != nil {
			return err
		}
		if _, ok := refs[r.ID]; ok {
			return Reject(BindingConflict, "duplicate canonical image metadata")
		}
		refs[r.ID] = r
	}
	var bundle []Ref
	var textBytes, occurrence int
	used := make(map[string]bool)
	for index, p := range s.Input.Parts {
		switch p.Type {
		case "text":
			if p.Text == "" || p.MediaID != "" {
				return Reject(BindingConflict, "invalid text part")
			}
			textBytes += len(p.Text)
			if textBytes > 256<<10 {
				return Reject(LimitExceeded, "media turn text exceeds byte budget")
			}
		case "image":
			r, ok := refs[p.MediaID]
			if !ok || p.Text != "" || occurrence >= len(s.Occurrences) {
				return Reject(BindingConflict, "invalid image part")
			}
			o := s.Occurrences[occurrence]
			if o.RunID != s.RunID || o.BranchID != s.BranchID || o.InvocationID != s.InvocationID || o.TurnID != s.TurnID || o.PartIndex != index || o.MediaID != p.MediaID || !validOrigin(o) {
				return Reject(BindingConflict, "image occurrence does not match its part")
			}
			bundle = append(bundle, r)
			occurrence++
			used[r.ID] = true
		default:
			return Reject(BindingConflict, "unknown media part type")
		}
	}
	if occurrence != len(s.Occurrences) || len(used) != len(s.Refs) {
		return Reject(BindingConflict, "unreferenced media metadata or occurrences")
	}
	if err := CheckBundle(bundle, limits); err != nil {
		return err
	}
	hash, err := s.digest()
	if err != nil {
		return err
	}
	if s.Hash != hash || s.ID != "media-input-v1-"+hash[len("sha256:"):] {
		return Reject(IntegrityMismatch, "media snapshot hash mismatch")
	}
	return nil
}

func validIdentity(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "/\\\x00\r\n\t ;")
}

func validHash(value string) bool {
	digest, ok := strings.CutPrefix(value, "sha256:")
	return ok && validDigest(digest)
}

func validOrigin(o Occurrence) bool {
	switch o.Origin {
	case "user_file", "trusted_artifact":
		return o.ToolCallID == ""
	case "mcp_tool":
		return o.ToolCallID != ""
	default:
		return false
	}
}

type Selection struct {
	SnapshotID   string `json:"snapshot_id"`
	SnapshotHash string `json:"snapshot_hash"`
	TurnID       string `json:"turn_id"`
	PartIndex    int    `json:"part_index"`
}

// TaskBinding's owner is finalized after reserving the Todo ID. Revision and
// lease are deliberately not immutable input: the current occurrence grants
// each attempt separately, after its transition has committed.
type TaskBinding struct {
	Version     int         `json:"version"`
	RunID       string      `json:"run_id"`
	BranchID    string      `json:"branch_id"`
	TaskID      string      `json:"task_id"`
	Agent       string      `json:"agent"`
	Purpose     string      `json:"purpose"`
	PolicyHash  string      `json:"policy_hash"`
	Selections  []Selection `json:"selections"`
	Refs        []Ref       `json:"refs"`
	InputDigest string      `json:"input_digest"`
	Hash        string      `json:"hash"`
}

func (b *TaskBinding) Clone() *TaskBinding {
	if b == nil {
		return nil
	}
	copy := *b
	copy.Selections = slices.Clone(b.Selections)
	copy.Refs = slices.Clone(b.Refs)
	return &copy
}

func (b TaskBinding) Seal() (TaskBinding, error) {
	var err error
	b.Hash = ""
	b.InputDigest, err = Digest(struct {
		Selections []Selection `json:"selections"`
		Refs       []Ref       `json:"refs"`
	}{b.Selections, b.Refs})
	if err != nil {
		return TaskBinding{}, err
	}
	b.Hash, err = Digest(b)
	return b, err
}

func (b TaskBinding) Validate(limits Limits) error {
	if b.Version != Version || !validIdentity(b.RunID) || !validIdentity(b.BranchID) || !validIdentity(b.TaskID) || b.Agent == "" || b.Purpose != "image_analysis" || !validHash(b.PolicyHash) || len(b.Selections) != len(b.Refs) {
		return Reject(BindingConflict, "incomplete task media binding")
	}
	if err := CheckBundle(b.Refs, limits); err != nil {
		return err
	}
	for _, s := range b.Selections {
		if s.SnapshotID == "" || s.SnapshotHash == "" || s.TurnID == "" || s.PartIndex < 0 {
			return Reject(BindingConflict, "invalid media selection")
		}
	}
	sealed, err := b.Seal()
	if err != nil {
		return err
	}
	if sealed.Hash != b.Hash || sealed.InputDigest != b.InputDigest {
		return Reject(IntegrityMismatch, "task media binding hash mismatch")
	}
	return nil
}
