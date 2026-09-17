package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStateRootForOS(t *testing.T) {
	home := t.TempDir()
	environment := func(values map[string]string) stateRootEnvironment {
		return stateRootEnvironment{
			getenv:      func(key string) string { return values[key] },
			userHomeDir: func() (string, error) { return home, nil },
		}
	}

	tests := []struct {
		name   string
		goos   string
		values map[string]string
		want   string
	}{
		{name: "linux xdg", goos: "linux", values: map[string]string{"XDG_STATE_HOME": filepath.Join(home, "xdg state")}, want: filepath.Join(home, "xdg state", "hufu")},
		{name: "linux fallback", goos: "linux", values: map[string]string{}, want: filepath.Join(home, ".local", "state", "hufu")},
		{name: "linux relative xdg falls back", goos: "linux", values: map[string]string{"XDG_STATE_HOME": "relative"}, want: filepath.Join(home, ".local", "state", "hufu")},
		{name: "macos", goos: "darwin", values: map[string]string{}, want: filepath.Join(home, "Library", "Application Support", "hufu", "state")},
		{name: "windows", goos: "windows", values: map[string]string{"LOCALAPPDATA": filepath.Join(home, "Local App Data")}, want: filepath.Join(home, "Local App Data", "hufu", "state")},
		{name: "exact override", goos: "linux", values: map[string]string{"HUFU_STATE_HOME": filepath.Join(home, " custom state ")}, want: filepath.Join(home, " custom state ")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := stateRootForOS(test.goos, environment(test.values))
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("state root = %q, want %q", got, test.want)
			}
		})
	}
}

func TestStateRootRejectsRelativeOverrideAndMissingWindowsBase(t *testing.T) {
	home := t.TempDir()
	for _, test := range []struct {
		name   string
		goos   string
		values map[string]string
	}{
		{name: "relative override", goos: "linux", values: map[string]string{"HUFU_STATE_HOME": "relative"}},
		{name: "missing local app data", goos: "windows", values: map[string]string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := stateRootForOS(test.goos, stateRootEnvironment{
				getenv:      func(key string) string { return test.values[key] },
				userHomeDir: func() (string, error) { return home, nil },
			})
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestStateRootPropagatesHomeLookupFailure(t *testing.T) {
	want := errors.New("home unavailable")
	_, err := stateRootForOS("linux", stateRootEnvironment{
		getenv:      func(string) string { return "" },
		userHomeDir: func() (string, error) { return "", want },
	})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want wrapped %v", err, want)
	}
}

func TestCanonicalPathResolvesNearestExistingAncestor(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	got, err := canonicalPath(filepath.Join(link, "missing", "child"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(real, "missing", "child")
	if got != want {
		t.Fatalf("canonical path = %q, want %q", got, want)
	}
}
