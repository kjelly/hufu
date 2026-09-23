package versionstore

import "testing"

func TestHufuignoreMatching(t *testing.T) {
	matcher, err := parseHufuignore([]byte(`
# comments and blank lines are ignored

node_modules/
*.log
/build
/docs/generated/
vendor/cache
`))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{rel: "node_modules", isDir: true, want: true},
		{rel: "web/node_modules/pkg/index.js", want: true},
		{rel: "node_modules", isDir: false, want: false},
		{rel: "app.log", want: true},
		{rel: "deep/dir/trace.log", want: true},
		{rel: "log.txt", want: false},
		{rel: "build", isDir: true, want: true},
		{rel: "build/out.bin", want: true},
		{rel: "src/build/out.bin", want: false},
		{rel: "docs/generated", isDir: true, want: true},
		{rel: "docs/generated/api.md", want: true},
		{rel: "docs/generated", isDir: false, want: false},
		{rel: "vendor/cache/x", want: true},
		{rel: "other/vendor/cache/x", want: false},
		{rel: "keep.go", want: false},
	}
	for _, tt := range tests {
		if got := matcher.matches(tt.rel, tt.isDir); got != tt.want {
			t.Errorf("matches(%q, dir=%v) = %v, want %v", tt.rel, tt.isDir, got, tt.want)
		}
	}
	var nilMatcher *hufuIgnore
	if nilMatcher.matches("anything", false) {
		t.Fatal("nil matcher matched")
	}
}

func TestHufuignoreRejectsUnsupportedSyntax(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "negation", data: "*.log\n!keep.log\n", want: ".hufuignore line 2: negation"},
		{name: "double star", data: "**/tmp\n", want: ".hufuignore line 1: '**'"},
		{name: "bad glob", data: "\n[\n", want: ".hufuignore line 2: invalid pattern"},
		{name: "root only", data: "/\n", want: ".hufuignore line 1: empty or malformed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseHufuignore([]byte(tt.data))
			if err == nil || len(err.Error()) < len(tt.want) || err.Error()[:len(tt.want)] != tt.want {
				t.Fatalf("err = %v, want prefix %q", err, tt.want)
			}
		})
	}
}
