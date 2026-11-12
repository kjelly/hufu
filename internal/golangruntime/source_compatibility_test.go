package golangruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestPrepareRejectsMixedParallelFieldCalls(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		reject     bool
	}{
		{"interpreted call", `row.Status,row.SHA,row.Bytes="present",digest([]byte("hash")),13`, true},
		{"native call", `row.Status,row.SHA,row.Bytes="present",strings.TrimSpace(" hash "),13`, true},
		{"nested call", `row.Status,row.SHA,row.Bytes="present",string(digestBytes()),13`, true},
		{"one field", `value:="";row.Status,value="present",digest([]byte("hash"));row.SHA=value;row.Bytes=13`, true},
		{"literal", `row.Status,row.SHA,row.Bytes="present","hash",13`, false},
		{"conversion and builtin", `row.Status,row.SHA,row.Bytes="present",string([]byte("hash")),int64(len([]byte("thirteenbytes")))`, false},
		{"imported conversion", `row.Status,row.SHA,row.Bytes="present","hash",int64(time.Duration(13))`, false},
		{"locals", `status,sha,size:="present",digest([]byte("hash")),int64(13);row.Status=status;row.SHA=sha;row.Bytes=size`, false},
		{"sequential", `row.Status="present";row.SHA=digest([]byte("hash"));row.Bytes=13`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			source := `package main
import("context";"encoding/json";"io";"strings";"time")
type record struct{Status string;SHA string;Bytes int64}
func digest(data []byte)string{return string(data)}
func digestBytes()[]byte{return []byte("hash")}
func Run(_ context.Context,_ io.Reader,out io.Writer)error{_=strings.TrimSpace("");_=time.Second;row:=record{}; BODY;return json.NewEncoder(out).Encode(row)}
`
			writeSource(t, dir, strings.Replace(source, "BODY", tc.body, 1))
			program, err := Prepare(dir, ".")
			if tc.reject {
				if !errors.Is(err, ErrUnsafeParallelFieldCall) {
					t.Fatalf("preflight=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err := Execute(t.Context(), "", program, nil, os.Environ(), 4096, 4096)
			if err != nil {
				t.Fatalf("execute=%v stderr=%s", err, result.Stderr)
			}
			var record struct {
				Status, SHA string
				Bytes       int64
			}
			if err = json.Unmarshal(result.Stdout, &record); err != nil || record.Status != "present" || record.SHA != "hash" || record.Bytes != 13 {
				t.Fatalf("corrupt record=%s err=%v", result.Stdout, err)
			}
		})
	}
}

func TestPrepareChecksCallsAcrossSourceFiles(t *testing.T) {
	dir := t.TempDir()
	writeSource(t, dir, `package main
import("context";"io")
type record struct{First,Second string}
func Run(_ context.Context,_ io.Reader,_ io.Writer)error{r:=record{};r.First,r.Second="first",helper();return nil}
`)
	if err := os.WriteFile(dir+"/helper.go", []byte("package main\nfunc helper() string{return \"second\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Prepare(dir, ".")
	if !errors.Is(err, ErrUnsafeParallelFieldCall) {
		t.Fatalf("cross-file preflight=%v", err)
	}
}

func TestChildRejectsUnsafeAssignmentsBeforeExecution(t *testing.T) {
	dir := t.TempDir()
	marker := dir + "/side-effect"
	source := fmt.Sprintf(`package main
import("context";"io";"os";"strings")
type record struct{First,Second string}
func Run(_ context.Context,_ io.Reader,_ io.Writer)error{r:=record{};r.First,r.Second="first",strings.TrimSpace(" second ");return os.WriteFile(%q,[]byte(r.First),0600)}
`, marker)
	writeSource(t, dir, source)
	result, err := Execute(t.Context(), "", Program{Source: dir, Digest: "sha256:legacy-program"}, nil, os.Environ(), 4096, 4096)
	if err == nil || !strings.Contains(string(result.Stderr), ErrUnsafeParallelFieldCall.Error()) {
		t.Fatalf("child preflight=%v stderr=%s", err, result.Stderr)
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("unsafe child executed: %v", err)
	}
}
