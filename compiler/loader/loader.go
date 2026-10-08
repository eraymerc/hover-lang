package loader

import (
	"fmt"
	ast "hover/compiler/ast"
	"hover/compiler/lexer"
	"hover/compiler/parser"
	token "hover/compiler/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Load starts at entryPath and recursively discovers every .hvr file
// reachable via import statements. Each import path is resolved relative
// to the directory of the file that declared it (not the entry file, and
// not the current working directory).
//
// Imports are non-transitive: this function still needs to load every file
// in the chain (a file's content must be read to elaborate it), but the
// per-file Imports table in the result only records what each file itself
// imports — callers (the elaborator) are responsible for not leaking
// transitive visibility.
//
// File-level cycles (a.hvr imports b.hvr imports a.hvr) are detected and
// reported as an error, since a file importing itself transitively is
// always a mistake — there is no valid non-transitive interpretation of it.
func Load(entryPath string) (*LoadResult, error) {
	absEntry, err := filepath.Abs(entryPath)
	if err != nil {
		return nil, fmt.Errorf("could not resolve entry path %q: %w", entryPath, err)
	}
	absEntry = filepath.Clean(absEntry)

	result := &LoadResult{
		EntryPath:   absEntry,
		Sources:     make(map[string]string),
		Programs:    make(map[string]*ast.Program),
		ParseErrors: make(map[string][]string),
		TokenCounts: make(map[string]int),
		Imports:     make(map[string][]ImportEntry),
		LoadOrder:   []string{},
	}

	visiting := make(map[string]int) // currently on the DFS stack, with depth — for cycle detection
	visited := make(map[string]bool) // fully processed — avoids re-reading shared files

	if err := loadFile(absEntry, result, visiting, visited); err != nil {
		return nil, err
	}

	return result, nil
}

// loadFile reads and parses filePath, resolves each of its import statements
// relative to filePath's directory, and recurses into each import that
// hasn't been fully loaded yet.
func loadFile(filePath string, result *LoadResult, visiting map[string]int, visited map[string]bool) error {
	if _, onStack := visiting[filePath]; onStack {
		return fmt.Errorf("import cycle detected: %s", describeCycle(filePath, visiting))
	}
	if visited[filePath] {
		return nil // already loaded via another import path — fine, just skip
	}

	visiting[filePath] = len(visiting)
	defer delete(visiting, filePath)

	contentBytes, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("could not read imported file %q: %w", filePath, err)
	}
	// Editors on Windows (Notepad among them) save UTF-8 with a byte-order
	// mark, which would otherwise lex as stray bytes on line 1.
	content := strings.TrimPrefix(string(contentBytes), "\xEF\xBB\xBF")

	result.Sources[filePath] = content
	result.LoadOrder = append(result.LoadOrder, filePath)

	// Imports are read from the parsed AST, so the loader and the elaborator
	// can never disagree about what a file imports. A file with syntax
	// errors still yields the imports that did parse, and loading carries on
	// so every file's errors are reported together.
	program := parseFile(filePath, content, result)
	fileDir := filepath.Dir(filePath)

	var scanned []*ast.ImportStatement
	for _, stmt := range program.Statements {
		if imp, ok := stmt.(*ast.ImportStatement); ok {
			scanned = append(scanned, imp)
		}
	}

	entries := make([]ImportEntry, 0, len(scanned))
	for _, stmt := range scanned {
		imp := foundImport{
			PathLiteral: stmt.Path,
			Alias:       stmt.Alias,
			Line:        stmt.Line(),
			IsSystem:    stmt.IsSystem,
			Selective:   stmt.Selective,
		}
		for _, n := range stmt.Names {
			imp.Names = append(imp.Names, SelectedSymbol{Name: n.Name, Alias: n.Alias})
		}
		// The old file-based spelling is caught here rather than left to fail
		// as "not a directory". The parser has the same check, but the loader
		// runs first, so this is the one a user actually sees — and naming
		// the exact replacement is what makes the migration mechanical.
		if strings.HasSuffix(imp.PathLiteral, ".hvr") {
			return fmt.Errorf("in %s, line %d: an import names a directory, not a file — "+
				"write %s instead of %s (every .hvr file in that directory is imported together)",
				filePath, imp.Line,
				bracketPath(dirOf(imp.PathLiteral), imp.IsSystem),
				bracketPath(imp.PathLiteral, imp.IsSystem))
		}

		dir := ResolveImportPath(fileDir, imp.PathLiteral, imp.IsSystem)

		// Importing the directory you are already in is not a cycle to
		// report, it is a leftover from the file-based scheme: siblings
		// already share a namespace, so the import is simply unnecessary.
		// Saying so is far more useful than "import cycle detected", which
		// is what the generic path would produce.
		if sameDir(dir, fileDir) {
			return fmt.Errorf("in %s, line %d: %q is this file's own directory — "+
				"files in the same directory already share a namespace, so remove the import",
				filePath, imp.Line, imp.PathLiteral)
		}

		files, err := HvrFilesIn(dir)
		if err != nil {
			if hint := missingPackageHint(imp.PathLiteral, imp.IsSystem); hint != "" {
				return fmt.Errorf("in %s, line %d: %s", filePath, imp.Line, hint)
			}
			return fmt.Errorf("in %s, line %d: could not read imported directory %q (looked in %s): %w",
				filePath, imp.Line, imp.PathLiteral, dir, err)
		}
		if len(files) == 0 {
			return fmt.Errorf("in %s, line %d: %q contains no .hvr files (%s)",
				filePath, imp.Line, imp.PathLiteral, dir)
		}

		// A bad qualifier has to be rejected HERE, before anything is parsed.
		// The default comes from a directory name and a directory can be
		// called anything, so `import "./3d-parts"` would bind `3d_parts` —
		// and the first reference to it fails to lex, producing a syntax
		// error pointing at the use site with no hint that the import is
		// what needs fixing.
		qualifier := QualifierFor(imp.PathLiteral, imp.Alias)
		if !imp.Selective {
			if err := validQualifier(qualifier); err != nil {
				return fmt.Errorf("in %s, line %d: %s would be imported as '%s', which %w — add `as <name>`",
					filePath, imp.Line, bracketPath(imp.PathLiteral, imp.IsSystem), qualifier, err)
			}
		}

		entries = append(entries, ImportEntry{
			Qualifier:   qualifier,
			Alias:       imp.Alias,
			ResolvedDir: dir,
			Files:       files,
			RawPath:     imp.PathLiteral,
			Line:        imp.Line,
			IsSystem:    imp.IsSystem,
			Selective:   imp.Selective,
			Selected:    imp.Names,
			Package:     PackageOf(imp.PathLiteral, imp.IsSystem),
		})

		for _, f := range files {
			if err := loadFile(f, result, visiting, visited); err != nil {
				return fmt.Errorf("in %s, line %d: %w", filePath, imp.Line, err)
			}
		}
	}

	result.Imports[filePath] = entries
	visited[filePath] = true
	return nil
}

