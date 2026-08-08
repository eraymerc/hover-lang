package main

import (
	"fmt"
	"hover/compiler/ast"
	codegen "hover/compiler/codegen"
	"hover/compiler/elaborator"
	"hover/compiler/lexer"
	"hover/compiler/loader"
	"hover/compiler/parser"
	"hover/compiler/semantic"
	"hover/compiler/token"
	"hover/hpm"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const usage = `Usage: ./hover <filename.hvr> [options]
       ./hover hpm <command> [arguments]
       ./hover --setup

Options:
  -o <path>       Write the built artifact to <path> instead of the default
                  name (sim / sim.exe, or libhovercraft.so / hovercraft.dll
                  with --hovercraft). -o=<path> is also accepted.
                  The generated sim.cpp is always written to the current
                  directory regardless of -o.
  --hovercraft    Emit a reusable HVR_* C-ABI shared library instead of a
                  one-shot simulation binary (see examples/hovercraft/).
  --dump-ast      Print the entry file's AST and exit without compiling.
  --setup         Check for a usable Zig toolchain and fix it up if not:
                  on Windows, downloads one into ./toolchain/zig; on Linux,
                  reports whether one is already on PATH. Run this once
                  after extracting a release, or any time a compile fails
                  with a "Zig not found" error. Takes no other arguments.

Package management:
  hpm <command>   Manage this project's dependencies (install, update,
                  remove, list, verify, index, clean). Run "hover hpm" for
                  its own help. Releases also ship an "hpm" symlink to this
                  binary, so "hpm install foo" works directly.`

// cliOptions is the parsed command line. Parsing lives in parseArgs rather
// than inline in main so that flag order never matters and an unrecognized
// flag is an error instead of being silently ignored — a typo'd
// "--hovercaft" used to quietly produce a standalone binary, and with -o in
// the mix a swallowed flag could just as quietly write the wrong file.
type cliOptions struct {
	entryFile string

	// outputPath is -o's argument, or "" for the platform default name.
	// Names the final artifact only (the binary or the shared library),
	// the way clang's -o does; sim.cpp is an inspectable intermediate and
	// stays in the working directory.
	outputPath string

	dumpAST bool

	// libraryMode is --hovercraft: codegen + build switch to library
	// output — a reusable HVR_* C-ABI shared library rather than a
	// one-shot binary that runs to completion and exits. See
	// codegen.GenerateLibrary and compiler/codegen/hovercraft_emit.go.
	libraryMode bool
}

// parseArgs parses os.Args[1:]. -o accepts clang's canonical separated form
// (-o sim) and the =-joined form (-o=sim). The attached form (-osim) is
// deliberately NOT accepted: matching it by prefix would silently swallow
// any future flag beginning with "-o", turning a typo into a wrong output
// filename instead of the error every other unknown flag produces.
func parseArgs(args []string) (cliOptions, error) {
	var opts cliOptions

	setOutput := func(v string) error {
		if v == "" {
			return fmt.Errorf("-o requires an output filename")
		}
		if strings.HasPrefix(v, "-") {
			return fmt.Errorf("-o expects a filename, got flag %q "+
				"(use ./%s if you really meant a file by that name)", v, v)
		}
		opts.outputPath = v // last -o wins, as clang does
		return nil
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-o":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("-o requires an output filename")
			}
			i++
			if err := setOutput(args[i]); err != nil {
				return opts, err
			}
		case strings.HasPrefix(arg, "-o="):
			if err := setOutput(strings.TrimPrefix(arg, "-o=")); err != nil {
				return opts, err
			}
		case arg == "--hovercraft":
			opts.libraryMode = true
		case arg == "--dump-ast":
			opts.dumpAST = true
		case strings.HasPrefix(arg, "-"):
			return opts, fmt.Errorf("unknown flag %q", arg)
		default:
			if opts.entryFile != "" {
				return opts, fmt.Errorf(
					"unexpected argument %q — only one input file is accepted (already have %q)",
					arg, opts.entryFile)
			}
			opts.entryFile = arg
		}
	}

	if opts.entryFile == "" {
		return opts, fmt.Errorf("no input file")
	}
	return opts, nil
}

