package improver

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/team"
)

func testChangeProposal(prepared Prepared, text string) paired.ChangeProposal {
	surface := prepared.Preview.Surfaces[0]
	return paired.ChangeProposal{
		SchemaVersion: 1, Hypothesis: "Explicit instructions address the observed failure.",
		ObservedFailureRefs: []string{"probe_0001"},
		Changes:             []paired.Change{{SurfaceID: surface.ID, BeforeHash: surface.BeforeHash, ReplacementText: text}},
		ExpectedBehavior:    "Return a schema-valid result satisfying the frozen verifier.",
		Applicability:       "This frozen classification fixture.",
	}
}

func TestProposalMaterializationEnforcesRegisteredSurfaces(t *testing.T) {
	prepared, err := Prepare("testdata/extraction/experiment.yaml", testBuild)
	if err != nil {
		t.Fatal(err)
	}
	inventory := prepared.Variants["baseline"].Tree.Inventory()
	proposal := testChangeProposal(prepared, "Return the allowed category exactly.\n")
	variant, err := ApplyProposal(prepared, proposal)
	if err != nil || variant.VariantDigest == prepared.Variants["baseline"].VariantDigest {
		t.Fatalf("valid proposal rejected: %v", err)
	}
	if !reflect.DeepEqual(inventory, prepared.Variants["baseline"].Tree.Inventory()) {
		t.Fatal("proposal changed the frozen baseline")
	}
	for _, tc := range []struct {
		name string
		edit func(*paired.ChangeProposal)
	}{
		{"foreign_surface", func(p *paired.ChangeProposal) { p.Changes[0].SurfaceID = "unregistered" }},
		{"base_drift", func(p *paired.ChangeProposal) { p.Changes[0].BeforeHash = strings.Repeat("b", 64) }},
		{"template", func(p *paired.ChangeProposal) { p.Changes[0].ReplacementText = "{@ .extra_tools @}" }},
		{"duplicate", func(p *paired.ChangeProposal) { p.Changes = append(p.Changes, p.Changes[0]) }},
		{"empty", func(p *paired.ChangeProposal) { p.Changes[0].ReplacementText = " " }},
		{"secret", func(p *paired.ChangeProposal) { p.Changes[0].ReplacementText = "api_key=private_credential_value" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed, err := cloneRuntimeValue(proposal)
			if err != nil {
				t.Fatal(err)
			}
			tc.edit(&changed)
			if _, err := ApplyProposal(prepared, changed); err == nil {
				t.Fatal("invalid proposal changed a variant")
			}
		})
	}
}

func TestProposalRoleSchemasAreDistinctAndCompile(t *testing.T) {
	seen := map[string]bool{}
	for _, role := range []string{"proposer", "composer", "reflector"} {
		raw, err := paired.RoleSchema(role)
		if err != nil {
			t.Fatal(err)
		}
		contract, err := team.CompileResultContractDocument(role+".json", raw)
		if err != nil {
			t.Fatal(err)
		}
		if seen[contract.SchemaSHA256] {
			t.Fatal("roles shared an unrelated payload contract")
		}
		seen[contract.SchemaSHA256] = true
	}
}

func TestProposalVerifierSurvivesNumericJSONReplay(t *testing.T) {
	authored := roleVerifier().RuntimeSpec()
	replayed, err := cloneRuntimeValue(authored)
	if err != nil || !sameVerificationSpec(&authored, &replayed) {
		t.Fatalf("equivalent numeric verifier changed after replay: %v", err)
	}
	replayed.TaskResultAssertions[0].Value = 2
	if sameVerificationSpec(&authored, &replayed) {
		t.Fatal("changed assertion value accepted")
	}
}
