package golangruntime

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"reflect"
	"runtime"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

// NormalizerSupported reports whether the child can impose its hard memory limit.
func NormalizerSupported() bool { return runtime.GOOS == "linux" }

func normalizerSymbols() interp.Exports {
	result := make(interp.Exports)
	for _, name := range []string{
		"bytes", "context", "encoding/base64", "encoding/hex", "encoding/json",
		"errors", "fmt", "go/ast", "go/token", "io", "math", "path", "regexp",
		"sort", "strconv", "strings", "unicode", "unicode/utf8",
	} {
		key := name + "/" + path.Base(name)
		result[key] = stdlib.Symbols[key]
	}
	result["go/parser/parser"] = make(map[string]reflect.Value)
	for name, value := range stdlib.Symbols["go/parser/parser"] {
		switch name {
		case "ParseDir", "ParseFile":
		default:
			result["go/parser/parser"][name] = value
		}
	}
	result["go/parser/parser"]["ParseFile"] = reflect.ValueOf(parseNormalizerSource)
	return result
}

func parseNormalizerSource(set *token.FileSet, name string, src any, mode parser.Mode) (*ast.File, error) {
	if src == nil {
		return nil, errors.New("result normalizers must pass source bytes to ParseFile")
	}
	return parser.ParseFile(set, name, src, mode)
}
