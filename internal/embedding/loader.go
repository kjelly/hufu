package embedding

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"unicode/utf8"
)

const (
	maxManifestBytes = 1 << 20
	maxVocabBytes    = 64 << 20
	maxTableBytes    = 512 << 20
	maxDimensions    = 2048
	maxVocabSize     = 1_000_000
)

// VerifiedModelFiles names locally available files and the identity the caller
// already trusts. The loader verifies the bytes again before constructing a
// Model; it does not establish distribution trust by itself.
type VerifiedModelFiles struct {
	ManifestPath string
	VocabPath    string
	TablePath    string
	Expected     ModelIdentity
}

type modelManifest struct {
	SchemaVersion  int               `json:"schema_version"`
	ID             string            `json:"id"`
	Revision       string            `json:"revision"`
	Dimensions     int               `json:"dimensions"`
	VocabSize      int               `json:"vocab_size"`
	DType          string            `json:"dtype"`
	Pooling        string            `json:"pooling"`
	Normalize      bool              `json:"normalize"`
	QueryPrefix    string            `json:"query_prefix"`
	DocumentPrefix string            `json:"document_prefix"`
	Tokenizer      tokenizerManifest `json:"tokenizer"`
	Files          manifestFiles     `json:"files"`
}

type tokenizerManifest struct {
	Type                 string `json:"type"`
	DoBasicTokenize      bool   `json:"do_basic_tokenize"`
	DoLowerCase          bool   `json:"do_lower_case"`
	TokenizeChineseChars bool   `json:"tokenize_chinese_chars"`
	StripAccents         bool   `json:"strip_accents"`
	UNKToken             string `json:"unk_token"`
	AddSpecialTokens     bool   `json:"add_special_tokens"`
	SkipTokenIDs         []int  `json:"skip_token_ids"`
	MaxInputCharsPerWord int    `json:"max_input_chars_per_word"`
	MaxInputTokens       int    `json:"max_input_tokens"`
}

type manifestFiles struct {
	Vocab string `json:"vocab.txt"`
	Table string `json:"embeddings.f32"`
}

func OpenVerifiedModel(files VerifiedModelFiles) (*Model, error) {
	if err := validateExpectedIdentity(files.Expected); err != nil {
		return nil, err
	}
	manifestBytes, err := readBoundedFile(files.ManifestPath, maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("read model manifest: %w", err)
	}
	if got := sha256Hex(manifestBytes); got != files.Expected.ManifestSHA256 {
		return nil, fmt.Errorf("model manifest hash mismatch: got %s", got)
	}

	manifest, err := decodeManifest(manifestBytes)
	if err != nil {
		return nil, err
	}
	if err = validateManifest(manifest, files.Expected); err != nil {
		return nil, err
	}

	vocabBytes, err := readBoundedFile(files.VocabPath, maxVocabBytes)
	if err != nil {
		return nil, fmt.Errorf("read model vocabulary: %w", err)
	}
	if !utf8.Valid(vocabBytes) {
		return nil, errors.New("model vocabulary is not valid UTF-8")
	}
	if got := sha256Hex(vocabBytes); got != manifest.Files.Vocab || got != files.Expected.TokenizerSHA256 {
		return nil, fmt.Errorf("model vocabulary hash mismatch: got %s", got)
	}
	tokens, err := parseVocabulary(vocabBytes, manifest.VocabSize)
	if err != nil {
		return nil, err
	}

	tableBytes, err := readBoundedFile(files.TablePath, maxTableBytes)
	if err != nil {
		return nil, fmt.Errorf("read embedding table: %w", err)
	}
	if got := sha256Hex(tableBytes); got != manifest.Files.Table || got != files.Expected.TableSHA256 {
		return nil, fmt.Errorf("embedding table hash mismatch: got %s", got)
	}
	vectors, err := decodeEmbeddingTable(tableBytes, manifest.VocabSize, manifest.Dimensions)
	if err != nil {
		return nil, err
	}

	tokenizer, err := newWordPieceTokenizer(tokens, manifest.Tokenizer)
	if err != nil {
		return nil, err
	}
	return &Model{
		identity:       files.Expected,
		tokenizer:      tokenizer,
		vectors:        vectors,
		dimensions:     manifest.Dimensions,
		queryPrefix:    manifest.QueryPrefix,
		documentPrefix: manifest.DocumentPrefix,
		skipTokenIDs:   intSet(manifest.Tokenizer.SkipTokenIDs),
	}, nil
}

