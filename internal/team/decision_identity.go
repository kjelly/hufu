package team

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxCanonicalJSONInteger = uint64(1<<53 - 1)

// NewLogicalRunID creates a non-recoverable logical identity. Failure to read
// cryptographic entropy is returned; there is deliberately no clock fallback.
func NewLogicalRunID() (string, error) {
	return newLogicalRunID(rand.Reader)
}

func newLogicalRunID(source io.Reader) (string, error) {
	if source == nil {
		return "", fmt.Errorf("generate logical run id: entropy source is unavailable")
	}
	value := make([]byte, 16)
	if _, err := io.ReadFull(source, value); err != nil {
		return "", fmt.Errorf("generate logical run id: %w", err)
	}
	return "ldr_" + hex.EncodeToString(value), nil
}

// CanonicalDecisionJSON implements hufu-json-c14n@v1 for contract metadata.
// It accepts JSON-compatible Go values and rejects floats and unsafe integers.
func CanonicalDecisionJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal canonical decision input: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode canonical decision input: %w", err)
	}
	var out bytes.Buffer
	if err := appendCanonicalDecisionJSON(&out, decoded); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func appendCanonicalDecisionJSON(out *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if typed {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		return appendCanonicalJSONString(out, typed)
	case json.Number:
		text := typed.String()
		if strings.ContainsAny(text, ".eE+-") || strings.HasPrefix(text, "0") && text != "0" {
			return fmt.Errorf("canonical decision JSON only accepts unsigned base-10 integers: %q", text)
		}
		integer, err := strconv.ParseUint(text, 10, 64)
		if err != nil || integer > maxCanonicalJSONInteger {
			return fmt.Errorf("canonical decision JSON integer is outside 0..2^53-1: %q", text)
		}
		out.WriteString(text)
	case []any:
		out.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				out.WriteByte(',')
			}
			if err := appendCanonicalDecisionJSON(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				out.WriteByte(',')
			}
			if err := appendCanonicalJSONString(out, key); err != nil {
				return err
			}
			out.WriteByte(':')
			if err := appendCanonicalDecisionJSON(out, typed[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("canonical decision JSON has unsupported value type %T", value)
	}
	return nil
}

func appendCanonicalJSONString(out *bytes.Buffer, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("canonical decision JSON string is not valid UTF-8")
	}
	const hexDigits = "0123456789abcdef"
	out.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"', '\\':
			out.WriteByte('\\')
			out.WriteRune(r)
		case '\b':
			out.WriteString(`\b`)
		case '\t':
			out.WriteString(`\t`)
		case '\n':
			out.WriteString(`\n`)
		case '\f':
			out.WriteString(`\f`)
		case '\r':
			out.WriteString(`\r`)
		default:
			if r < 0x20 {
				out.WriteString(`\u00`)
				out.WriteByte(hexDigits[byte(r)>>4])
				out.WriteByte(hexDigits[byte(r)&0x0f])
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
	return nil
}

// DecisionContractDigest computes H(domain, object).
func DecisionContractDigest(domain string, value any) (string, error) {
	if strings.TrimSpace(domain) == "" || strings.ContainsRune(domain, 0) {
		return "", fmt.Errorf("decision digest domain is invalid")
	}
	canonical, err := CanonicalDecisionJSON(value)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(domain))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(canonical)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func DecisionTeamID(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("decision team name is empty")
	}
	digest, err := DecisionContractDigest("hufu/team-identity/v1", struct {
		Name string `json:"name"`
	}{Name: name})
	if err != nil {
		return "", err
	}
	return "team_" + digest, nil
}

func PrimaryDecisionTaskID(branchID, logicalRunID string) (string, error) {
	digest, err := DecisionContractDigest("hufu/primary-task/v1", struct {
		BranchID     string `json:"branch_id"`
		LogicalRunID string `json:"logical_run_id"`
	}{BranchID: branchID, LogicalRunID: logicalRunID})
	if err != nil {
		return "", err
	}
	return "__hufu_pd_" + digest, nil
}

func PrimaryDecisionID(branchID, logicalRunID string, generation uint32) (string, error) {
	if generation == 0 {
		return "", fmt.Errorf("primary decision generation must be positive")
	}
	digest, err := DecisionContractDigest("hufu/primary-decision/v1", struct {
		BranchID     string `json:"branch_id"`
		Generation   uint32 `json:"generation"`
		LogicalRunID string `json:"logical_run_id"`
	}{BranchID: branchID, Generation: generation, LogicalRunID: logicalRunID})
	if err != nil {
		return "", err
	}
	return "pd_" + digest, nil
}

// strictTeamDefinitionRevision preserves teamDefinitionRevision's framing but
// fails when any selected input cannot be read.
// StrictTeamDefinitionRevision hashes the same inputs and framing as the
// existing telemetry revision, but fails closed on absent or unreadable data.
func StrictTeamDefinitionRevision(teamDir string) (string, error) {
	entries, err := os.ReadDir(teamDir)
	if err != nil {
		return "", fmt.Errorf("read team definition directory: %w", err)
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "team.yaml" || name == "team.yml" || strings.HasSuffix(name, ".md") {
			files = append(files, name)
		}
	}
	slices.Sort(files)
	if len(files) == 0 {
		return "", fmt.Errorf("team definition has no configuration or agent files")
	}
	hash := sha256.New()
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(teamDir, name))
		if err != nil {
			return "", fmt.Errorf("read team definition %q: %w", name, err)
		}
		_, _ = hash.Write([]byte(name))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(data)
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
