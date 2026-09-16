package embedding

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

var fixtureTokens = []string{"[UNK]", "[PAD]", "協", "調", "器", "resume", "失", "敗"}

var fixtureVectors = [][]float32{
	{1, 0, 0, 0},
	{0, 1, 0, 0},
	{1, 0, 0, 0},
	{0, 1, 0, 0},
	{0, 0, 1, 0},
	{0, 0, 0, 1},
	{1, 1, 0, 0},
	{0, 1, 1, 0},
}

func TestOpenVerifiedModelAndEmbed(t *testing.T) {
	files := writeFixtureModel(t, nil, nil)
	model, err := OpenVerifiedModel(files)
	if err != nil {
		t.Fatal(err)
	}
	if got := model.Identity(); got != files.Expected {
		t.Fatalf("identity = %#v, want %#v", got, files.Expected)
	}

	vector, err := model.EmbedQuery(t.Context(), "協調器")
	if err != nil {
		t.Fatal(err)
	}
	want := float32(1 / math.Sqrt(3))
	for i := range 3 {
		if diff := math.Abs(float64(vector[i] - want)); diff > 1e-6 {
			t.Fatalf("vector[%d] = %f, want %f", i, vector[i], want)
		}
	}
	if vector[3] != 0 {
		t.Fatalf("vector[3] = %f, want 0", vector[3])
	}

	documents, err := model.EmbedDocuments(t.Context(), []string{"resume", "失敗"})
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 2 || documents[0][3] != 1 {
		t.Fatalf("document vectors = %#v", documents)
	}
}

func TestModelIsSafeForConcurrentEmbedding(t *testing.T) {
	model, err := OpenVerifiedModel(writeFixtureModel(t, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			vector, embedErr := model.EmbedQuery(t.Context(), "協調器 resume")
			if embedErr != nil {
				t.Error(embedErr)
				return
			}
			if len(vector) != 4 {
				t.Errorf("vector length = %d, want 4", len(vector))
			}
		})
	}
	wg.Wait()
}

func TestOpenVerifiedModelRejectsMalformedInputs(t *testing.T) {
	tests := []struct {
		name           string
		mutateManifest func(*modelManifest)
		mutateTable    func([]byte) []byte
	}{
		{
			name: "unsupported schema",
			mutateManifest: func(manifest *modelManifest) {
				manifest.SchemaVersion = 2
			},
		},
		{
			name: "dimension mismatch",
			mutateManifest: func(manifest *modelManifest) {
				manifest.Dimensions = 5
			},
		},
		{
			name: "non-finite table",
			mutateTable: func(table []byte) []byte {
				binary.LittleEndian.PutUint32(table[:4], math.Float32bits(float32(math.NaN())))
				return table
			},
		},
		{
			name: "truncated table",
			mutateTable: func(table []byte) []byte {
				return table[:len(table)-1]
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files := writeFixtureModel(t, test.mutateManifest, test.mutateTable)
			if _, err := OpenVerifiedModel(files); err == nil {
				t.Fatal("OpenVerifiedModel succeeded, want error")
			}
		})
	}
}

