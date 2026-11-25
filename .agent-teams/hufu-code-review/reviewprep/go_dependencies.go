package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func declaredGoSymbols(filename string, data []byte) map[string]struct{} {
	result := make(map[string]struct{})
	parsed, err := parser.ParseFile(token.NewFileSet(), filename, data, parser.SkipObjectResolution)
	if err != nil {
		return result
	}
	add := func(name string) {
		if name != "_" && len([]rune(name)) >= 4 {
			result[name] = struct{}{}
		}
	}
	for _, declaration := range parsed.Decls {
		switch current := declaration.(type) {
		case *ast.FuncDecl:
			if current.Recv == nil {
				add(current.Name.Name)
			} else {
				// Method names such as Error and Unwrap are shared by unrelated
				// types. Match the receiver identity instead of treating every
				// selector with the same method name as a caller of this file.
				for _, receiver := range current.Recv.List {
					typeExpr := receiver.Type
					for typeExpr != nil {
						switch value := typeExpr.(type) {
						case *ast.StarExpr:
							typeExpr = value.X
						case *ast.IndexExpr:
							typeExpr = value.X
						case *ast.IndexListExpr:
							typeExpr = value.X
						case *ast.Ident:
							add(value.Name)
							typeExpr = nil
						default:
							typeExpr = nil
						}
					}
				}
			}
		case *ast.GenDecl:
			for _, spec := range current.Specs {
				switch value := spec.(type) {
				case *ast.TypeSpec:
					add(value.Name.Name)
				case *ast.ValueSpec:
					for _, name := range value.Names {
						add(name.Name)
					}
				}
			}
		}
	}
	return result
}

func goFileIdentifiers(filename string, data []byte) map[string]struct{} {
	identifiers := make(map[string]struct{})
	parsed, err := parser.ParseFile(token.NewFileSet(), filename, data, parser.SkipObjectResolution)
	if err != nil {
		return identifiers
	}
	ast.Inspect(parsed, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if !ok {
			return true
		}
		if len([]rune(identifier.Name)) >= 4 {
			identifiers[identifier.Name] = struct{}{}
		}
		return true
	})
	return identifiers
}

func identifierSetsIntersect(first, second map[string]struct{}) bool {
	for identifier := range first {
		if _, ok := second[identifier]; ok {
			return true
		}
	}
	return false
}

type goSnapshotIndexEntry struct {
	path        string
	identifiers map[string]struct{}
	packagePath string
	imports     []string
}

type goReferenceScope struct {
	directory   string
	packagePath string
	symbols     map[string]struct{}
	methods     map[string]struct{}
}