func validateExpectedIdentity(identity ModelIdentity) error {
	if strings.TrimSpace(identity.ID) == "" || strings.TrimSpace(identity.Revision) == "" {
		return errors.New("expected model ID and revision are required")
	}
	if identity.Dimensions <= 0 || identity.Dimensions > maxDimensions {
		return fmt.Errorf("invalid expected model dimensions %d", identity.Dimensions)
	}
	for name, value := range map[string]string{
		"manifest":  identity.ManifestSHA256,
		"table":     identity.TableSHA256,
		"tokenizer": identity.TokenizerSHA256,
	} {
		if !validSHA256(value) {
			return fmt.Errorf("invalid expected %s SHA-256", name)
		}
	}
	return nil
}

func decodeManifest(data []byte) (modelManifest, error) {
	var manifest modelManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, fmt.Errorf("decode model manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return manifest, errors.New("decode model manifest: trailing JSON value")
		}
		return manifest, fmt.Errorf("decode model manifest trailing data: %w", err)
	}
	return manifest, nil
}

func validateManifest(manifest modelManifest, expected ModelIdentity) error {
	if manifest.SchemaVersion != 1 {
		return fmt.Errorf("unsupported model schema version %d", manifest.SchemaVersion)
	}
	if manifest.ID != expected.ID || manifest.Revision != expected.Revision {
		return fmt.Errorf("model identity mismatch: got %s@%s", manifest.ID, manifest.Revision)
	}
	if manifest.Dimensions != expected.Dimensions || manifest.Dimensions <= 0 || manifest.Dimensions > maxDimensions {
		return fmt.Errorf("model dimensions mismatch: got %d", manifest.Dimensions)
	}
	if manifest.VocabSize <= 0 || manifest.VocabSize > maxVocabSize {
		return fmt.Errorf("invalid model vocabulary size %d", manifest.VocabSize)
	}
	if manifest.DType != "float32-le" || manifest.Pooling != "mean" || !manifest.Normalize {
		return errors.New("unsupported model numeric contract")
	}
	t := manifest.Tokenizer
	if t.Type != "bert-wordpiece" || !t.DoBasicTokenize || !t.TokenizeChineseChars ||
		t.DoLowerCase || t.StripAccents || t.AddSpecialTokens || t.UNKToken == "" {
		return errors.New("unsupported tokenizer contract")
	}
	if t.MaxInputCharsPerWord <= 0 || t.MaxInputCharsPerWord > 1_000_000 ||
		t.MaxInputTokens <= 0 || t.MaxInputTokens > 1_000_000 {
		return errors.New("invalid tokenizer limits")
	}
	if !validSHA256(manifest.Files.Vocab) || !validSHA256(manifest.Files.Table) {
		return errors.New("invalid model file SHA-256")
	}
	return nil
}

func parseVocabulary(data []byte, expected int) ([]string, error) {
	lines := strings.Split(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) != expected {
		return nil, fmt.Errorf("model vocabulary has %d rows, want %d", len(lines), expected)
	}
	seen := make(map[string]struct{}, len(lines))
	for i, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			return nil, fmt.Errorf("model vocabulary row %d is empty", i)
		}
		if _, ok := seen[line]; ok {
			return nil, fmt.Errorf("model vocabulary token %q is duplicated", line)
		}
		seen[line] = struct{}{}
		lines[i] = line
	}
	return lines, nil
}

func decodeEmbeddingTable(data []byte, vocabSize, dimensions int) ([]float32, error) {
	if vocabSize > maxVocabSize || dimensions > maxDimensions || vocabSize <= 0 || dimensions <= 0 {
		return nil, errors.New("invalid embedding table dimensions")
	}
	wantValues := int64(vocabSize) * int64(dimensions)
	wantBytes := wantValues * 4
	if wantBytes > maxTableBytes || int64(len(data)) != wantBytes {
		return nil, fmt.Errorf("embedding table length is %d, want %d", len(data), wantBytes)
	}
	vectors := make([]float32, int(wantValues))
	for i := range vectors {
		value := math.Float32frombits(binary.LittleEndian.Uint32(data[i*4 : i*4+4]))
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("embedding table contains non-finite value at index %d", i)
		}
		vectors[i] = value
	}
	return vectors, nil
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("model file is not regular")
	}
	if info.Size() < 0 || info.Size() > limit {
		return nil, fmt.Errorf("model file size %d exceeds limit %d", info.Size(), limit)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("model file size %d exceeds limit %d", len(data), limit)
	}
	return data, nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func intSet(values []int) map[int]struct{} {
	set := make(map[int]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}