// checkOutputPath rejects an -o target that cannot possibly be written,
// before the compiler spends a full lex/parse/elaborate/codegen pass only
// for the linker to fail at the very end. Like clang, a missing directory
// is an error rather than something to create.
func checkOutputPath(path string) error {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return fmt.Errorf("output path %q is a directory", path)
	}
	dir := filepath.Dir(path)
	if dir == "." {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("output directory %q does not exist", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("output directory %q is not a directory", dir)
	}
	return nil
}

// invokedAsHPM reports whether this process was started through the `hpm`
// symlink rather than as `hover`.
//
// argv[0] dispatch, the busybox trick. It is what lets `hover hpm install
// foo` and `hpm install foo` be the same words in the same order from one
// binary — and it is why "hpm" is a better subcommand-group name than "pkg"
// would have been, since "pkg" could not also serve as the standalone
// command without inventing a second vocabulary.
//
// A separate hpm binary was considered and rejected: separate binaries
// create pip's version-pairing problem ("which hover does this hpm install
// for?"), which would bite harder here because hover resolves the standard
// library, runtime and toolchain relative to its own executable
// (loader.ExeDir). The symlink costs nothing, since ExeDir already calls
// EvalSymlinks and resolves back to the real hover directory.
func invokedAsHPM() bool {
	base := strings.ToLower(filepath.Base(os.Args[0]))
	base = strings.TrimSuffix(base, ".exe")
	return base == "hpm"
}

