package team

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/sidecar"
)

func TestParseSkepticVote(t *testing.T) {
	cases := []struct {
		name     string
		response string
		want     skepticVote
		wantErr  bool
	}{
		{name: "plain refuted", response: `{"refuted": true, "reason": "file missing"}`, want: skepticVote{Refuted: true, Reason: "file missing", status: skepticVoteCompleted}},
		{name: "plain confirmed", response: `{"refuted": false, "reason": ""}`, want: skepticVote{status: skepticVoteCompleted}},
		{name: "fenced", response: "```json\n{\"refuted\": true, \"reason\": \"r\"}\n```", want: skepticVote{Refuted: true, Reason: "r", status: skepticVoteCompleted}},
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
		name            string
		votes           []skepticVote
		wantRefuted     bool
		wantSignal      string
		wantAbstentions int
	}{
		{name: "single refute", votes: []skepticVote{{Refuted: true, Reason: "bad", status: skepticVoteCompleted}}, wantRefuted: true, wantSignal: "refuted"},
		{name: "2-1 refuted", votes: []skepticVote{{Refuted: true, Reason: "a", status: skepticVoteCompleted}, {Refuted: true, Reason: "b", status: skepticVoteCompleted}, {}}, wantRefuted: true, wantSignal: "refuted", wantAbstentions: 1},
		{name: "1-2 confirmed", votes: []skepticVote{{Refuted: true, Reason: "a", status: skepticVoteCompleted}, {status: skepticVoteCompleted}, {status: skepticVoteCompleted}}, wantSignal: "confirmed"},
		{name: "tie confirms", votes: []skepticVote{{Refuted: true, status: skepticVoteCompleted}, {status: skepticVoteCompleted}}, wantSignal: "confirmed"},
		{name: "abstention is not confirmation", votes: []skepticVote{{status: skepticVoteCompleted}, {}}, wantSignal: "confirmed_with_abstentions", wantAbstentions: 1},
		{name: "all abstain", votes: []skepticVote{{}, {}, {}}, wantSignal: "abstained", wantAbstentions: 3},
		{name: "empty abstains", votes: nil, wantSignal: "abstained"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tally := tallySkepticVotes(tc.votes)
			if tally.Refuted != tc.wantRefuted {
				t.Errorf("refuted = %v, want %v", tally.Refuted, tc.wantRefuted)
			}
			if tally.Refuted && tally.Reason == "" {
				t.Errorf("refutation must carry a reason")
			}
			if tally.Signal != tc.wantSignal {
				t.Errorf("signal = %q, want %q", tally.Signal, tc.wantSignal)
			}
			if tally.Abstentions != tc.wantAbstentions {
				t.Errorf("abstentions = %d, want %d", tally.Abstentions, tc.wantAbstentions)
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
	prompt, err := buildSkepticPrompt("correctness", "the goal", "the constraints", "the output", "go test ./...")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"correctness", "the goal", "the constraints", "the output", "go test ./...", "refuted"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}

	// Optional sections are omitted when empty.
	prompt, err = buildSkepticPrompt("completeness", "g", "", "o", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "## Constraints") || strings.Contains(prompt, "Objective check") {
		t.Errorf("empty sections should be omitted:\n%s", prompt)
	}
}

func TestBuildSkepticPromptBoundsLargeResultWithoutDroppingContract(t *testing.T) {
	prompt, err := buildSkepticPrompt(
		"reproducibility",
		strings.Repeat("goal", 1000),
		strings.Repeat("constraint", 1000),
		strings.Repeat("output", 3000),
		strings.Repeat("verify", 500),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := utf8.RuneCountInString(prompt), sidecar.ClassifierProfile.InputRuneLimit(); got > want {
		t.Fatalf("skeptic prompt = %d runes, want at most %d", got, want)
	}
	for _, want := range []string{"## Goal", "## Constraints", "## Objective check already passed", "## Claimed result", "runes omitted", "Respond with STRICT JSON"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("bounded skeptic prompt missing %q", want)
		}
	}
}

func TestAdversarialVerifySkipsWithoutSidecar(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{},
	}
	task := TaskDef{Agent: "a", Goal: "g", AdversarialVerify: 3}
	if err := c.adversarialVerify(context.Background(), task, "task-1", "output"); err != nil {
		t.Errorf("expected silent skip without sidecar, got %v", err)
	}

	// Disabled tasks are a no-op regardless of sidecar availability.
	if err := c.adversarialVerify(context.Background(), TaskDef{Agent: "a", Goal: "g"}, "task-1", "output"); err != nil {
		t.Errorf("expected no-op when disabled, got %v", err)
	}
}
