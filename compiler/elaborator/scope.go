package elaborator

import (
	"fmt"
	"hover/compiler/ast"
	"path/filepath"
	"sort"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// PER-FILE MODULE SCOPES
//
// Every source file resolves module references against ITS OWN import list,
// the way Go resolves identifiers against the importing file's import block.
//
// This replaces an earlier model in which one flat namespace was built solely
// from the ENTRY file's imports and every file resolved against it. That model
// had two consequences worth remembering, since both were reported as bugs:
//
//  1. A non-entry file's `import` statements were completely inert. A library
//     could not declare its own dependency — standard_library/optoelectronics/
//     leds/led_colors.hvr imported led.hvr on line 1 and still failed with
//     "undeclared module 'LED'", because only the entry file's imports were
//     ever consulted. Library correctness depended on the consumer's import
//     list, and the error surfaced at a line number inside a file the user had
//     never opened.
//  2. `as` aliases were unusable inside a library, since a library cannot
//     control what names the entry file chooses. The feature only worked in
//     one file.
//
// Non-transitivity is preserved and deliberate: when file A imports file B, A
// sees B's OWN declarations, never B's imports. That's the Go rule too, and
// it's why buildModuleScopes below reads exclusively from the per-file "own
// declarations" table rather than from another file's finished scope.
//
// STAGE 1 SCOPE: modules only. Functions (e.output.Functions /
// AliasedFunctions) and importc headers are still gathered from the entry
// file's imports alone, unchanged — moving those requires threading the
// declaring file through LogicObject into codegen, and inverting codegen's
// alias-based function mangling so that one declaration reached through two
// different aliases emits ONE C function rather than two.
// ─────────────────────────────────────────────────────────────────────────────

// fileScope is one source file's view of the module namespace: what it
// declares itself, plus what its own imports bring in.
type fileScope struct {
	path string // absolute source path, for diagnostics ("" for the no-import case)

	// modules holds this file's own declarations plus every bare import's
	// declarations, sharing one flat namespace — a bare import is a request to
	// merge, so collisions within it are errors rather than silent shadowing.
	modules map[string]*ast.ModuleDeclStatement

	// aliased maps an import alias to that imported file's own module
	// declarations, reachable only as Alias.Name. Each alias is isolated, so
	// two files aliasing the same target under different names never interact
	// — which is the whole point of per-file scoping.
	aliased map[string]map[string]*ast.ModuleDeclStatement
}

func newFileScope(path string) *fileScope {
	return &fileScope{
		path:    path,
		modules: make(map[string]*ast.ModuleDeclStatement),
		aliased: make(map[string]map[string]*ast.ModuleDeclStatement),
	}
}

// resolveModule looks up a possibly-aliased module name like "Motor.PMSM" in
// this file's scope. A name with no dot is a plain lookup in the flat
// (own + bare-imported) namespace; otherwise the part before the first dot is
// an import alias.
//
// Only ONE dot is meaningful: imports are non-transitive, so a module name is
// always either bare ("PMSM") or exactly one alias segment plus the real name
// ("Motor.PMSM"), never deeper.
func (s *fileScope) resolveModule(name string) (*ast.ModuleDeclStatement, bool) {
	alias, bare, isQualified := splitQualifiedName(name)
	if !isQualified {
		decl, ok := s.modules[name]
		return decl, ok
	}
	byName, ok := s.aliased[alias]
	if !ok {
		return nil, false
	}
	decl, ok := byName[bare]
	return decl, ok
}

// buildModuleScopes constructs one fileScope per loaded file.
//
// Two passes, and the split is load-bearing rather than stylistic: pass 1
// records each file's OWN declarations, and pass 2 wires up imports reading
// only from that table. A single pass would let one file's scope be populated
// from another file's already-import-mutated scope, which would silently make
// imports transitive depending on map iteration order.
func (e *Elaborator) buildModuleScopes(files map[string]*ImportedFile) error {
	own := make(map[string]map[string]*ast.ModuleDeclStatement, len(files))

	// Pass 1 — each file's own module declarations.
	for path, f := range files {
		decls := make(map[string]*ast.ModuleDeclStatement)
		for _, stmt := range f.Program.Statements {
			m, ok := stmt.(*ast.ModuleDeclStatement)
			if !ok {
				continue
			}
			decls[m.Name] = m
			e.declFile[m] = path
		}
		own[path] = decls
		e.scopes[path] = newFileScope(path)
	}

	// Pass 2 — fold each file's own declarations, then its own imports, into
	// its scope.
	for path, f := range files {
		sc := e.scopes[path]
		for name, decl := range own[path] {
			sc.modules[name] = decl
		}

		for _, imp := range f.Imports {
			resolvedPath := resolveImportPathFor(path, imp.Path, imp.IsSystem)
			if _, ok := files[resolvedPath]; !ok {
				return fmt.Errorf("line %d: import %q could not be resolved (looked for %s) — was it loaded?",
					imp.Token.Line, imp.Path, resolvedPath)
			}
			imported := own[resolvedPath] // OWN decls only — imports are non-transitive

			if imp.Alias != "" {
				if sc.aliased[imp.Alias] == nil {
					sc.aliased[imp.Alias] = make(map[string]*ast.ModuleDeclStatement)
				}
				for name, decl := range imported {
					sc.aliased[imp.Alias][name] = decl
				}
				continue
			}

			for name, decl := range imported {
				if existing, exists := sc.modules[name]; exists && existing != decl {
					// existing == decl is the diamond case: the same
					// declaration reached through two import paths, which is
					// fine. Only genuinely different declarations collide.
					return fmt.Errorf("line %d: module '%s' from %q collides with an existing module '%s' (declared at line %d) — use an aliased import (`as Name`) to avoid this",
						imp.Token.Line, name, imp.Path, existing.Name, existing.Line())
				}
				sc.modules[name] = decl
			}
		}
	}

	return nil
}

// scopeFor returns the scope of the file that declared mod — the namespace its
// body's module references must resolve against. Falls back to the entry
// scope for a module with no recorded origin (the single-file New() path, and
// any module synthesized rather than parsed).
func (e *Elaborator) scopeFor(mod *ast.ModuleDeclStatement) *fileScope {
	if path, ok := e.declFile[mod]; ok {
		if sc, ok := e.scopes[path]; ok {
			return sc
		}
	}
	return e.entryScope
}

// describeScope renders what a scope can actually see, for an
// undeclared-module error.
//
// This exists because the failure it explains is genuinely confusing: the
// reference is in one file, the module the user expected is in another, and
// the reason they don't connect is a missing import in a third. Listing the
// visible names and the file they're visible from turns "undeclared module
// 'LED'" into something actionable.
func describeScope(s *fileScope) string {
	if s == nil {
		return ""
	}
	var names []string
	for n := range s.modules {
		names = append(names, n)
	}
	for alias, byName := range s.aliased {
		for n := range byName {
			names = append(names, alias+"."+n)
		}
	}
	sort.Strings(names)

	where := ""
	if s.path != "" {
		where = " in " + filepath.Base(s.path)
	}
	if len(names) == 0 {
		return fmt.Sprintf(" — no modules are visible%s; add an import for the file that declares it", where)
	}
	return fmt.Sprintf(" — modules visible%s: %s (add an import if the one you want is missing)",
		where, strings.Join(names, ", "))
}

// allModules returns every module declaration across every loaded file,
// deduplicated by pointer identity (a file reached through two import paths is
// loaded once, so its declarations are the same objects). Used by the macro
// passes, which must rewrite every analog module regardless of which file
// declared it or whether anything imports it.
func (e *Elaborator) allModules() []*ast.ModuleDeclStatement {
	mods := make([]*ast.ModuleDeclStatement, 0, len(e.declFile))
	for m := range e.declFile {
		mods = append(mods, m)
	}
	return mods
}
