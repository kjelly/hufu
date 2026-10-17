package main

import (
	"path/filepath"
	"strings"
	"testing"
)

const selectorFixtureSource = `package team

type TeamSession struct {
	MCPServers map[string]string
	Limits     struct{ MaxInvocations int }
}

type ActionProvider interface {
	Validate() error
}

type Coordinator struct{}

func (c *Coordinator) AuthorizeMCPCall() {}

type Hook func(Param int)

type Topic struct{}
`

func TestReviewSymbolExistsResolvesVariableSelectorsByMember(t *testing.T) {
	repo := newFixtureRepo(t)
	writeAndCommit(t, repo, "internal/team/team.go", selectorFixtureSource, "add team declarations", "2025-01-02T00:00:00Z")
	revision, err := git(t.Context(), repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	revision = strings.TrimSpace(revision)
	tests := []struct {
		symbol string
		want   bool
	}{
		{symbol: "session.MCPServers", want: true},
		{symbol: "provider.Validate", want: true},
		{symbol: "c.AuthorizeMCPCall", want: true},
		{symbol: "limits.MaxInvocations", want: true},
		{symbol: "c.Missing"},
		// A top-level type or a parameter name is not a selector member.
		{symbol: "x.Topic"},
		{symbol: "x.Param"},
		// A qualifier that names a package keeps the package-qualified rule.
		{symbol: "team.MCPServers"},
	}
	for _, rangeValue := range []reviewRange{{End: revision}, {Source: "working_tree"}} {
		for _, test := range tests {
			found, err := reviewSymbolExists(t.Context(), repo, rangeValue, test.symbol)
			if err != nil || found != test.want {
				t.Fatalf("reviewSymbolExists(%q, working tree %v) = %t, %v; want %t", test.symbol, rangeValue.isWorkingTree(), found, err, test.want)
			}
		}
	}
}

func TestPrepareDocumentationVerificationAcceptsVariableSelectors(t *testing.T) {
	repo := newFixtureRepo(t)
	writeFile(t, filepath.Join(repo, "internal/team/team.go"), selectorFixtureSource)
	writeFile(t, filepath.Join(repo, "docs/architecture/team.md"), "`session.MCPServers` `provider.Validate` `c.AuthorizeMCPCall`\n")
	commit(t, repo, "document team selectors", "2025-01-02T00:00:00Z")
	config := fixtureConfig(repo, "out")
	config.Routing = routingDocumentation
	result, err := Prepare(t.Context(), config)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	verification, ok := result.Outputs["documentation_verification"].(documentationVerification)
	if !ok || !verification.Passed || verification.CheckedSymbols != 3 {
		t.Fatalf("documentation verification = %#v, want three resolved selectors", result.Outputs["documentation_verification"])
	}

	missing := newFixtureRepo(t)
	writeFile(t, filepath.Join(missing, "internal/team/team.go"), selectorFixtureSource)
	writeFile(t, filepath.Join(missing, "docs/architecture/team.md"), "`c.MissingMethod`\n")
	commit(t, missing, "document a missing selector", "2025-01-02T00:00:00Z")
	config = fixtureConfig(missing, "out")
	config.Routing = routingDocumentation
	result, err = Prepare(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	verification = result.Outputs["documentation_verification"].(documentationVerification)
	if !verification.Passed || len(verification.ReferenceCandidates) != 1 || verification.ReferenceCandidates[0].Target != "c.MissingMethod" || verification.ReferenceCandidates[0].Type != "go_symbol" {
		t.Fatalf("verification = %#v, want the missing selector classified by the reviewer", verification)
	}
}
