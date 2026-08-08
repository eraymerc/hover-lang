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
// FUNCTIONS follow the same rule, via buildFunctionScopes at the bottom of
// this file. Getting there required inverting how a function's C name is
// chosen: it now belongs to the declaration (elaborator.FunctionInfo.CName),
// not to the caller's spelling, so one declaration reached bare from one file
// and as `M.fabs` from another emits exactly one C function.
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

			// `from <path> import A, B as C;` — only the listed names enter
			// the flat namespace, each under its local spelling. A name that
			// matches nothing here may still be a function, so being absent
			// is not an error yet; buildFunctionScopes reports the names that
			// matched neither (it is the pass that can see both).
			if imp.Selective {
				for _, sym := range imp.Names {
					decl, ok := imported[sym.Name]
					if !ok {
						continue
					}
					local := sym.Local()
					if existing, exists := sc.modules[local]; exists && existing != decl {
						return fmt.Errorf("line %d: module '%s' from %s collides with an existing module '%s' (declared at line %d) — import it under a different name (`import %s as %s`)",
							imp.Token.Line, local, imp.PathString(), existing.Name, existing.Line(), sym.Name, local+"2")
					}
					sc.modules[local] = decl
				}
				continue
			}

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

// ─────────────────────────────────────────────────────────────────────────────
// PER-FILE FUNCTION SCOPES
// ─────────────────────────────────────────────────────────────────────────────

// buildFunctionScopes assigns every function declaration in the loaded set a
// unique C identifier, then builds one call-spelling→declaration table per
// file from that file's own imports. It also gathers importc headers, which
// must come from EVERY file rather than just the entry's: a library's extern
// declarations are emitted whether or not the entry file knows they exist, so
// the header that defines them has to be included regardless.
//
// Structure mirrors buildModuleScopes, and the two-pass split is load-bearing
// for the same reason: pass 1 records each file's OWN declarations, pass 2
// wires up imports reading only from that table, so imports cannot become
// accidentally transitive through map iteration order.
func (e *Elaborator) buildFunctionScopes(files map[string]*ImportedFile) error {
	// Sorted, so C names and emission order are a property of the program
	// rather than of Go's randomized map iteration. Two builds of the same
	// source must produce the same sim.cpp.
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	// Pass 0 — extern names are fixed: they name a real C function in a header
	// and cannot be renamed. Reserve them all before uniquing anything else, so
	// a Hover-level function never takes a name the linker has already promised
	// to something in a header.
	taken := make(map[string]bool)
	for _, path := range paths {
		for _, stmt := range files[path].Program.Statements {
			if f, ok := stmt.(*ast.FuncDeclStatement); ok && f.IsExtern {
				taken[f.Name] = true
			}
		}
	}

	// Pass 1 — own declarations, unique C names, and importc headers.
	own := make(map[string]map[string]*FunctionInfo, len(files))
	for _, path := range paths {
		decls := make(map[string]*FunctionInfo)
		for _, stmt := range files[path].Program.Statements {
			if ic, ok := stmt.(*ast.ImportCStatement); ok {
				e.output.CIncludes = append(e.output.CIncludes, ic.Path)
			}
			f, ok := stmt.(*ast.FuncDeclStatement)
			if !ok {
				continue
			}
			if existing, dup := decls[f.Name]; dup {
				return fmt.Errorf("function '%s' is declared twice in the same file (lines %d and %d)",
					f.Name, existing.Decl.Line(), f.Line())
			}
			info := &FunctionInfo{Decl: f, CName: uniqueCName(f, taken), File: path}
			decls[f.Name] = info
			e.output.Functions = append(e.output.Functions, info)
		}
		own[path] = decls
		e.output.FuncScopes[path] = make(map[string]*FunctionInfo)
	}

	// Pass 2 — fold each file's own declarations, then its own imports.
	for _, path := range paths {
		sc := e.output.FuncScopes[path]
		for name, info := range own[path] {
			sc[name] = info
		}

		for _, imp := range files[path].Imports {
			resolvedPath := resolveImportPathFor(path, imp.Path, imp.IsSystem)
			imported, ok := own[resolvedPath]
			if !ok {
				// buildModuleScopes runs first and reports unresolvable imports
				// with a better message; nothing to add here.
				continue
			}

			// Selective import. This pass owns the "no such name" diagnostic
			// because it is the only one that can see both namespaces: a
			// selected name is valid if EITHER a module or a function answers
			// to it, and buildModuleScopes ran without knowing about
			// functions.
			if imp.Selective {
				for _, sym := range imp.Names {
					info, ok := imported[sym.Name]
					if !ok {
						if declaresModule(files[resolvedPath], sym.Name) {
							continue // handled by buildModuleScopes
						}
						return fmt.Errorf("line %d: %s declares no module or function named '%s'%s",
							imp.Token.Line, imp.PathString(), sym.Name,
							describeExports(files[resolvedPath]))
					}
					local := sym.Local()
					if existing, exists := sc[local]; exists && existing != info {
						if existing.Decl.IsExtern && info.Decl.IsExtern {
							continue // two headers declaring the same C function — harmless
						}
						return fmt.Errorf("line %d: function '%s' from %s collides with an existing function '%s' (declared at line %d) — import it under a different name (`%s as ...`)",
							imp.Token.Line, local, imp.PathString(), existing.Decl.Name, existing.Decl.Line(), sym.Name)
					}
					sc[local] = info
				}
				continue
			}

			if imp.Alias != "" {
				// Aliased names always contain a dot, and a bare function name
				// never can, so an alias can't collide with the flat namespace.
				for name, info := range imported {
					sc[imp.Alias+"."+name] = info
				}
				continue
			}

			for name, info := range imported {
				if existing, exists := sc[name]; exists && existing != info {
					if existing.Decl.IsExtern && info.Decl.IsExtern {
						continue // two headers declaring the same C function — harmless
					}
					return fmt.Errorf("line %d: function '%s' from %q collides with an existing function '%s' (declared at line %d) — use an aliased import (`as Name`) to avoid this",
						imp.Token.Line, name, imp.Path, existing.Decl.Name, existing.Decl.Line())
				}
				sc[name] = info
			}
		}
	}

	return nil
}