// Identifier names are a candidate heuristic, not a typed call graph. Bind
// them to a package and its import dependents before selecting caller context:
// context.Context in an unrelated package is not a reference to this source's
// Context method. Unknown module identities retain the conservative fallback.
func (s *reviewSourceSnapshot) relatedGoPaths(primaryPaths []string) ([]string, error) {
	primary := make(map[string]bool)
	paired := make(map[string]bool)
	scopes := make(map[string]*goReferenceScope)
	for _, repositoryPath := range primaryPaths {
		repositoryPath = filepath.ToSlash(repositoryPath)
		primary[repositoryPath] = true
		if path.Ext(repositoryPath) != ".go" {
			continue
		}
		if strings.HasSuffix(repositoryPath, "_test.go") {
			paired[strings.TrimSuffix(repositoryPath, "_test.go")+".go"] = true
		} else {
			paired[strings.TrimSuffix(repositoryPath, ".go")+"_test.go"] = true
		}
		data, err := s.readRegularFile(repositoryPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		directory := path.Dir(repositoryPath)
		scope := scopes[directory]
		if scope == nil {
			scope = &goReferenceScope{directory: directory, packagePath: s.goPackagePath(directory), symbols: make(map[string]struct{}), methods: make(map[string]struct{})}
			scopes[directory] = scope
		}
		for symbol := range declaredGoSymbols(repositoryPath, data) {
			scope.symbols[symbol] = struct{}{}
		}
		for method := range declaredGoMethodNames(repositoryPath, data) {
			scope.methods[method] = struct{}{}
		}
	}
	if len(scopes) == 0 && len(paired) == 0 {
		return nil, nil
	}
	if err := s.ensureGoIndex(); err != nil {
		return nil, err
	}
	type candidate struct {
		path     string
		priority int
	}
	candidates := []candidate{}
	for _, indexed := range s.goIndex {
		if primary[indexed.path] {
			continue
		}
		priority := 4
		if paired[indexed.path] {
			priority = 0
		} else {
			matched, methodOnly := s.goReferenceMatch(indexed, scopes)
			if !matched {
				continue
			}
			_, sameDirectory := scopes[path.Dir(indexed.path)]
			switch {
			case methodOnly:
				// Method names alone do not establish receiver ownership.
				// Keep indirect-call candidates behind all symbol matches.
				priority = 5
			case sameDirectory && strings.HasSuffix(indexed.path, "_test.go"):
				priority = 1
			case sameDirectory:
				priority = 2
			case strings.HasSuffix(indexed.path, "_test.go"):
				priority = 3
			}
		}
		candidates = append(candidates, candidate{path: indexed.path, priority: priority})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority < candidates[j].priority
		}
		return candidates[i].path < candidates[j].path
	})
	result := make([]string, 0, len(candidates))
	for _, current := range candidates {
		result = append(result, current.path)
	}
	return result, nil
}

func (s *reviewSourceSnapshot) goReferenceMatch(indexed goSnapshotIndexEntry, scopes map[string]*goReferenceScope) (matched, methodOnly bool) {
	weak := false
	for _, scope := range scopes {
		local := identifierSetsIntersect(indexed.identifiers, scope.symbols)
		if path.Dir(indexed.path) == scope.directory {
			// Generic method names alone cannot identify a same-package owner.
			if local {
				return true, false
			}
			continue
		}
		if !local && !identifierSetsIntersect(indexed.identifiers, scope.methods) {
			continue
		}
		// Indirect callers may mention only a method on a returned value.
		// Retain that observation when package lineage admits the caller.
		if scope.packagePath == "" || indexed.packagePath == "" || s.goPackageDependents(scope.packagePath)[indexed.packagePath] {
			if local {
				return true, false
			}
			weak = true
		}
	}
	return weak, weak
}

func declaredGoMethodNames(filename string, data []byte) map[string]struct{} {
	result := make(map[string]struct{})
	parsed, err := parser.ParseFile(token.NewFileSet(), filename, data, parser.SkipObjectResolution)
	if err != nil {
		return result
	}
	for _, declaration := range parsed.Decls {
		method, ok := declaration.(*ast.FuncDecl)
		if ok && method.Recv != nil && len([]rune(method.Name.Name)) >= 4 {
			result[method.Name.Name] = struct{}{}
		}
	}
	return result
}

func (s *reviewSourceSnapshot) ensureGoIndex() error {
	if s.goIndexReady {
		return s.goIndexErr
	}
	s.goIndexReady = true
	s.goReverseImports = make(map[string]map[string]bool)
	s.goIndexErr = filepath.WalkDir(s.root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		relative, err := filepath.Rel(s.root, current)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		indexed := goSnapshotIndexEntry{path: relative, identifiers: goFileIdentifiers(relative, data), packagePath: s.goPackagePath(path.Dir(relative))}
		parsed, _ := parser.ParseFile(token.NewFileSet(), relative, data, parser.ImportsOnly)
		if parsed != nil {
			for _, spec := range parsed.Imports {
				imported, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					continue
				}
				indexed.imports = append(indexed.imports, imported)
				if indexed.packagePath != "" {
					if s.goReverseImports[imported] == nil {
						s.goReverseImports[imported] = make(map[string]bool)
					}
					s.goReverseImports[imported][indexed.packagePath] = true
				}
			}
		}
		s.goIndex = append(s.goIndex, indexed)
		return nil
	})
	return s.goIndexErr
}

