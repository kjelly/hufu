package golangruntime

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path"
	"reflect"
	"strconv"

	"github.com/traefik/yaegi/stdlib"
)

// ErrUnsafeParallelFieldCall marks valid Go syntax that this interpreter cannot
// execute faithfully. Fail before action/model side effects instead of silently
// publishing partially assigned records.
var ErrUnsafeParallelFieldCall = errors.New("unsupported_parallel_field_call")

type sourceSyntax struct {
	name      string
	file      *ast.File
	positions *token.FileSet
}

func validateSourceAssignments(files []sourceSyntax) error {
	namedTypes := make(map[string]bool)
	for _, source := range files {
		for _, decl := range source.file.Decls {
			group, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range group.Specs {
				if declared, ok := spec.(*ast.TypeSpec); ok {
					namedTypes[declared.Name.Name] = true
				}
			}
		}
	}
	for _, source := range files {
		var failure error
		ast.Inspect(source.file, func(node ast.Node) bool {
			if failure != nil {
				return false
			}
			assignment, ok := node.(*ast.AssignStmt)
			if !ok || len(assignment.Lhs) < 2 || len(assignment.Rhs) < 2 {
				return true
			}
			fieldDestination := false
			for _, lhs := range assignment.Lhs {
				if _, ok := lhs.(*ast.SelectorExpr); ok {
					fieldDestination = true
				}
			}
			if !fieldDestination {
				return true
			}
			for _, rhs := range assignment.Rhs {
				if hasEagerFunctionCall(rhs, source.file, namedTypes) {
					failure = fmt.Errorf("%w: %s:%d: capture RHS call results in local variables before assigning fields in parallel", ErrUnsafeParallelFieldCall, source.name, source.positions.Position(assignment.Pos()).Line)
					return false
				}
			}
			return true
		})
		if failure != nil {
			return failure
		}
	}
	return nil
}

func hasEagerFunctionCall(expression ast.Expr, file *ast.File, namedTypes map[string]bool) bool {
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		if found {
			return false
		}
		// A function value's body is not evaluated during this assignment. An
		// immediately invoked literal is detected at its enclosing CallExpr.
		if _, ok := node.(*ast.FuncLit); ok {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if ok && !isConversionOrBuiltin(call.Fun, file, namedTypes) {
			found = true
			return false
		}
		return true
	})
	return found
}

func isConversionOrBuiltin(expression ast.Expr, file *ast.File, namedTypes map[string]bool) bool {
	switch value := expression.(type) {
	case *ast.ParenExpr:
		return isConversionOrBuiltin(value.X, file, namedTypes)
	case *ast.StarExpr:
		return isConversionOrBuiltin(value.X, file, namedTypes)
	case *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType, *ast.StructType:
		return true
	case *ast.Ident:
		if value.Obj != nil {
			return value.Obj.Kind == ast.Typ
		}
		if namedTypes[value.Name] {
			return true
		}
		switch types.Universe.Lookup(value.Name).(type) {
		case *types.TypeName, *types.Builtin:
			return true
		}
		for _, imported := range file.Imports {
			if imported.Name != nil && imported.Name.Name == "." && importedTypeSymbol(imported, value.Name) {
				return true
			}
		}
	case *ast.SelectorExpr:
		qualifier, ok := value.X.(*ast.Ident)
		if !ok || (qualifier.Obj != nil && qualifier.Obj.Kind != ast.Pkg) {
			return false
		}
		for _, imported := range file.Imports {
			importPath, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				continue
			}
			name := path.Base(importPath)
			if imported.Name != nil {
				name = imported.Name.Name
			}
			if name == qualifier.Name && importedTypeSymbol(imported, value.Sel.Name) {
				return true
			}
		}
	}
	return false
}

func importedTypeSymbol(imported *ast.ImportSpec, name string) bool {
	importPath, err := strconv.Unquote(imported.Path.Value)
	if err != nil {
		return false
	}
	key := importPath + "/" + path.Base(importPath)
	symbols := stdlib.Symbols[key]
	if shared, ok := actionStateSymbols[key]; ok {
		symbols = shared
	}
	value, ok := symbols[name]
	// Yaegi's binary export contract represents named types as nil *T values.
	return ok && value.IsValid() && value.Kind() == reflect.Pointer && value.IsNil()
}
