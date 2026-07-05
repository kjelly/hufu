package team

import (
	"context"
	"strings"
	"testing"
)

func TestParseSkepticVote(t *testing.T) {
	cases := []struct {
		name     string
		response string
		want     skepticVote
		wantErr  bool
	}{
		{name: "plain refuted", response: `{"refuted": true, "reason": "file missing"}`, want: skepticVote{Refuted: true, Reason: "file missing"}},
		{name: "plain confirmed", response: `{"refuted": false, "reason": ""}`, want: skepticVote{}},
		{name: "fenced", response: "```json\n{\"refuted\": true, \"reason\": \"r\"}\n```", want: skepticVote{Refuted: true, Reason: "r"}},
		{name: "malformed", response: "I think it is fine", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSkepticVote(tc.response)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestTallySkepticVotes(t *testing.T) {
	cases := []struct {
		name        string
		votes       []skepticVote
		wantRefuted bool
	}{
		{name: "single refute", votes: []skepticVote{{Refuted: true, Reason: "bad"}}, wantRefuted: true},
		{name: "2-1 refuted", votes: []skepticVote{{Refuted: true, Reason: "a"}, {Refuted: true, Reason: "b"}, {}}, wantRefuted: true},
		{name: "1-2 confirmed", votes: []skepticVote{{Refuted: true, Reason: "a"}, {}, {}}, wantRefuted: false},
		{name: "tie confirms", votes: []skepticVote{{Refuted: true}, {}}, wantRefuted: false},
		{name: "all abstain confirms", votes: []skepticVote{{}, {}, {}}, wantRefuted: false},
		{name: "empty confirms", votes: nil, wantRefuted: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refuted, reason := tallySkepticVotes(tc.votes)
			if refuted != tc.wantRefuted {
				t.Errorf("refuted = %v, want %v", refuted, tc.wantRefuted)
			}
			if refuted && reason == "" {
				t.Errorf("refutation must carry a reason")
			}
		})
	}
}

func TestSkepticLenses(t *testing.T) {
	for n, wantLen := range map[int]int{0: 1, 1: 1, 2: 2, 3: 3, 5: 3} {
		lenses := skepticLenses(n)
		if len(lenses) != wantLen {
			t.Errorf("skepticLenses(%d) has %d lenses, want %d", n, len(lenses), wantLen)
		}
		seen := make(map[string]bool)
		for _, l := range lenses {
			if seen[l] {
				t.Errorf("skepticLenses(%d) repeats lens %q", n, l)
			}
			seen[l] = true
		}
	}
}

func TestBuildSkepticPrompt(t *testing.T) {
	prompt := buildSkepticPrompt("correctness", "the goal", "the constraints", "the output", "go test ./...")
	for _, want := range []string{"correctness", "the goal", "the constraints", "the output", "go test ./...", "refuted"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}

	// Optional sections are omitted when empty.
	prompt = buildSkepticPrompt("completeness", "g", "", "o", "")
	if strings.Contains(prompt, "## Constraints") || strings.Contains(prompt, "Objective check") {
		t.Errorf("empty sections should be omitted:\n%s", prompt)
	}
}

func TestAdversarialVerifySkipsWithoutSidecar(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{},
	}
	task := TaskDef{Agent: "a", Goal: "g", AdversarialVerify: 3}
	if err := c.adversarialVerify(context.Background(), task, "output"); err != nil {
		t.Errorf("expected silent skip without sidecar, got %v", err)
	}

	// Disabled tasks are a no-op regardless of sidecar availability.
	if err := c.adversarialVerify(context.Background(), TaskDef{Agent: "a", Goal: "g"}, "output"); err != nil {
		t.Errorf("expected no-op when disabled, got %v", err)
	}
}
