package paired

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"math"
	"strconv"
	"time"
)

// CanonicalJSON sorts object keys recursively, without rounding integer
// literals through float64. It rejects duplicate keys and invalid UTF-8.
func CanonicalJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value, json.Deterministic(true), json.WithMarshalers(json.MarshalFunc(
		func(duration time.Duration) ([]byte, error) {
			return []byte(strconv.FormatInt(int64(duration), 10)), nil
		},
	)))
	if err != nil {
		return nil, err
	}
	canonical := jsontext.Value(raw)
	if err := canonical.Format(jsontext.ReorderRawObjects(true)); err != nil {
		return nil, err
	}
	return canonical, nil
}

// ProjectionJSON is for existing runtime configuration only. Floating-point
// settings become exact decimal strings; data/schema JSON keeps its own types.
func ProjectionJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value, json.Deterministic(true), json.WithMarshalers(json.JoinMarshalers(
		json.MarshalFunc(func(duration time.Duration) ([]byte, error) {
			return []byte(strconv.FormatInt(int64(duration), 10)), nil
		}),
		json.MarshalFunc(func(value float64) ([]byte, error) {
			text, err := FloatProjection(value)
			if err != nil {
				return nil, err
			}
			return json.Marshal(text)
		}),
		json.MarshalFunc(func(value float32) ([]byte, error) {
			text, err := FloatProjection(float64(value))
			if err != nil {
				return nil, err
			}
			return json.Marshal(text)
		}),
	)))
	if err != nil {
		return nil, err
	}
	return CanonicalJSON(jsontext.Value(raw))
}

func Digest(kind string, value any) (string, error) {
	if !ValidID(kind) {
		return "", errors.New("digest: invalid domain")
	}
	raw, err := CanonicalJSON(value)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("hufu.paired." + kind + ".v1\n"))
	_, _ = hash.Write(raw)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func BytesDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// FloatProjection encodes an existing runtime floating-point setting without
// changing the original execution-policy snapshot format.
func FloatProjection(value float64) (string, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "", errors.New("projection: non-finite number")
	}
	if value == 0 {
		return "0", nil
	}
	return strconv.FormatFloat(value, 'g', -1, 64), nil
}

type ArtifactRef struct {
	Kind      string `json:"kind"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
}

func (r ArtifactRef) Validate(kind string) error {
	decoded, err := hex.DecodeString(r.Digest)
	if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != r.Digest {
		return errors.New("artifact: invalid digest")
	}
	if r.Kind != kind || !ValidID(kind) || r.SizeBytes < 0 || r.SizeBytes > MaxTotalBytes {
		return errors.New("artifact: unexpected kind or invalid size")
	}
	return nil
}
