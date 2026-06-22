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
	fmt.Println("Hover v0.5.0")

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

		program := parser.Parse(tokens)

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

	compileArgs = append(compileArgs,
		"sim.cpp",
		"-I./runtime",
		runtimeLib,
		"-o", exeName,
	)

	compileCmd := exec.Command(zigPath, compileArgs...)
	compileCmd.Stdout = os.Stdout
	compileCmd.Stderr = os.Stderr
	if err := compileCmd.Run(); err != nil {
		fmt.Printf("[Compile] Compilation failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("[Compile] Successfully built %s\n", exeName)

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
