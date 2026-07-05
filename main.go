package main

import (
	"fmt"
	codegen "hover/Codegen"
	"hover/Interpreter/ast"
	"hover/Interpreter/elaborator"
	"hover/Interpreter/lexer"
	"hover/Interpreter/loader"
	"hover/Interpreter/parser"
	"hover/Interpreter/semantic"
	"hover/Interpreter/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func main() {
	fmt.Println("Hover v0.6.3")

	if len(os.Args) < 2 {
		fmt.Println("Usage: ./hover <filename.hvr> [--dump-ast]")
		os.Exit(1)
	}

	dumpAST := len(os.Args) >= 3 && os.Args[2] == "--dump-ast"
	entryFile := os.Args[1]

	// ── 0. Load — discover entry file + every (non-transitive) import ────────
	loadResult, err := loader.Load(entryFile)
	if err != nil {
		fmt.Printf("[Loader] Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[Loader] OK — %d file(s) loaded\n", len(loadResult.LoadOrder))

	// ── 1+2. Lex + Parse every loaded file ────────────────────────────────────
	// Each file is tokenized and parsed completely independently — this is
	// what keeps line/column numbers in error messages accurate per file,
	// rather than shifted by however much text came before it in some
	// merged blob.
	importedFiles := make(map[string]*elaborator.ImportedFile, len(loadResult.LoadOrder))
	parseFailed := false
	for _, path := range loadResult.LoadOrder {
		source := loadResult.Sources[path]

		l := lexer.New(source)
		var tokens []token.Token
		for {
			tok := l.NextToken()
			tokens = append(tokens, tok)
			if tok.Type == token.EOF {
				break
			}
		}

		program, parseErrors := parser.Parse(tokens)
		if len(parseErrors) > 0 {
			parseFailed = true
			fmt.Printf("[Parser] %d syntax error(s) in %s:\n", len(parseErrors), path)
			for _, msg := range parseErrors {
				fmt.Println("   -", msg)
			}
		}

		// Pull out this file's own ImportStatement nodes so the elaborator
		// can match them against loadResult.Imports[path] (which has the
		// already-resolved absolute paths) without re-parsing anything.
		var imports []*ast.ImportStatement
		for _, stmt := range program.Statements {
			if imp, ok := stmt.(*ast.ImportStatement); ok {
				imports = append(imports, imp)
			}
		}

		importedFiles[path] = &elaborator.ImportedFile{
			FilePath: path,
			Program:  program,
			Imports:  imports,
		}

		if path == loadResult.EntryPath {
			fmt.Printf("[Lexer]  %d tokens from %s\n", len(tokens), path)
			fmt.Printf("[Parser] OK — %d top-level statements\n", len(program.Statements))
		} else {
			fmt.Printf("[Lexer]  %d tokens from %s (imported)\n", len(tokens), path)
		}
	}

	if parseFailed {
		os.Exit(1)
	}

	entryProgram := importedFiles[loadResult.EntryPath].Program

	if dumpAST {
		fmt.Println("\n==========================================")
		fmt.Println("                 AST DUMP                ")
		fmt.Println("           (entry file only)             ")
		fmt.Println("==========================================")
		fmt.Println(entryProgram.String())
		fmt.Println("==========================================")
		os.Exit(0)
	}

	// ── 3. Semantic Check ────────────────────────────────────────────────────
	// Semantic analysis runs on the entry file's own AST only. Imported
	// files are analyzed implicitly through the elaborator's resolution —
	// a module/function that doesn't exist in an aliased import surfaces
	// as an elaboration error ("undeclared module") rather than a semantic
	// one, since semantic.Analyzer has no concept of cross-file imports.

	analyzer := semantic.NewAnalyzer()
	// Make functions from the entry file's own imports (e.g. sin from
	// <math/math.hvr>) visible to the entry-file-only semantic pass. Real
	// visibility is still enforced by the elaborator; this only prevents
	// false "undeclared" errors.
	for _, imp := range loadResult.Imports[loadResult.EntryPath] {
		if imp.Alias != "" {
			continue // aliased funcs are called as Alias.f, not bare globals
		}
		if f, ok := importedFiles[imp.ResolvedPath]; ok {
			analyzer.RegisterImportedFunctions(f.Program)
		}
	}

	if errors := analyzer.Analyze(entryProgram); len(errors) > 0 {
		fmt.Printf("[Semantic] %d error(s):\n", len(errors))
		for _, e := range errors {
			fmt.Println(" ", e)
		}
		os.Exit(1)
	}
	fmt.Println("[Semantic] OK")

	// ── 4. Elaborate ─────────────────────────────────────────────────────────
	var elab *elaborator.Elaborator
	if len(importedFiles) == 1 {
		// No imports at all — use the plain single-file constructor.
		// Behaviorally identical to NewWithImports with an empty import
		// table, but keeps the zero-import path exactly as simple as it
		// was before this feature existed.
		elab = elaborator.New(entryProgram)
	} else {
		elab, err = elaborator.NewWithImports(importedFiles, loadResult.EntryPath)
		if err != nil {
			fmt.Printf("[Elaborator] Import resolution error: %v\n", err)
			os.Exit(1)
		}
	}

	flatProg, _, err := elab.Elaborate()
	if err != nil {
		fmt.Printf("[Elaborator] Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[Elaborator] OK — %d logic blocks, %d physicals\n",
		len(flatProg.Logic), len(flatProg.Physicals))

	// ── 5. Code Generation ───────────────────────────────────────────────────
	simCpp, unresolved, err := codegen.GenerateWithDiagnostics(flatProg)
	if err != nil {
		fmt.Printf("[Codegen] Error: %v\n", err)
		os.Exit(1)
	}

	if len(unresolved) > 0 {
		// Search every file the loader actually loaded from disk
		// (transitively — this is the loader's full file graph, NOT the
		// elaborator's deliberately non-transitive merged namespace) for
		// a function declaration matching each unresolved name. Same
		// idea as Clang's "undeclared identifier" diagnostics: no
		// automatic fix, no change to import semantics — just looking at
		// what's already known and printing something useful instead of
		// silently deferring to a confusing C++ compiler error three
		// layers removed from the actual Hover-level mistake.
		for _, fnName := range unresolved {
			foundIn := ""
			for path, f := range importedFiles {
				for _, stmt := range f.Program.Statements {
					if fd, ok := stmt.(*ast.FuncDeclStatement); ok && fd.Name == fnName {
						foundIn = path
						break
					}
				}
				if foundIn != "" {
					break
				}
			}
			if foundIn != "" {
				suggested := foundIn
				if rel, relErr := filepath.Rel(filepath.Dir(entryFile), foundIn); relErr == nil {
					// Match the "./..." style every import in a Hover
					// project actually uses, rather than printing a raw
					// absolute path nobody would type by hand.
					if !strings.HasPrefix(rel, ".") {
						rel = "./" + rel
					}
					suggested = rel
				}
				fmt.Printf("[Codegen] Error: undefined function '%s'\n", fnName)
				fmt.Printf("  found in: %s\n", foundIn)
				fmt.Printf("  add: import %q;\n", suggested)
			} else {
				fmt.Printf("[Codegen] Error: undefined function '%s' (not found in any loaded file)\n", fnName)
			}
		}
		os.Exit(1)
	}

	if err := os.WriteFile("sim.cpp", []byte(simCpp), 0644); err != nil {
		fmt.Printf("[Codegen] Failed to write sim.cpp: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("[Codegen] sim.cpp written")

	// ── 6. Compile ───────────────────────────────────────────────────────────
	fmt.Println("[Compile] Building simulation binary with Zig...")

	exeName := "sim"
	if runtime.GOOS == "windows" {
		exeName = "sim.exe"
	}

	zigPath := "zig"
	if runtime.GOOS == "windows" {
		if _, err := os.Stat("./toolchain/zig/zig.exe"); err == nil {
			zigPath = "./toolchain/zig/zig.exe"
		}
	} else {
		if _, err := os.Stat("./toolchain/zig/zig"); err == nil {
			zigPath = "./toolchain/zig/zig"
		}
	}

	var runtimeLib string
	if runtime.GOOS == "windows" {
		if _, err := os.Stat("./runtime/hover_runtime.lib"); err == nil {
			runtimeLib = "./runtime/hover_runtime.lib"
		} else {
			runtimeLib = "./runtime/build/windows/hover_runtime.lib"
		}
	} else {
		if _, err := os.Stat("./runtime/libhover_runtime.a"); err == nil {
			runtimeLib = "./runtime/libhover_runtime.a"
		} else {
			runtimeLib = "./runtime/build/linux/libhover_runtime.a"
		}
	}

	// ── FFI: resolve `importc` headers ───────────────────────────────────────
	// Quoted headers are resolved relative to the entry .hvr file's directory.
	// That directory goes on the include path, and any sibling source file
	// providing the extern definitions (foo.hpp -> foo.cpp/.c/.cc/.cxx) is
	// auto-linked, so `importc "wrappers.hpp"` needs no Makefile edit.
	// Angle headers (<stdio.h>) are system headers and need no -I.
	entryDir, _ := filepath.Abs(filepath.Dir(entryFile))
	ffiIncludeSet := map[string]bool{}
	var ffiIncludeDirs []string
	var ffiSources []string
	seenSrc := map[string]bool{}

	for _, inc := range flatProg.CIncludes {
		h := strings.TrimSpace(inc)
		if h == "" || strings.HasPrefix(h, "<") {
			continue // system header — found automatically
		}
		headerPath := h
		if !filepath.IsAbs(headerPath) {
			headerPath = filepath.Join(entryDir, h)
		}
		dir := filepath.Dir(headerPath)
		if !ffiIncludeSet[dir] {
			ffiIncludeSet[dir] = true
			ffiIncludeDirs = append(ffiIncludeDirs, dir)
		}
		base := strings.TrimSuffix(headerPath, filepath.Ext(headerPath))
		for _, ext := range []string{".cpp", ".c", ".cc", ".cxx"} {
			cand := base + ext
			if _, err := os.Stat(cand); err == nil && !seenSrc[cand] {
				seenSrc[cand] = true
				ffiSources = append(ffiSources, cand)
			}
		}
	}
	if len(ffiSources) > 0 {
		fmt.Printf("[Compile] Linking FFI sources: %s\n", strings.Join(ffiSources, ", "))
	}

	compileArgs := []string{
		"c++",
		"-std=c++17",
		"-O3",
		"-w",
	}

	if runtime.GOOS == "windows" {
		compileArgs = append(compileArgs, "-target", "x86_64-windows-gnu")
	} else {
		compileArgs = append(compileArgs, "-target", "x86_64-linux-gnu")
	}

	compileArgs = append(compileArgs, "sim.cpp")
	compileArgs = append(compileArgs, ffiSources...) // FFI definitions
	compileArgs = append(compileArgs, "-I./runtime")
	for _, dir := range ffiIncludeDirs { // FFI header locations
		compileArgs = append(compileArgs, "-I"+dir)
	}
	compileArgs = append(compileArgs, runtimeLib, "-o", exeName)

	compileCmd := exec.Command(zigPath, compileArgs...)
	compileCmd.Stdout = os.Stdout
	compileCmd.Stderr = os.Stderr
	if err := compileCmd.Run(); err != nil {
		fmt.Printf("[Compile] Compilation failed: %v\n", err)
		os.Exit(1)
	}

	// ── 7. Run ───────────────────────────────────────────────────────────────
	fmt.Println("[Run] Starting simulation...")

	runPath := "./" + exeName
	if runtime.GOOS == "windows" {
		runPath = ".\\" + exeName
	}

	runCmd := exec.Command(runPath)
	runCmd.Stdout = os.Stdout
	runCmd.Stderr = os.Stderr
	if err := runCmd.Run(); err != nil {
		fmt.Printf("[Run] Simulation crashed or failed: %v\n", err)
		os.Exit(1)
	}
}
