package utils

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
)

func TestRedactSecretsPreservesStructuredEvidence(t *testing.T) {
	for _, fence := range []string{"", "```", "``````"} {
		t.Run("fence_"+fence, func(t *testing.T) {
			resetLearnedSecrets(t)
			const secret = "fenced-example-secret-76a9"
			// A transcript contains encoded tool output, which itself includes
			// source with credential assignments: the same layers as collection.
			source, err := json.Marshal(map[string]string{"source": `const password = "` + secret + `"`})
			if err != nil {
				t.Fatal(err)
			}
			tool, err := json.Marshal(map[string]string{"output": string(source)})
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(map[string]any{
				"work_transcript": string(tool) + "\n" + string(tool),
				"claims_schema":   map[string]any{"type": "object", "required": []string{"status"}},
				"findings":        []any{},
				"total_tokens":    int64(9007199254740993),
			})
			if err != nil {
				t.Fatal(err)
			}
			prefix, suffix := "", ""
			if fence != "" {
				prefix = "Frozen evidence:\n" + fence + "json\n"
				suffix = "\n" + fence + "\nPreserve the observations."
			}
			got := RedactSecrets(prefix + string(data) + suffix)
			for range 3 {
				got = RedactSecrets(got)
				body := strings.TrimSuffix(strings.TrimPrefix(got, prefix), suffix)
				var decoded struct {
					Transcript string         `json:"work_transcript"`
					Schema     map[string]any `json:"claims_schema"`
					Findings   []any          `json:"findings"`
					Tokens     int64          `json:"total_tokens"`
				}
				if err := json.Unmarshal([]byte(body), &decoded); err != nil {
					t.Fatalf("redaction corrupted the evidence JSON: %v", err)
				}
				if strings.Contains(got, secret) || !strings.Contains(got, redactedSecret) {
					t.Fatal("credential redaction was lost")
				}
				if decoded.Findings == nil || len(decoded.Findings) != 0 || decoded.Tokens != 9007199254740993 || decoded.Schema["type"] != "object" {
					t.Fatalf("redaction changed structured facts: %+v", decoded)
				}
				for line := range strings.SplitSeq(decoded.Transcript, "\n") {
					if !jsontext.Value(line).IsValid() {
						t.Fatal("redaction corrupted the encoded tool transcript")
					}
				}
			}
		})
	}
}

func TestRedactSecretsDiscoversAcrossJSONFences(t *testing.T) {
	resetLearnedSecrets(t)
	const secret = "cross-fence-secret-6a4d"
	raw := "earlier=" + secret + "\n```json\n{\"password\":\"" + secret + "\"}\n```\nafter=" + secret
	got := RedactSecrets(raw)
	if strings.Contains(got, secret) || strings.Count(got, redactedSecret) != 3 {
		t.Fatalf("credential escaped cross-block discovery: %s", got)
	}
}

func TestRedactSecretsPreservesDiagnosticMemberOrder(t *testing.T) {
	resetLearnedSecrets(t)
	const secret = "ordered-diagnostic-secret-92b6"
	prefix := "{\n  \"status\": \"success\",\n  \"files_read\": [\"unauthorized.txt\"],\n  \"details\": "
	raw := prefix + `"api_token=` + secret + " " + strings.Repeat("diagnostic ", 2000) + `"` + "\n}"
	got := RedactSecrets(raw)
	if !strings.HasPrefix(got, prefix) || !strings.Contains(got[:500], "unauthorized.txt") {
		t.Fatal("redaction moved identifying evidence out of its bounded preview")
	}
	if strings.Contains(got, secret) || !jsontext.Value(got).IsValid() {
		t.Fatal("ordered diagnostic redaction lost JSON framing or credential protection")
	}
	if repeated := RedactSecrets(got); repeated != got {
		t.Fatal("ordered diagnostic redaction is not idempotent")
	}
}

func TestRedactSecretsKeepsMalformedJSONProtected(t *testing.T) {
	for _, body := range []string{
		`{"password":"malformed-secret-48b1"`,
		`{"password":"malformed-secret-48b1","password":"malformed-secret-48b1"}`,
		`{"password":"malformed-secret-48b1"} trailing`,
	} {
		resetLearnedSecrets(t)
		got := RedactSecrets("```json\n" + body + "\n```")
		if strings.Contains(got, "malformed-secret-48b1") {
			t.Fatal("ambiguous or malformed JSON bypassed redaction")
		}
		if strings.Count(body, `"password"`) != strings.Count(got, `"password"`) {
			t.Fatal("redaction collapsed duplicate keys")
		}
	}
}

func TestRedactSecretsPreservesCrossFenceProtections(t *testing.T) {
	resetLearnedSecrets(t)
	raw := "-----BEGIN PRIVATE KEY-----\n```json\n{\"note\":\"material\"}\n```\n-----END PRIVATE KEY-----"
	if got := RedactSecrets(raw); got != redactedSecret {
		t.Fatalf("private key block crossing JSON framing escaped redaction: %s", got)
	}
	const exact = "registry-json-secret-2a5e"
	RegisterSecretRedactor(testSecretRedactor{value: exact})
	got := RedactSecrets("```json\n{\"note\":\"" + exact + "\"}\n```")
	if strings.Contains(got, exact) {
		t.Fatal("JSON framing bypassed the process redactor")
	}
}