func (s *reviewSourceSnapshot) goPackageDependents(owner string) map[string]bool {
	if s.goDependents == nil {
		s.goDependents = make(map[string]map[string]bool)
	}
	if result, ok := s.goDependents[owner]; ok {
		return result
	}
	result := map[string]bool{owner: true}
	pending := []string{owner}
	for len(pending) > 0 {
		current := pending[0]
		pending = pending[1:]
		for importer := range s.goReverseImports[current] {
			if !result[importer] {
				result[importer] = true
				pending = append(pending, importer)
			}
		}
	}
	s.goDependents[owner] = result
	return result
}

type snapshotGoTestPlan struct {
	selected    map[string]bool
	directories map[string]bool
	importers   map[string]map[string][]goSnapshotIndexEntry
}

func (s *reviewSourceSnapshot) planSnapshotGoTests(paths []string) (snapshotGoTestPlan, error) {
	selected := make(map[string]bool)
	for _, name := range paths {
		if path.Ext(name) == ".go" {
			selected[path.Dir(name)] = true
		}
	}
	if len(selected) == 0 {
		return snapshotGoTestPlan{}, nil
	}
	if err := s.ensureGoIndex(); err != nil {
		return snapshotGoTestPlan{}, err
	}
	directories := make(map[string]bool)
	for _, entry := range s.goIndex {
		dir := path.Dir(entry.path)
		if !selected[dir] || directories[dir] {
			continue
		}
		matched, err := matchesSnapshotGoBuild(s.root, entry)
		if err != nil {
			return snapshotGoTestPlan{}, err
		}
		if matched {
			directories[dir] = true
		}
	}
	importers, err := s.snapshotDirectImportCandidates(selected, directories)
	return snapshotGoTestPlan{selected: selected, directories: directories, importers: importers}, err
}

func (p *snapshotGoTestPlan) packages() []string {
	packages := make([]string, 0, len(p.directories))
	for dir := range p.directories {
		if dir != "." {
			dir = "./" + dir
		}
		packages = append(packages, dir)
	}
	sort.Strings(packages)
	return packages
}

func matchesSnapshotGoBuild(root string, entry goSnapshotIndexEntry) (bool, error) {
	buildContext := &build.Default
	for _, imported := range entry.imports {
		if imported == "C" && !buildContext.CgoEnabled {
			return false, nil
		}
	}
	dir, name := path.Split(entry.path)
	matched, err := buildContext.MatchFile(filepath.Join(root, filepath.FromSlash(dir)), name)
	if err != nil {
		return false, fmt.Errorf("evaluate Go build constraints for %q: %w", entry.path, err)
	}
	return matched, nil
}

func (s *reviewSourceSnapshot) snapshotDirectImportCandidates(selected, directories map[string]bool) (map[string]map[string][]goSnapshotIndexEntry, error) {
	// Local replacements can change a frozen module's import prefix. The
	// package suffix relative to that module is still stable.
	suffixes, primary := make(map[string]bool), make(map[string]map[string]bool)
	for dir := range selected {
		root, _ := s.goModule(dir)
		if root == "" || !directories[dir] {
			continue
		}
		if primary[root] == nil {
			primary[root] = make(map[string]bool)
		}
		if owner := s.goPackagePath(dir); owner != "" {
			primary[root][owner] = true
		}
		if dir == root {
			suffixes[""] = true
		} else {
			prefix := root + "/"
			if root == "." {
				prefix = ""
			}
			suffixes["/"+strings.TrimPrefix(dir, prefix)] = true
		}
	}
	importers := make(map[string]map[string][]goSnapshotIndexEntry)
	for _, entry := range s.goIndex {
		dir := path.Dir(entry.path)
		if selected[dir] || directories[dir] || len(entry.imports) == 0 || isExcludedTestDirectory(dir) {
			continue
		}
		module, _ := s.goModule(dir)
		if module == "" {
			continue
		}
		for _, imported := range entry.imports {
			if primary[module][imported] {
				matched, err := matchesSnapshotGoBuild(s.root, entry)
				if err != nil {
					return nil, err
				}
				if matched {
					directories[dir] = true
				}
				break
			}
			if !mayImportSelectedPackage(imported, suffixes) {
				continue
			}
			// Ignore inactive files immediately. Report a constraint error
			// only if frozen Go resolution confirms this imports the target.
			if matched, err := matchesSnapshotGoBuild(s.root, entry); err == nil && !matched {
				continue
			}
			if importers[module] == nil {
				importers[module] = make(map[string][]goSnapshotIndexEntry)
			}
			importers[module][imported] = append(importers[module][imported], entry)
		}
	}
	return importers, nil
}

