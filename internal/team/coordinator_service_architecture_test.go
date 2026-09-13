package team

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCoordinatorServiceOwnershipArchitecture keeps the extracted services
// from silently acquiring a second state owner inside Coordinator. It also
// confines direct field access and default implementation construction to the
// composition/accessor files; production behavior must use the service seams.
func TestCoordinatorServiceOwnershipArchitecture(t *testing.T) {
	requiredCoordinatorFields := map[string]bool{
		"taskCache":        false,
		"evidenceService":  false,
		"repairController": false,
	}
	forbiddenCoordinatorFields := map[string]bool{
		"taskCacheEntries":    true,
		"taskCacheMu":         true,
		"taskCacheGeneration": true,
	}
	directFieldFiles := map[string]map[string]bool{
		"taskCache": {
			"coordinator.go":              true,
			"coordinator_extra_models.go": true,
		},
		"evidenceService": {
			"coordinator.go": true,
		},
		"repairController": {
			"coordinator.go": true,
		},
	}
	implementationFiles := map[string]map[string]bool{
		"defaultTaskCache": {
			"coordinator.go":           true,
			"coordinator_taskcache.go": true,
		},
		"defaultEvidenceService": {
			"coordinator.go":       true,
			"evidence_manifest.go": true,
		},
		"NewRepairController": {
			"coordinator.go":       true,
			"repair_controller.go": true,
			"reliability_eval.go":  true,
		},
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Clean(name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.TypeSpec:
				if node.Name.Name == "Coordinator" {
					if fields, ok := node.Type.(*ast.StructType); ok {
						for _, field := range fields.Fields.List {
							for _, fieldName := range field.Names {
								if _, required := requiredCoordinatorFields[fieldName.Name]; required {
									requiredCoordinatorFields[fieldName.Name] = true
								}
								if forbiddenCoordinatorFields[fieldName.Name] {
									t.Errorf("%s reintroduces extracted cache state field Coordinator.%s", name, fieldName.Name)
								}
							}
						}
					}
				}
			case *ast.SelectorExpr:
				if allowed, guarded := directFieldFiles[node.Sel.Name]; guarded && !allowed[name] {
					t.Errorf("%s directly accesses Coordinator.%s; use the canonical service accessor", name, node.Sel.Name)
				}
			case *ast.KeyValueExpr:
				if key, ok := node.Key.(*ast.Ident); ok {
					if allowed, guarded := directFieldFiles[key.Name]; guarded && !allowed[name] {
						t.Errorf("%s initializes Coordinator.%s outside its composition boundary", name, key.Name)
					}
				}
			case *ast.Ident:
				if allowed, guarded := implementationFiles[node.Name]; guarded && !allowed[name] {
					t.Errorf("%s references %s outside its implementation/composition boundary", name, node.Name)
				}
			}
			return true
		})
	}
	for name, found := range requiredCoordinatorFields {
		if !found {
			t.Errorf("Coordinator no longer owns canonical service field %s", name)
		}
	}
}