// dirOf strips the final segment of an import path, turning the old
// file-based spelling into the directory that replaces it. Always uses '/',
// since import paths are slash-separated regardless of platform.
func dirOf(importPath string) string {
	if i := strings.LastIndexByte(importPath, '/'); i >= 0 {
		return importPath[:i]
	}
	return "."
}

// bracketPath re-renders a path in whichever delimiters the user wrote, so a
// suggested replacement can be pasted verbatim.
func bracketPath(path string, isSystem bool) string {
	if isSystem {
		return "<" + path + ">"
	}
	return "\"" + path + "\""
}

// sameDir compares two directory paths for identity, tolerating the ways the
// same directory can be spelled (trailing separators, "." segments, and — on
// systems where it matters — nothing more, since both sides have already been
// through filepath.Clean).
func sameDir(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

// describeCycle builds a human-readable chain like "a.hvr -> b.hvr -> a.hvr"
// from the files currently on the DFS stack, for a clearer error message than
// just naming the file that closed the loop. Only the loop itself is shown:
// the stack from closingFile's own depth down, in import order.
func describeCycle(closingFile string, visiting map[string]int) string {
	start := visiting[closingFile]
	var loop []string
	for f, depth := range visiting {
		if depth >= start {
			loop = append(loop, f)
		}
	}
	sort.Slice(loop, func(i, j int) bool { return visiting[loop[i]] < visiting[loop[j]] })

	names := make([]string, 0, len(loop)+1)
	for _, f := range loop {
		names = append(names, filepath.Base(f))
	}
	names = append(names, filepath.Base(closingFile))
	return strings.Join(names, " -> ")
}

// foundImport is one import statement as the loader acts on it: the parsed
// ast.ImportStatement's fields, with the selected names in the loader's own
// SelectedSymbol type (what ImportEntry carries to its callers).
type foundImport struct {
	PathLiteral string // the path as written, without its "" or <> delimiters
	Alias       string // "" if no `as Name` clause
	Line        int
	IsSystem    bool // true for `import <...>`, false for `import "..."`
	Selective   bool // the `from <path> import a, b as c;` form
	Names       []SelectedSymbol
}

// parseFile lexes and parses one file's source, recording the program, its
// syntax errors and its token count in result. Every file is parsed exactly
// once, here; main.go reads these results rather than re-parsing.
func parseFile(filePath, content string, result *LoadResult) *ast.Program {
	l := lexer.New(content)
	var tokens []token.Token
	for {
		tok := l.NextToken()
		tokens = append(tokens, tok)
		if tok.Type == token.EOF {
			break
		}
	}
	program, errs := parser.Parse(tokens)
	result.Programs[filePath] = program
	result.ParseErrors[filePath] = errs
	result.TokenCounts[filePath] = len(tokens)
	return program
}