func isExcludedTestDirectory(dir string) bool {
	return strings.Contains("/"+dir+"/", "/vendor/") || strings.Contains("/"+dir+"/", "/testdata/")
}

func mayImportSelectedPackage(imported string, suffixes map[string]bool) bool {
	switch imported {
	case "C", "all", "cmd", "std", "tool":
		return false
	}
	if strings.HasPrefix(imported, ".") || strings.Contains(imported, "...") {
		return false
	}
	for suffix := range suffixes {
		if len(imported) > len(suffix) && strings.HasSuffix(imported, suffix) {
			return true
		}
	}
	return false
}

// Resolve identities from the nearest frozen go.mod without executing go list
// or downloading dependencies. A missing, unreadable or malformed declaration
// is unknown, not evidence that cross-package callers do not exist.
func (s *reviewSourceSnapshot) goPackagePath(directory string) string {
	if s.goPackagePaths == nil {
		s.goPackagePaths = make(map[string]string)
	}
	if result, ok := s.goPackagePaths[directory]; ok {
		return result
	}
	result := s.resolveGoPackagePath(directory)
	s.goPackagePaths[directory] = result
	return result
}

func (s *reviewSourceSnapshot) resolveGoPackagePath(directory string) string {
	// Vendored import identities do not include the module's vendor prefix.
	segments := strings.Split(directory, "/")
	for i := len(segments) - 2; i >= 0; i-- {
		if segments[i] == "vendor" {
			return strings.Join(segments[i+1:], "/")
		}
	}
	root, module := s.goModule(directory)
	if module == "" || root == directory {
		return module
	}
	if root == "." {
		return module + "/" + directory
	}
	return module + "/" + strings.TrimPrefix(directory, root+"/")
}

func (s *reviewSourceSnapshot) goModule(directory string) (string, string) {
	for current := directory; ; current = path.Dir(current) {
		moduleFile := filepath.Join(s.root, filepath.FromSlash(current), "go.mod")
		info, err := os.Lstat(moduleFile)
		if err == nil {
			if !info.Mode().IsRegular() {
				return "", ""
			}
			data, readErr := os.ReadFile(moduleFile)
			if readErr != nil {
				return "", ""
			}
			module := goModulePath(data)
			if module == "" {
				return "", ""
			}
			return current, module
		}
		if !os.IsNotExist(err) || current == "." {
			return "", ""
		}
	}
}

func goModulePath(data []byte) string {
	module := ""
	inBlock := false
	for _, line := range strings.Split(string(data), "\n") {
		line, _, _ = strings.Cut(line, "//")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if inBlock {
			if fields[0] == ")" {
				inBlock = false
			}
			continue
		}
		if fields[0] != "module" {
			if fields[len(fields)-1] == "(" {
				inBlock = true
			}
			continue
		}
		if len(fields) != 2 || module != "" {
			return ""
		}
		value := fields[1]
		if strings.HasPrefix(value, "\"") {
			unquoted, err := strconv.Unquote(value)
			if err != nil {
				return ""
			}
			value = unquoted
		}
		if value == "" || strings.HasPrefix(value, ".") || strings.HasPrefix(value, "/") || path.Clean(value) != value || strings.ContainsAny(value, " \t\r\n\\:@\"") {
			return ""
		}
		module = value
	}
	return module
}