func main() {
	// Dispatched before the version banner: hpm has its own output, and a
	// stray "Hover v0.8.0" on stdout would land in anything parsing it.
	if invokedAsHPM() {
		os.Exit(hpm.Run(os.Args[1:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "hpm" {
		os.Exit(hpm.Run(os.Args[2:]))
	}

	fmt.Println("Hover v0.8.0")

	if len(os.Args) > 1 && os.Args[1] == "--setup" {
		if len(os.Args) > 2 {
			fmt.Println("Error: --setup takes no other arguments")
			os.Exit(1)
		}
		runSetup()
		return
	}

	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Printf("Error: %v\n\n", err)
		fmt.Println(usage)
		os.Exit(1)
	}
	if opts.outputPath != "" {
		if err := checkOutputPath(opts.outputPath); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
	}

	entryFile := opts.entryFile
	dumpAST := opts.dumpAST
	libraryMode := opts.libraryMode

	// ── 0. Load — discover entry file + every (non-transitive) import ────────
	// Package roots come from the LOCKFILE of whatever project contains the
	// entry file, before anything is read: `import <@pkg/x.hvr>` has to
	// resolve to exactly what `hover hpm install` put in the cache. Nothing
	// here resolves versions or touches the network — compiling must never
	// install, or a build would silently differ from the one that was
	// locked. A file outside any project simply gets no packages, and
	// stdlib plus relative imports keep working as before.
	pkgRoots, err := hpm.ProjectPackagesForFile(entryFile)
	if err != nil {
		fmt.Printf("[hpm] Error: %v\n", err)
		os.Exit(1)
	}
	loader.SetPackageRoots(pkgRoots)

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
		f, ok := importedFiles[imp.ResolvedPath]
		if !ok {
			continue
		}
		switch {
		case imp.Selective:
			// Only the names actually asked for, under their local spelling.
			locals := make(map[string]string, len(imp.Selected))
			for _, sym := range imp.Selected {
				locals[sym.Name] = sym.Local()
			}
			analyzer.RegisterSelectedFunctions(f.Program, locals)
		case imp.Alias != "":
			// aliased funcs are called as Alias.f, not bare globals
		default:
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

	flatProg, err := elab.Elaborate()
	if err != nil {
		fmt.Printf("[Elaborator] Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[Elaborator] OK — %d logic blocks, %d physicals\n",
		len(flatProg.Logic), len(flatProg.Physicals))

	// ── 5. Code Generation ───────────────────────────────────────────────────
	var simCpp string
	var unresolved []string
	if libraryMode {
		simCpp, unresolved, err = codegen.GenerateLibrary(flatProg)
	} else {
		simCpp, unresolved, err = codegen.GenerateWithDiagnostics(flatProg)
	}
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
	if libraryMode {
		exeName = "libhovercraft.so"
		if runtime.GOOS == "windows" {
			exeName = "hovercraft.dll"
		}
	}
	// -o overrides the platform default verbatim — no extension is added or
	// corrected, exactly like clang. A path is fine; checkOutputPath has
	// already confirmed its directory exists.
	if opts.outputPath != "" {
		exeName = opts.outputPath
	}

	// Windows: a locally bundled zig, else one on PATH. Linux: PATH only.
	// Neither downloads anything here — that is `hover --setup`'s job.
	var zigPath string
	if runtime.GOOS == "windows" {
		p, err := resolveWindowsZig()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		zigPath = p
	} else {
		p, err := exec.LookPath("zig")
		if err != nil {
			fmt.Println("[Compile] Zig not found on PATH. Install it via your package manager (e.g. `sudo pacman -S zig`, `sudo apt install zig`, `sudo dnf install zig`, `sudo pkg install zig`) and make sure it's on PATH, then run `hover --setup`.")
			os.Exit(1)
		}
		zigPath = p
	}

	// Every path below is anchored to the executable's own directory, not
	// the caller's cwd — same reasoning as loader.ExeDir()'s stdlib
	// resolution: `hover foo.hvr` has to work from any directory once
	// hover is on PATH, not just from inside its install folder.
	exeDir, err := loader.ExeDir()
	if err != nil {
		fmt.Printf("[Compile] Could not locate the hover executable's own directory: %v\n", err)
		os.Exit(1)
	}
	runtimeDir := filepath.Join(exeDir, "runtime")

	// The runtime archive is built by `hover --setup` using this same Zig
	// (see runtimebuild.go); this also rejects an archive left over from a
	// different Zig rather than letting it fail at link time.
	runtimeLib, err := checkRuntimeLib(zigPath, runtimeDir)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
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
	if libraryMode {
		compileArgs = append(compileArgs, "-shared")
		if runtime.GOOS != "windows" {
			compileArgs = append(compileArgs, "-fPIC")
		}
	}

	// No -target: this is a native build, which is Zig's default and
	// detects the host libc properly rather than guessing a triple. It is
	// also what the runtime archive was built as (see runtimebuild.go), so
	// the two always agree.
	compileArgs = append(compileArgs, "sim.cpp")
	compileArgs = append(compileArgs, ffiSources...) // FFI definitions
	compileArgs = append(compileArgs, "-I"+runtimeDir)
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
	// A --hovercraft build produces a library, not something to execute —
	// the host program (see examples/hovercraft/) drives it instead.
	if libraryMode {
		fmt.Printf("[Hovercraft] %s built — see examples/hovercraft/ for usage.\n", exeName)
		return
	}

	fmt.Println("[Run] Starting simulation...")

	// A bare name has to be spelled ./sim so exec doesn't search $PATH for
	// it, but an -o path may already be absolute or directory-qualified —
	// blindly prefixing "./" would turn /tmp/sim into .//tmp/sim.
	runPath := exeName
	if !filepath.IsAbs(runPath) {
		runPath = "." + string(filepath.Separator) + runPath
	}

	runCmd := exec.Command(runPath)
	runCmd.Stdout = os.Stdout
	runCmd.Stderr = os.Stderr
	if err := runCmd.Run(); err != nil {
		fmt.Printf("[Run] Simulation crashed or failed: %v\n", err)
		os.Exit(1)
	}
}
