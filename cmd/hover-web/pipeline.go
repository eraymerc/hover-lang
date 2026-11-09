package main

// The browser playground's front end: the same loader → parser → semantic →
// elaborator → codegen pipeline as the CLI (main.go), minus everything that
// needs a machine — Zig, hpm, and the disk. Source files come from an
// in-memory fs.FS the page hands over (the user's buffer plus the bundled
// standard library), and the result is the same sim.cpp the CLI writes; the
// page then compiles that with a WASM build of clang and links it against the
// runtime, prebuilt to WASM.
//
// This file has no build constraint so the pipeline can be exercised by a
// native `go test` (see pipeline_test.go); only main_js.go is WASM-specific.

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"hover/compiler/ast"
	codegen "hover/compiler/codegen"
	"hover/compiler/elaborator"
	"hover/compiler/loader"
	"hover/compiler/semantic"
)

// compileResult is what one compile hands back to the page.
type compileResult struct {
	OK   bool     `json:"ok"`
	Code string   `json:"code,omitempty"` // the generated sim.cpp
	Log  []string `json:"log"`            // the CLI's progress lines, in order
}

// compile runs the front end on entryPath. fsys == nil reads the real disk
// (native tests); otherwise every source is read from fsys. roots is the
// package table — for the playground, the bundled standard library's import
// roots — exactly what hpm would install from a lockfile.
func compile(fsys fs.FS, entryPath string, roots map[string]string) compileResult {
	res := compileResult{}
	logf := func(format string, args ...interface{}) {
		res.Log = append(res.Log, fmt.Sprintf(format, args...))
	}
	fail := func() compileResult { res.OK = false; return res }

	if fsys != nil {
		loader.SetFS(fsys)
	}
	loader.SetPackageRoots(roots)

	loadResult, err := loader.Load(entryPath)
	if err != nil {
		logf("[Loader] Error: %v", err)
		return fail()
	}
	logf("[Loader] OK — %d file(s) loaded", len(loadResult.LoadOrder))

	importedFiles := make(map[string]*elaborator.ImportedFile, len(loadResult.LoadOrder))
	parseFailed := false
	for _, path := range loadResult.LoadOrder {
		program := loadResult.Programs[path]
		if errs := loadResult.ParseErrors[path]; len(errs) > 0 {
			parseFailed = true
			logf("[Parser] %d syntax error(s) in %s:", len(errs), displayPath(path))
			for _, msg := range errs {
				logf("   - %s", msg)
			}
		}
		var imports []*ast.ImportStatement
		for _, stmt := range program.Statements {
			if imp, ok := stmt.(*ast.ImportStatement); ok {
				imports = append(imports, imp)
			}
		}
		importedFiles[path] = &elaborator.ImportedFile{FilePath: path, Program: program, Imports: imports}
	}
	if parseFailed {
		return fail()
	}
	logf("[Parser] OK")

	semanticFailed := false
	for _, path := range loadResult.LoadOrder {
		if errs := analyzeFile(path, loadResult, importedFiles); len(errs) > 0 {
			semanticFailed = true
			logf("[Semantic] %d error(s) in %s:", len(errs), displayPath(path))
			for _, e := range errs {
				logf("  %s", e)
			}
		}
	}
	if semanticFailed {
		return fail()
	}
	logf("[Semantic] OK")

	entryProgram := importedFiles[loadResult.EntryPath].Program
	var elab *elaborator.Elaborator
	if len(importedFiles) == 1 {
		elab = elaborator.New(entryProgram)
	} else {
		elab, err = elaborator.NewWithImports(importedFiles, loadResult.EntryPath)
		if err != nil {
			logf("[Elaborator] Import resolution error: %v", err)
			return fail()
		}
	}
	flatProg, err := elab.Elaborate()
	if err != nil {
		logf("[Elaborator] Error: %v", err)
		return fail()
	}
	logf("[Elaborator] OK — %d logic blocks, %d physicals", len(flatProg.Logic), len(flatProg.Physicals))

	code, unresolved, err := codegen.GenerateWithDiagnostics(flatProg)
	if err != nil {
		logf("[Codegen] Error: %v", err)
		return fail()
	}
	if len(unresolved) > 0 {
		for _, fn := range unresolved {
			logf("[Codegen] Error: undefined function '%s'%s", fn, foundInHint(fn, importedFiles))
		}
		return fail()
	}
	logf("[Codegen] OK")

	res.OK = true
	res.Code = code
	return res
}

// foundInHint mirrors main.go's "found in … add: import …" diagnostic.
func foundInHint(fnName string, files map[string]*elaborator.ImportedFile) string {
	for path, f := range files {
		for _, stmt := range f.Program.Statements {
			if fd, ok := stmt.(*ast.FuncDeclStatement); ok && fd.Name == fnName {
				return fmt.Sprintf(" — it is declared in %s; import that directory", displayPath(path))
			}
		}
	}
	return " (not found in any loaded file)"
}

// displayPath shows playground paths the way a user thinks of them: the
// buffer is just "main.hvr", a stdlib file is "<math/math.hvr>".
func displayPath(p string) string {
	p = filepath.ToSlash(p)
	if strings.HasPrefix(p, "/stdlib/") {
		return "<" + strings.TrimPrefix(p, "/stdlib/") + ">"
	}
	if strings.HasPrefix(p, "/work/") {
		return strings.TrimPrefix(p, "/work/")
	}
	return p
}

// analyzeFile is main.go's analyzeFile, verbatim in behaviour: semantic
// analysis of one file, with the functions and structs it can see registered
// first — its own directory's siblings, then what its imports expose. It is
// copied rather than shared because main.go is package main; if the CLI's
// copy changes, change this one with it.
func analyzeFile(path string, loadResult *loader.LoadResult, files map[string]*elaborator.ImportedFile) []string {
	a := semantic.NewAnalyzer()
	a.RegisterImportedFunctions(files[path].Program)

	structsDone := map[string]bool{path: true}
	registerStructs := func(p string) {
		if !structsDone[p] {
			structsDone[p] = true
			a.RegisterImportedStructs(files[p].Program)
		}
	}

	dir := filepath.Dir(path)
	for _, p := range loadResult.LoadOrder {
		if p != path && filepath.Dir(p) == dir {
			registerStructs(p)
			a.RegisterImportedFunctions(files[p].Program)
		}
	}
	for _, imp := range loadResult.Imports[path] {
		for _, p := range imp.Files {
			f, ok := files[p]
			if !ok {
				continue
			}
			registerStructs(p)
			if imp.Selective {
				locals := make(map[string]string, len(imp.Selected))
				for _, sym := range imp.Selected {
					locals[sym.Name] = sym.Local()
				}
				a.RegisterSelectedFunctions(f.Program, locals)
			} else {
				a.RegisterAliasedFunctions(imp.Qualifier, f.Program)
			}
		}
	}
	return a.Analyze(files[path].Program)
}

// stdlibRoots is the package table for a standard library rooted at dir:
// the archive's own name plus each of its top-level directories, which is
// how hpm binds an installed stdlib (see stdlib/hover.toml).
//
// fsys is rooted AT the stdlib (so "." lists its top level); prefix is the
// absolute path the loader should see for that root.
func stdlibRoots(fsys fs.FS, prefix string) map[string]string {
	roots := map[string]string{"stdlib": prefix}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return roots
	}
	for _, e := range entries {
		if e.IsDir() {
			roots[e.Name()] = prefix + "/" + e.Name()
		}
	}
	return roots
}
