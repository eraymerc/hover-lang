package elaborator

import (
	"fmt"
	"hover/compiler/ast"
	"hover/compiler/loader"
	"path/filepath"
	"sort"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// SCOPES
//
// Two groupings, and they are deliberately different:
//
//   - A FILE is the unit of parsing. Errors keep an accurate file and line,
//     and each file resolves references against ITS OWN import list, the way
//     Go resolves identifiers against the importing file's import block.
//
//   - A DIRECTORY is the unit of naming. Every .hvr file directly inside one
//     shares a single namespace, so an import names a directory and a library
//     no longer imports its own siblings. This is Go's package rule.
//
// Every whole-directory import is QUALIFIED: `import <math>;` binds `math`,
// and its declarations are reachable only as `math.name`. That replaces an
// earlier model in which a bare import merged into the flat namespace and
// `as` was needed to avoid collisions. Merging by default made every new
// declaration in a library a potential break for its consumers, and made a
// reader unable to tell where a name came from without checking every import
// in the file. The escape hatch is `from <math> import sin;`, which binds
// exactly the names asked for — so a math-heavy analog body still reads as
// `sin(x)` after one line at the top.
//
// History worth keeping: an even earlier model built ONE flat namespace from
// the entry file's imports alone, and every file resolved against it. Two
// consequences were reported as bugs, and both are why the per-file import
// list below is read rather than any other file's finished scope:
//
//  1. A non-entry file's `import` statements were completely inert. A library
//     could not declare its own dependency, and the error surfaced at a line
//     inside a file the user had never opened.
//  2. `as` aliases were unusable inside a library, since a library cannot
//     control what names the entry file chooses.
//
// Non-transitivity is preserved and deliberate: when file A imports directory
// B, A sees B's OWN declarations, never what B imported. That's the Go rule
// too, and it's why the passes below read exclusively from the per-directory
// "own declarations" table rather than from another file's finished scope.
// ─────────────────────────────────────────────────────────────────────────────

// fileScope is one source file's view of the namespace: what its directory
// declares, plus what its own imports bring in.
type fileScope struct {
	path string // absolute source path, for diagnostics ("" for the no-import case)

	// modules holds the declarations of every file in THIS file's directory,
	// plus any names pulled in by a selective import. Whole-directory imports
	// never land here — that is what makes them non-shadowing.
	modules map[string]*ast.ModuleDeclStatement

	// aliased maps an import qualifier to that directory's module
	// declarations, reachable only as Qualifier.Name. Each qualifier is
	// isolated, so two files importing the same directory under different
	// names never interact — which is the point of per-file scoping.
	aliased map[string]map[string]*ast.ModuleDeclStatement
}

func newFileScope(path string) *fileScope {
	return &fileScope{
		path:    path,
		modules: make(map[string]*ast.ModuleDeclStatement),
		aliased: make(map[string]map[string]*ast.ModuleDeclStatement),
	}
}

// resolveModule looks up a possibly-qualified module name like "motor.PMSM"
// in this file's scope. A name with no dot is a plain lookup in the flat
// (own-directory + selectively-imported) namespace; otherwise the part before
// the first dot is an import qualifier.
//
// Only ONE dot is meaningful: imports are non-transitive, so a module name is
// always either bare ("PMSM") or exactly one qualifier segment plus the real
// name ("motor.PMSM"), never deeper.
func (s *fileScope) resolveModule(name string) (*ast.ModuleDeclStatement, bool) {
	qualifier, bare, isQualified := splitQualifiedName(name)
	if !isQualified {
		decl, ok := s.modules[name]
		return decl, ok
	}
	byName, ok := s.aliased[qualifier]
	if !ok {
		return nil, false
	}
	decl, ok := byName[bare]
	return decl, ok
}

// dirGroups groups loaded files by their directory — the namespace unit.
// Sorted within each directory so declaration order, and therefore the C
// names codegen assigns, is a property of the source rather than of Go's
// randomized map iteration.
func dirGroups(files map[string]*ImportedFile) map[string][]string {
	byDir := make(map[string][]string)
	for path := range files {
		d := filepath.Dir(path)
		byDir[d] = append(byDir[d], path)
	}
	for d := range byDir {
		sort.Strings(byDir[d])
	}
	return byDir
}

// buildModuleScopes constructs one fileScope per loaded file.
//
// Two passes, and the split is load-bearing rather than stylistic: pass 1
// records each DIRECTORY's own declarations, and pass 2 wires up imports
// reading only from that table. A single pass would let one file's scope be
// populated from another file's already-import-mutated scope, which would
// silently make imports transitive depending on map iteration order.
func (e *Elaborator) buildModuleScopes(files map[string]*ImportedFile) error {
	byDir := dirGroups(files)

	// Pass 1 — every directory's own module declarations, merged across its
	// files. A name declared twice in one directory is an error now, where it
	// used to be legal in two different files: they share a namespace, so
	// there would be no way to say which one a reference meant.
	own := make(map[string]map[string]*ast.ModuleDeclStatement, len(byDir))
	for dir, paths := range byDir {
		decls := make(map[string]*ast.ModuleDeclStatement)
		for _, path := range paths {
			for _, stmt := range files[path].Program.Statements {
				m, ok := stmt.(*ast.ModuleDeclStatement)
				if !ok {
					continue
				}
				if prev, dup := decls[m.Name]; dup && prev != m {
					return fmt.Errorf("module '%s' is declared twice in %s — %s line %d and %s line %d. "+
						"Files in one directory share a namespace, so the two names have to differ",
						m.Name, dir,
						filepath.Base(e.declFile[prev]), prev.Line(),
						filepath.Base(path), m.Line())
				}
				decls[m.Name] = m
				e.declFile[m] = path
			}
		}
		own[dir] = decls
	}

	for path := range files {
		e.scopes[path] = newFileScope(path)
	}

	// Pass 2 — fold each file's directory declarations, then its own imports,
	// into its scope.
	for path, f := range files {
		sc := e.scopes[path]
		for name, decl := range own[filepath.Dir(path)] {
			sc.modules[name] = decl
		}

		for _, imp := range f.Imports {
			dir := resolveImportPathFor(path, imp.Path, imp.IsSystem)
			imported, ok := own[dir]
			if !ok {
				return fmt.Errorf("line %d: import %s could not be resolved (looked in %s) — was it loaded?",
					imp.Token.Line, imp.PathString(), dir)
			}

			// `from <dir> import A, B as C;` — only the listed names enter the
			// flat namespace, each under its local spelling. A name that
			// matches nothing here may still be a function, so being absent is
			// not an error yet; buildFunctionScopes reports the names that
			// matched neither, since it is the pass that can see both.
			if imp.Selective {
				for _, sym := range imp.Names {
					decl, found := imported[sym.Name]
					if !found {
						continue
					}
					local := sym.Local()
					if existing, exists := sc.modules[local]; exists && existing != decl {
						return fmt.Errorf("line %d: module '%s' from %s collides with '%s' already visible here (declared at line %d) — import it under a different name (`%s as ...`)",
							imp.Token.Line, local, imp.PathString(), existing.Name, existing.Line(), sym.Name)
					}
					sc.modules[local] = decl
				}
				continue
			}

			// Whole-directory import: everything behind the qualifier, and
			// nothing in the flat namespace. Two imports resolving to the same
			// qualifier would silently merge, so that is rejected.
			q := loader.QualifierFor(imp.Path, imp.Alias)
			if err := checkQualifier(q, imp); err != nil {
				return err
			}
			if _, taken := sc.aliased[q]; taken {
				return fmt.Errorf("line %d: two imports both bind the name '%s' — give one an explicit `as` name",
					imp.Token.Line, q)
			}
			bound := make(map[string]*ast.ModuleDeclStatement, len(imported))
			for name, decl := range imported {
				bound[name] = decl
			}
			sc.aliased[q] = bound
		}
	}

	return nil
}

// checkQualifier rejects a binding name that could not be written as an
// identifier. The default is derived from a directory name, and a directory
// can be called anything at all — so this is where "your package is named
// 3d-parts" turns into an actionable message rather than a reference nobody
// can spell.
func checkQualifier(q string, imp *ast.ImportStatement) error {
	if q == "" {
		return fmt.Errorf("line %d: %s does not give a usable name to import as — add `as <name>`",
			imp.Token.Line, imp.PathString())
	}
	for i := 0; i < len(q); i++ {
		c := q[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' ||
			(i > 0 && c >= '0' && c <= '9')
		if !ok {
			return fmt.Errorf("line %d: %s would bind the name '%s', which is not a valid identifier — add `as <name>`",
				imp.Token.Line, imp.PathString(), q)
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
	for qualifier, byName := range s.aliased {
		for n := range byName {
			names = append(names, qualifier+"."+n)
		}
	}
	sort.Strings(names)

	where := ""
	if s.path != "" {
		where = " in " + filepath.Base(s.path)
	}
	if len(names) == 0 {
		return fmt.Sprintf(" — no modules are visible%s; add an import for the directory that declares it", where)
	}
	return fmt.Sprintf(" — modules visible%s: %s (add an import if the one you want is missing)",
		where, strings.Join(names, ", "))
}

// ─────────────────────────────────────────────────────────────────────────────
// PER-FILE FUNCTION SCOPES
// ─────────────────────────────────────────────────────────────────────────────

// buildFunctionScopes assigns every function declaration in the loaded set a
// unique C identifier, then builds one call-spelling→declaration table per
// file from that file's directory and its own imports. It also gathers
// importc headers, which must come from EVERY file rather than just the
// entry's: a library's extern declarations are emitted whether or not the
// entry file knows they exist, so the header that defines them has to be
// included regardless.
//
// Structure mirrors buildModuleScopes, and the two-pass split is load-bearing
// for the same reason: pass 1 records each directory's OWN declarations, pass
// 2 wires up imports reading only from that table, so imports cannot become
// accidentally transitive through map iteration order.
func (e *Elaborator) buildFunctionScopes(files map[string]*ImportedFile) error {
	byDir := dirGroups(files)

	// Sorted, so C names and emission order are a property of the program
	// rather than of Go's randomized map iteration. Two builds of the same
	// source must produce the same sim.cpp.
	dirs := make([]string, 0, len(byDir))
	for dir := range byDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	// Pass 0 — extern names are fixed: they name a real C function in a header
	// and cannot be renamed. Reserve them all before uniquing anything else, so
	// a Hover-level function never takes a name the linker has already promised
	// to something in a header.
	taken := make(map[string]bool)
	for _, dir := range dirs {
		for _, path := range byDir[dir] {
			for _, stmt := range files[path].Program.Statements {
				if f, ok := stmt.(*ast.FuncDeclStatement); ok && f.IsExtern {
					taken[f.Name] = true
				}
			}
		}
	}

	// Pass 1 — per-directory declarations, unique C names, importc headers.
	own := make(map[string]map[string]*FunctionInfo, len(byDir))
	for _, dir := range dirs {
		decls := make(map[string]*FunctionInfo)
		for _, path := range byDir[dir] {
			for _, stmt := range files[path].Program.Statements {
				if ic, ok := stmt.(*ast.ImportCStatement); ok {
					e.output.CIncludes = append(e.output.CIncludes, ic.Path)
				}
				f, ok := stmt.(*ast.FuncDeclStatement)
				if !ok {
					continue
				}
				if existing, dup := decls[f.Name]; dup {
					if existing.Decl.IsExtern && f.IsExtern {
						continue // two files declaring the same C function — harmless
					}
					return fmt.Errorf("function '%s' is declared twice in %s — %s line %d and %s line %d. "+
						"Files in one directory share a namespace, so the two names have to differ",
						f.Name, dir,
						filepath.Base(existing.File), existing.Decl.Line(),
						filepath.Base(path), f.Line())
				}
				info := &FunctionInfo{Decl: f, CName: uniqueCName(f, taken), File: path}
				decls[f.Name] = info
				e.output.Functions = append(e.output.Functions, info)
			}
		}
		own[dir] = decls
	}

	for path := range files {
		e.output.FuncScopes[path] = make(map[string]*FunctionInfo)
	}

	// Pass 2 — fold each file's directory declarations, then its own imports.
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		sc := e.output.FuncScopes[path]
		for name, info := range own[filepath.Dir(path)] {
			sc[name] = info
		}

		for _, imp := range files[path].Imports {
			dir := resolveImportPathFor(path, imp.Path, imp.IsSystem)
			imported, ok := own[dir]
			if !ok {
				// buildModuleScopes runs first and reports unresolvable
				// imports with a better message; nothing to add here.
				continue
			}

			// Selective import. This pass owns the "no such name" diagnostic
			// because it is the only one that can see both namespaces: a
			// selected name is valid if EITHER a module or a function answers
			// to it, and buildModuleScopes ran without knowing about functions.
			if imp.Selective {
				for _, sym := range imp.Names {
					info, found := imported[sym.Name]
					if !found {
						if declaresModule(files, byDir[dir], sym.Name) {
							continue // handled by buildModuleScopes
						}
						return fmt.Errorf("line %d: %s declares no module or function named '%s'%s",
							imp.Token.Line, imp.PathString(), sym.Name,
							describeExports(files, byDir[dir]))
					}
					local := sym.Local()
					if existing, exists := sc[local]; exists && existing != info {
						if existing.Decl.IsExtern && info.Decl.IsExtern {
							continue
						}
						return fmt.Errorf("line %d: function '%s' from %s collides with '%s' already visible here (declared at line %d) — import it under a different name (`%s as ...`)",
							imp.Token.Line, local, imp.PathString(), existing.Decl.Name, existing.Decl.Line(), sym.Name)
					}
					sc[local] = info
				}
				continue
			}

			// Whole-directory import: qualified names only. A qualified
			// spelling always contains a dot and a bare function name never
			// can, so this cannot collide with the flat namespace.
			q := loader.QualifierFor(imp.Path, imp.Alias)
			for name, info := range imported {
				sc[q+"."+name] = info
			}
		}
	}

	return nil
}

// declaresModule reports whether any file in the group declares a module by
// that name. Used on the error path of a selective import, to tell "you asked
// for a name this directory doesn't have" apart from "you asked for a module,
// which the function pass simply isn't the one that binds it".
func declaresModule(files map[string]*ImportedFile, group []string, name string) bool {
	for _, path := range group {
		f := files[path]
		if f == nil {
			continue
		}
		for _, stmt := range f.Program.Statements {
			if m, ok := stmt.(*ast.ModuleDeclStatement); ok && m.Name == name {
				return true
			}
		}
	}
	return false
}

// describeExports lists what a directory actually declares, for the error a
// selective import produces when it names something absent. Without it the
// message says only that the name is missing, leaving the user to open every
// file in the directory — and a typo'd or renamed declaration is by far the
// most likely cause.
func describeExports(files map[string]*ImportedFile, group []string) string {
	var names []string
	for _, path := range group {
		f := files[path]
		if f == nil {
			continue
		}
		for _, stmt := range f.Program.Statements {
			switch d := stmt.(type) {
			case *ast.ModuleDeclStatement:
				names = append(names, d.Name)
			case *ast.FuncDeclStatement:
				names = append(names, d.Name)
			}
		}
	}
	if len(names) == 0 {
		return " — that directory declares no modules or functions at all"
	}
	sort.Strings(names)
	if len(names) > 20 {
		names = append(names[:20], fmt.Sprintf("... and %d more", len(names)-20))
	}
	return " — it declares: " + strings.Join(names, ", ")
}

// uniqueCName picks the C identifier a function will be emitted under,
// recording it in taken.
//
// Extern functions keep their exact name — it IS the C symbol the header
// declares, so renaming it would break the link. Everything else prefers its
// Hover name and falls back to a numbered suffix, which only happens now that
// two different directories may each declare a private helper of the same
// name. The suffix is deterministic because callers walk directories and
// files in sorted order.
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
//
// Sorted by declaring file and then source line. The map it reads from has
// randomized iteration order, and the macro passes it feeds (processAnalogIdt,
// processAnalogDdt) inject statements whose order reaches the generated C++ —
// so without this, two builds of identical source produced different sim.cpp.
// That was a real, observable bug: repeated runs of examples/DCMotor emitted
// the same statements in different positions.
//
// Reproducibility is not cosmetic here. Hover's package manager identifies
// every package by the hash of its contents, and "the same input builds the
// same output" is the property that makes such a system worth anything.
func (e *Elaborator) allModules() []*ast.ModuleDeclStatement {
	mods := make([]*ast.ModuleDeclStatement, 0, len(e.declFile))
	for m := range e.declFile {
		mods = append(mods, m)
	}
	sort.Slice(mods, func(i, j int) bool {
		fi, fj := e.declFile[mods[i]], e.declFile[mods[j]]
		if fi != fj {
			return fi < fj
		}
		if mods[i].Line() != mods[j].Line() {
			return mods[i].Line() < mods[j].Line()
		}
		return mods[i].Name < mods[j].Name
	})
	return mods
}
