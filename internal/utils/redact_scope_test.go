package utils

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestSecretRedactionScopePreservesEscapedAndOverlappingCredentialMasks(t *testing.T) {
	const first = "opaque-value-00283"
	const second = "value-00283<&-746"
	const quoted = `opaque-rejected-"value"-746`
	var scope SecretRedactionScope
	if err := scope.DiscoverJSON([]byte(`{"api_key":"opaque-value-00283","password":"value-00283\u003c\u0026-746","api_token":"opaque-rejected-\"value\"-746"}`)); err != nil {
		t.Fatal(err)
	}
	for i := range maxLearnedSecrets + 32 {
		learnSecretValue(fmt.Sprintf("unrelated-cache-entry-%04d", i))
	}
	const diagnostic = `{"message":"detail opaque-value-00283\u003c\u0026-746","opaque-value-00283":"metadata"}`
	got, err := scope.RedactJSONCompact([]byte(diagnostic))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	if err := json.Unmarshal(got, &fields); err != nil {
		t.Fatal(err)
	}
	if !scope.Complete() || fields["message"] != "detail [REDACTED]" || fields["[REDACTED]"] != "metadata" {
		t.Fatal("escaped or overlapping credentials were not redacted")
	}
	if redacted := scope.RedactText("echo " + first + "\n" + second); strings.Contains(redacted, first) || strings.Contains(redacted, second) {
		t.Fatal("plain text retained an operation-local credential")
	}
	if redacted := scope.RedactText(fmt.Sprintf("unknown status %q", quoted)); redacted != `unknown status "[REDACTED]"` {
		t.Fatal("protocol error retained an escaped operation-local credential")
	}
}

func TestSecretRedactionScopeMasksEnvironmentCredentialBytes(t *testing.T) {
	const raw = "\xffbare passphrase\xfe"
	const overlapping = "abcab"
	var scope SecretRedactionScope
	scope.DiscoverEnvironment([]string{"OPENAI_API_KEY=" + raw, "ANTHROPIC_API_KEY=" + overlapping})
	for _, diagnostic := range []string{
		"value: " + raw,
		fmt.Sprintf("value: \"%s\"", overlapping+"cab"),
		fmt.Sprintf("value: %q", raw),
	} {
		redacted := scope.RedactText(diagnostic)
		if redacted != "value: [REDACTED]" && redacted != `value: "[REDACTED]"` {
			t.Fatal("environment credential was not fully redacted")
		}
	}
}

func TestSecretRedactionScopeMasksMatchingJSONScalars(t *testing.T) {
	var scope SecretRedactionScope
	scope.DiscoverEnvironment([]string{"TEST_TOKEN=49186278312", "TEST_API_KEY=true", "TEST_PASSWORD=null"})
	got, err := scope.RedactJSONCompact([]byte(`{"samples":[49186278312,true,null,47,false]}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"samples":["[REDACTED]","[REDACTED]","[REDACTED]",47,false]}` {
		t.Fatal("scalar redaction changed unrelated values or leaked a credential")
	}
}