func TestOpenVerifiedModelRejectsUnknownManifestField(t *testing.T) {
	files := writeFixtureModel(t, nil, nil)
	data, err := os.ReadFile(files.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err = json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	object["unexpected"] = true
	data, err = json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(files.ManifestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	files.Expected.ManifestSHA256 = hashHex(data)
	if _, err = OpenVerifiedModel(files); err == nil {
		t.Fatal("OpenVerifiedModel succeeded, want unknown-field error")
	}
}

func TestOpenVerifiedModelRejectsMalformedVocabulary(t *testing.T) {
	tests := []struct {
		name  string
		vocab []byte
	}{
		{name: "duplicate token", vocab: []byte("[UNK]\n[UNK]\n協\n調\n器\nresume\n失\n敗\n")},
		{name: "empty token", vocab: []byte("[UNK]\n\n協\n調\n器\nresume\n失\n敗\n")},
		{name: "invalid UTF-8", vocab: []byte("[UNK]\n[PAD]\n協\n調\n器\nresume\n失\n\xff\n")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files := writeFixtureModel(t, nil, nil)
			if err := os.WriteFile(files.VocabPath, test.vocab, 0o600); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(files.ManifestPath)
			if err != nil {
				t.Fatal(err)
			}
			var manifest modelManifest
			if err = json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			manifest.Files.Vocab = hashHex(test.vocab)
			data, err = json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(files.ManifestPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			files.Expected.ManifestSHA256 = hashHex(data)
			files.Expected.TokenizerSHA256 = hashHex(test.vocab)
			if _, err = OpenVerifiedModel(files); err == nil {
				t.Fatal("OpenVerifiedModel succeeded, want vocabulary error")
			}
		})
	}
}

func TestOpenVerifiedModelRejectsOutOfRangeSkipToken(t *testing.T) {
	files := writeFixtureModel(t, func(manifest *modelManifest) {
		manifest.Tokenizer.SkipTokenIDs = []int{len(fixtureTokens)}
	}, nil)
	if _, err := OpenVerifiedModel(files); err == nil {
		t.Fatal("OpenVerifiedModel succeeded, want skip-token error")
	}
}

func TestModelErrorsAndCancellation(t *testing.T) {
	model, err := OpenVerifiedModel(writeFixtureModel(t, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = model.EmbedQuery(t.Context(), ""); !errors.Is(err, ErrNoEmbeddableTokens) {
		t.Fatalf("empty input error = %v, want ErrNoEmbeddableTokens", err)
	}
	if _, err = model.EmbedQuery(t.Context(), string([]byte{0xff})); err == nil {
		t.Fatal("invalid UTF-8 input succeeded")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = model.EmbedQuery(canceled, "協調器"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v, want context.Canceled", err)
	}
}

func TestModelHonorsSkipTokensAndRejectsZeroNorm(t *testing.T) {
	allSkipped := writeFixtureModel(t, func(manifest *modelManifest) {
		manifest.Tokenizer.SkipTokenIDs = []int{0, 1, 2, 3, 4, 5, 6, 7}
	}, nil)
	model, err := OpenVerifiedModel(allSkipped)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = model.EmbedQuery(t.Context(), "協"); !errors.Is(err, ErrNoEmbeddableTokens) {
		t.Fatalf("all-skipped error = %v, want ErrNoEmbeddableTokens", err)
	}

	zeroResume := writeFixtureModel(t, nil, func(table []byte) []byte {
		start := 5 * 4 * 4
		clear(table[start : start+4*4])
		return table
	})
	model, err = OpenVerifiedModel(zeroResume)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = model.EmbedQuery(t.Context(), "resume"); !errors.Is(err, ErrInvalidEmbedding) {
		t.Fatalf("zero-norm error = %v, want ErrInvalidEmbedding", err)
	}
}

func TestOpenVerifiedModelCanRetryAfterCorrectingExpectedIdentity(t *testing.T) {
	files := writeFixtureModel(t, nil, nil)
	trusted := files.Expected
	files.Expected.ManifestSHA256 = strings.Repeat("0", sha256.Size*2)
	if _, err := OpenVerifiedModel(files); err == nil {
		t.Fatal("OpenVerifiedModel succeeded with bad expected identity")
	}
	files.Expected = trusted
	if _, err := OpenVerifiedModel(files); err != nil {
		t.Fatalf("OpenVerifiedModel retry failed: %v", err)
	}
}

func TestWordPieceLongestMatch(t *testing.T) {
	tokenizer, err := newWordPieceTokenizer(
		[]string{"[UNK]", "play", "##ing", "player", "!"},
		tokenizerManifest{
			UNKToken:             "[UNK]",
			MaxInputCharsPerWord: 100,
			MaxInputTokens:       16,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := tokenizer.tokenize(t.Context(), "playing player!")
	if err != nil {
		t.Fatal(err)
	}
	want := []int{1, 2, 3, 4}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}
}

func TestWordPieceUnknownAndTokenLimit(t *testing.T) {
	tokenizer, err := newWordPieceTokenizer(
		[]string{"[UNK]", "a"},
		tokenizerManifest{
			UNKToken:             "[UNK]",
			MaxInputCharsPerWord: 3,
			MaxInputTokens:       2,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := tokenizer.tokenize(t.Context(), "a missing aaaa a")
	if err != nil {
		t.Fatal(err)
	}
	want := []int{1, 0}
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

func TestBasicTokenizerContract(t *testing.T) {
	tokens, err := basicTokens(t.Context(), " A\t協，B\x00 ")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A", "協", "，", "B"}
	if len(tokens) != len(want) {
		t.Fatalf("tokens = %q, want %q", tokens, want)
	}
	for i := range want {
		if tokens[i] != want[i] {
			t.Fatalf("tokens = %q, want %q", tokens, want)
		}
	}
}

func FuzzBasicTokens(f *testing.F) {
	for _, seed := range []string{"協調器 resume", "playing!", "\x00\n", "👩‍💻"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if !utf8.ValidString(value) {
			return
		}
		if _, err := basicTokens(t.Context(), value); err != nil {
			t.Fatal(err)
		}
	})
}

func FuzzWordPieceTokenizer(f *testing.F) {
	for _, seed := range []string{"協調器 resume", "playing!", "\x00\n", "👩‍💻"} {
		f.Add(seed)
	}
	tokenizer, err := newWordPieceTokenizer(
		[]string{"[UNK]", "協", "調", "器", "resume", "!"},
		tokenizerManifest{
			UNKToken:             "[UNK]",
			MaxInputCharsPerWord: 100,
			MaxInputTokens:       32,
		},
	)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, value string) {
		_, _ = tokenizer.tokenize(t.Context(), value)
	})
}

func FuzzDecodeManifest(f *testing.F) {
	f.Add([]byte(`{"schema_version":1}`))
	f.Add([]byte(`not-json`))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = decodeManifest(data)
	})
}

func FuzzDecodeEmbeddingTable(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0}, uint8(1), uint8(1))
	f.Add([]byte{0xff}, uint8(2), uint8(4))
	f.Fuzz(func(t *testing.T, data []byte, rawVocab, rawDimensions uint8) {
		vocab := int(rawVocab%8) + 1
		dimensions := int(rawDimensions%8) + 1
		_, _ = decodeEmbeddingTable(data, vocab, dimensions)
	})
}

func writeFixtureModel(
	t *testing.T,
	mutateManifest func(*modelManifest),
	mutateTable func([]byte) []byte,
) VerifiedModelFiles {
	t.Helper()
	directory := t.TempDir()
	vocab := []byte("[UNK]\n[PAD]\n協\n調\n器\nresume\n失\n敗\n")
	table := make([]byte, len(fixtureVectors)*4*4)
	index := 0
	for _, vector := range fixtureVectors {
		for _, value := range vector {
			binary.LittleEndian.PutUint32(table[index:index+4], math.Float32bits(value))
			index += 4
		}
	}
	if mutateTable != nil {
		table = mutateTable(table)
	}
	manifest := modelManifest{
		SchemaVersion:  1,
		ID:             "fixture-static-wordpiece-v1",
		Revision:       "1",
		Dimensions:     4,
		VocabSize:      len(fixtureTokens),
		DType:          "float32-le",
		Pooling:        "mean",
		Normalize:      true,
		QueryPrefix:    "",
		DocumentPrefix: "",
		Tokenizer: tokenizerManifest{
			Type:                 "bert-wordpiece",
			DoBasicTokenize:      true,
			TokenizeChineseChars: true,
			UNKToken:             "[UNK]",
			MaxInputCharsPerWord: 100,
			MaxInputTokens:       32,
		},
		Files: manifestFiles{
			Vocab: hashHex(vocab),
			Table: hashHex(table),
		},
	}
	if mutateManifest != nil {
		mutateManifest(&manifest)
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(directory, "model.json")
	vocabPath := filepath.Join(directory, "vocab.txt")
	tablePath := filepath.Join(directory, "embeddings.f32")
	for path, data := range map[string][]byte{
		manifestPath: manifestBytes,
		vocabPath:    vocab,
		tablePath:    table,
	} {
		if err = os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return VerifiedModelFiles{
		ManifestPath: manifestPath,
		VocabPath:    vocabPath,
		TablePath:    tablePath,
		Expected: ModelIdentity{
			ID:              "fixture-static-wordpiece-v1",
			Revision:        "1",
			Dimensions:      4,
			ManifestSHA256:  hashHex(manifestBytes),
			TableSHA256:     hashHex(table),
			TokenizerSHA256: hashHex(vocab),
		},
	}
}

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
