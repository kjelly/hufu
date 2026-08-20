package tools

import "testing"

// TestReadOnlyBashGrammarRejectsWriteFlags covers the write-bypass class
// where a read-only (side_effect:none) command mutates the filesystem via a
// flag rather than a shell redirect (`>`). Each bypass must be rejected by
// BOTH the enforcement gate (checkReadOnlyBashCommand) and the observation
// helper (IsReadOnlyBashCommand) so neither can endorse the call.
func TestReadOnlyBashGrammarRejectsWriteFlags(t *testing.T) {
	denied := []struct {
		name    string
		command string
	}{
		{"go test -c compiles binary", "go test -c"},
		{"go test -o writes binary", "go test -o ./bin"},
		{"go test -o= writes binary", "go test -o=./bin"},
		{"go test -cover writes coverage", "go test -cover ./..."},
		{"go test -coverprofile writes file", "go test -coverprofile=cov.out ./..."},
		{"go env -w mutates config", "go env -w GOFLAGS=-v"},
		{"go env -u unsets config", "go env -u GOFLAGS"},
		{"sort -o writes file", "sort -o out.txt input.txt"},
		{"sort --output writes file", "sort --output out.txt input.txt"},
		{"sort --output= writes file", "sort --output=out.txt input.txt"},
		{"sed -i edits in place", "sed -i 's/a/b/' file"},
		{"sed -i.bak edits in place", "sed -i.bak 's/a/b/' file"},
		{"sed --in-place edits in place", "sed --in-place 's/a/b/' file"},
		{"sed --in-place= edits in place", "sed --in-place=.bak 's/a/b/' file"},
		{"find -fprint writes file", "find . -fprint out.txt"},
		{"find -fprint0 writes file", "find . -fprint0 out.txt"},
		{"find -fprintf writes file", "find . -fprintf out.txt %p"},
		{"find -fls writes file", "find . -fls out.txt"},
		{"find -delete removes", "find . -delete"},
		{"find -exec runs command", "find . -exec echo {} ;"},
		{"git status --output writes", "git status --output=out.txt"},
		{"git log --output writes", "git log --output=out.txt"},
		{"git -o global write", "git -o out.txt status"},
	}
	for _, tt := range denied {
		t.Run(tt.name, func(t *testing.T) {
			if err := checkReadOnlyBashCommand(tt.command); err == nil {
				t.Errorf("checkReadOnlyBashCommand(%q) = nil, want error", tt.command)
			}
			if IsReadOnlyBashCommand(tt.command) {
				t.Errorf("IsReadOnlyBashCommand(%q) = true, want false", tt.command)
			}
		})
	}
}

// TestReadOnlyBashGrammarPermitsInspectionPipelines asserts the tightened
// grammar still accepts the legitimate, contract-aligned inspection
// commands that read-only workers rely on (including restricted `go test`).
func TestReadOnlyBashGrammarPermitsInspectionPipelines(t *testing.T) {
	allowed := []string{
		"go test ./...",
		"go test -run TestX -v ./...",
		"go vet ./...",
		"go env GOPATH",
		"go env -json",
		"sort input.txt",
		"sed -n '1,5p' file",
		"find . -name x -print",
		"find . -name x -print0",
		"find . -printf '%p\\n'",
		"git -c core.pager=cat status",
		"git -C /repo log --oneline",
		"git diff --stat",
		"git log --oneline -5",
		"git rev-list --count abcdef..123456",
		"git rev-list --count --since=2.weeks.ago HEAD",
		// --count is not required to be the first argument: git accepts it in
		// any position, so the read-only classifier must not be more rigid
		// than git itself about where the flag appears.
		"git rev-list --since=2.weeks.ago HEAD --count",
		"git rev-list abcdef..123456 --count",
		"grep -rn needle missing-path 2>/dev/null",
	}
	for _, cmd := range allowed {
		if err := checkReadOnlyBashCommand(cmd); err != nil {
			t.Errorf("checkReadOnlyBashCommand(%q) = %v, want nil", cmd, err)
		}
		if !IsReadOnlyBashCommand(cmd) {
			t.Errorf("IsReadOnlyBashCommand(%q) = false, want true", cmd)
		}
	}
}

func TestReadOnlyBashGrammarRestrictsGitRevList(t *testing.T) {
	denied := []string{
		"git rev-list abcdef..123456",
		"git rev-list --all",
		"git rev-list --objects abcdef..123456",
		"git rev-list --count --stdin",
		"git rev-list --count --output=out.txt abcdef..123456",
		"git rev-list --count abcdef",
	}
	for _, command := range denied {
		t.Run(command, func(t *testing.T) {
			if err := checkReadOnlyBashCommand(command); err == nil {
				t.Fatalf("checkReadOnlyBashCommand(%q) = nil, want error", command)
			}
			if IsReadOnlyBashCommand(command) {
				t.Fatalf("IsReadOnlyBashCommand(%q) = true, want false", command)
			}
		})
	}
}

func TestReadOnlyBashGrammarRestrictsRedirectionToStderrNullDiscard(t *testing.T) {
	for _, command := range []string{
		"grep -rn needle missing-path >out.txt",
		"grep -rn needle missing-path 2>errors.txt",
		"grep -rn needle missing-path 2>/tmp/errors.txt",
		"grep -rn needle missing-path 1>/dev/null",
	} {
		t.Run(command, func(t *testing.T) {
			if err := checkReadOnlyBashCommand(command); err == nil {
				t.Fatalf("checkReadOnlyBashCommand(%q) = nil, want error", command)
			}
		})
	}
}