// declaresModule reports whether f declares a module by that name. Used on
// the error path of a selective import, to tell "you asked for a name this
// file doesn't have" apart from "you asked for a module, which the function
// pass simply isn't the one that binds it".
func declaresModule(f *ImportedFile, name string) bool {
	if f == nil {
		return false
	}
	for _, stmt := range f.Program.Statements {
		if m, ok := stmt.(*ast.ModuleDeclStatement); ok && m.Name == name {
			return true
		}
	}
	return false
}

// describeExports lists what a file actually declares, for the error a
// selective import produces when it names something absent. Without it the
// message says only that the name is missing, leaving the user to open the
// file and read it — and a typo'd or renamed declaration is by far the most
// likely cause.
func describeExports(f *ImportedFile) string {
	if f == nil {
		return ""
	}
	var names []string
	for _, stmt := range f.Program.Statements {
		switch d := stmt.(type) {
		case *ast.ModuleDeclStatement:
			names = append(names, d.Name)
		case *ast.FuncDeclStatement:
			names = append(names, d.Name)
		}
	}
	if len(names) == 0 {
		return " — that file declares no modules or functions at all"
	}
	sort.Strings(names)
	return " — it declares: " + strings.Join(names, ", ")
}

// uniqueCName picks the C identifier a function will be emitted under,
// recording it in taken.
//
// Extern functions keep their exact name — it IS the C symbol the header
// declares, so renaming it would break the link. Everything else prefers its
// Hover name and falls back to a numbered suffix, which only happens now that
// two different files may each declare a private helper of the same name. The
// suffix is deterministic because callers walk files in sorted order.
func uniqueCName(f *ast.FuncDeclStatement, taken map[string]bool) string {
	if f.IsExtern {
		return f.Name
	}
	name := f.Name
	for n := 2; taken[name]; n++ {
		name = fmt.Sprintf("%s__%d", f.Name, n)
	}
	taken[name] = true
	return name
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
