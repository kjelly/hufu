package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/golangruntime"
)

const forwardPlanDocument = "Add `internal/fixture/data.json`, declare `fixture.Future`, and link [the data](../../internal/fixture/data.json).\n"

// forwardReferenceRepo commits a plan that names a file and a declaration,
// then a later commit that adds them. It returns the plan commit and HEAD.
func forwardReferenceRepo(t *testing.T, plan string) (string, string, string) {
	t.Helper()
	repo := newFixtureRepo(t)
	writeAndCommit(t, repo, "docs/architecture/plan.md", plan, "add the plan", "2025-01-02T00:00:00Z")
	planCommit, err := git(t.Context(), repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, "internal/fixture/data.json"), "{}\n")
	writeFile(t, filepath.Join(repo, "internal/fixture/fixture.go"), "package fixture\n\nfunc Future() {}\n")
	commit(t, repo, "implement the plan", "2025-01-03T00:00:00Z")
	head, err := git(t.Context(), repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return repo, strings.TrimSpace(planCommit), strings.TrimSpace(head)
}

func documentationScopeConfig(repo, head string) Config {
	config := fixtureConfig(repo, "out")
	config.Scope = resolverScope{Kind: "last_n", Count: 1, History: "first_parent", Head: head}
	config.Routing = routingDocumentation
	return config
}

func TestDocumentationVerificationAcceptsForwardReferencesOfAHistoricalReview(t *testing.T) {
	repo, planCommit, head := forwardReferenceRepo(t, forwardPlanDocument)
	result, err := Prepare(t.Context(), documentationScopeConfig(repo, planCommit))
	if err != nil {
		t.Fatalf("Prepare of the plan commit: %v", err)
	}
	verification, ok := result.Outputs["documentation_verification"].(documentationVerification)
	if !ok || !verification.Passed || len(verification.Issues) != 0 {
		t.Fatalf("verification = %#v, want a pass", result.Outputs["documentation_verification"])
	}
	if len(verification.ForwardReferences) != 3 || verification.ReferenceTip != head || verification.ReviewedRevision != planCommit {
		t.Fatalf("forward references = %q at %q (reviewed %q), want the link, path, and symbol at %s reviewed at %s",
			verification.ForwardReferences, verification.ReferenceTip, verification.ReviewedRevision, head, planCommit)
	}
	for _, reference := range verification.ForwardReferences {
		if !strings.Contains(reference, "does not exist at "+planCommit) || !strings.HasSuffix(reference, "exists at the current commit "+head) {
			t.Fatalf("forward reference %q does not name both revisions", reference)
		}
	}
}

func TestDocumentationVerificationStillFailsAReferenceMissingEverywhere(t *testing.T) {
	repo, planCommit, _ := forwardReferenceRepo(t, forwardPlanDocument+"It also needs `internal/never/there.go`.\n")
	_, err := Prepare(t.Context(), documentationScopeConfig(repo, planCommit))
	if err == nil || !strings.Contains(err.Error(), `repository path "internal/never/there.go" does not exist`) {
		t.Fatalf("Prepare error = %v, want the reference missing at every revision", err)
	}
	if strings.Contains(err.Error(), "internal/fixture/data.json") || strings.Contains(err.Error(), "fixture.Future") {
		t.Fatalf("Prepare error = %v, want forward references kept out of the issues", err)
	}
}

func TestDocumentationVerificationWithoutALaterCommitIsUnchanged(t *testing.T) {
	repo, _, _ := forwardReferenceRepo(t, forwardPlanDocument)
	writeAndCommit(t, repo, "docs/architecture/next.md", "Planned: `internal/later/missing.go`.\n", "plan more", "2025-01-04T00:00:00Z")
	if _, err := Prepare(t.Context(), documentationScopeConfig(repo, "HEAD")); err == nil || !strings.Contains(err.Error(), "internal/later/missing.go") {
		t.Fatalf("review ending at HEAD: error = %v, want the missing path", err)
	}

	writeFile(t, filepath.Join(repo, "docs/architecture/draft.md"), "Planned: `internal/later/unwritten.go`.\n")
	config := fixtureConfig(repo, "out")
	config.Scope = resolverScope{Kind: "working_tree", History: "first_parent", Head: "HEAD"}
	config.Routing = routingDocumentation
	if _, err := Prepare(t.Context(), config); err == nil || !strings.Contains(err.Error(), "internal/later/unwritten.go") {
		t.Fatalf("working-tree review: error = %v, want the missing path", err)
	}
}

// TestEmbeddedRuntimeEmitsDocumentationVerification runs the action through
// Hufu's embedded Go interpreter, the production path. Compiled tests do not
// catch a type the interpreter encodes differently: a pointer method on
// documentationVerification once made it an empty object, so the team's
// acceptance assertion on /passed could not resolve.
func TestEmbeddedRuntimeEmitsDocumentationVerification(t *testing.T) {
	repo, planCommit, head := forwardReferenceRepo(t, forwardPlanDocument)
	scope := resolverScope{Kind: "last_n", Count: 1, History: "first_parent", Head: planCommit}
	payload, err := json.Marshal(wireConfig{
		Scope: &scope, Routing: routingDocumentation,
		MaxTotalDiffBytes: defaultMaxTotalDiffBytes, MaxTotalDiffLines: defaultMaxTotalDiffLines,
		MaxChangedPaths: defaultMaxChangedPaths, MaxWorksetItems: defaultMaxWorksetItems,
		MaxDiffBytes: 24_000, MaxDiffLines: 600, MaxPaths: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(actionRequest{Type: "prepare_review_workset", Payload: string(payload)})
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	program, err := golangruntime.Prepare(source, ".")
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "HUFU_REPOSITORY="+repo, "HUFU_WORKSPACE="+t.TempDir())
	result, err := golangruntime.Execute(t.Context(), "", program, request, env, 1<<20, 16<<10)
	if err != nil {
		t.Fatalf("execute embedded reviewprep runtime: %v; stderr=%s", err, result.Stderr)
	}
	var output struct {
		Outputs struct {
			DocumentationVerification documentationVerification `json:"documentation_verification"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal(result.Stdout, &output); err != nil {
		t.Fatalf("decode embedded runtime output: %v; stdout=%s", err, result.Stdout)
	}
	verification := output.Outputs.DocumentationVerification
	if !verification.Passed || len(verification.CheckedFiles) != 1 || len(verification.ForwardReferences) != 3 || verification.ReferenceTip != head || verification.ReviewedRevision != planCommit {
		t.Fatalf("embedded documentation_verification = %#v, want a pass with three forward references at %s", verification, head)
	}
}
