package loader

import (
	"os"
	"path/filepath"
	"strings"
)

// stdlibRoot is the standard library directory, always a "standard_library"
// folder next to the hover executable — not the current working directory,
// and not the source file's directory. This is what makes `import <...>`
// location-independent.
func stdlibRoot() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, e := filepath.EvalSymlinks(exe); e == nil {
		exe = real
	}
	return filepath.Join(filepath.Dir(exe), "standard_library"), nil
}

// resolveImportPath turns an import into an absolute file path.
//
//	import <a/b.hvr>   ->  {binaryDir}/standard_library/a/b.hvr
//	import "./a/b.hvr" ->  {importerDir}/a/b.hvr
//
// stdlibRoot: the standard_library dir shipped next to the hover binary
// (Makefile copies it there), independent of cwd or source location.
func resolveImportPath(currentFileDir, importPath string, isSystem bool) string {
	if isSystem {
		if root, err := stdlibRoot(); err == nil {
			return filepath.Clean(filepath.Join(root, filepath.FromSlash(strings.TrimLeft(importPath, "/"))))
		}
		// fall through to relative if the binary can't be located
	}
	return filepath.Clean(filepath.Join(currentFileDir, importPath))
}

func extractAnglePath(s string) (literal, rest string, ok bool) {
	start := strings.IndexByte(s, '<')
	if start == -1 {
		return "", "", false
	}
	end := strings.IndexByte(s[start+1:], '>')
	if end == -1 {
		return "", "", false
	}
	end = start + 1 + end
	return strings.TrimSpace(s[start+1 : end]), s[end+1:], true
}

// scannedImport is a raw import found by scanSourceForImports, before any
// path resolution has happened.
type scannedImport struct {
	PathLiteral string // the string literal contents, e.g. "./bar/ex2.hvr"
	Alias       string // "" if no "as Name" clause
	Line        int    // 1-based line number where the import appears
	IsSystem    bool   // true for `import <...>` (standard library), false for `import "..."`
}

// scanSourceForImports performs a minimal lexical scan for `import "...";`
// and `import "..." as Name;` statements. It deliberately does NOT use the
// full lexer/parser — the loader runs before any per-file AST exists, and
// import statements need to be found before we know if the rest of the file
// is even syntactically valid yet.
//
// Only top-level, line-oriented matching is needed: Hover requires import
// statements to start with the literal keyword "import" (after optional
// leading whitespace) and to be terminated by a semicolon on the same
// logical statement. This scanner does not need to handle imports split
// across multiple lines for the path itself, since paths are single string
// literals.
func scanSourceForImports(source string) []scannedImport {
	var found []scannedImport

	lines := strings.Split(source, "\n")
	for i, rawLine := range lines {
		line := strings.TrimSpace(rawLine)

		// Skip comments and non-import lines quickly.
		if !strings.HasPrefix(line, "import") {
			continue
		}
		// Ensure "import" is a whole word, not a prefix of an identifier
		// like "importantThing".
		rest := strings.TrimPrefix(line, "import")
		if rest == line { // prefix not actually trimmed — shouldn't happen given HasPrefix
			continue
		}
		if len(rest) > 0 && !isImportSeparator(rest[0]) {
			continue
		}

		trimmed := strings.TrimSpace(rest)
		var pathLit, afterPath string
		var isSystem, ok bool
		if strings.HasPrefix(trimmed, "<") {
			pathLit, afterPath, ok = extractAnglePath(trimmed)
			isSystem = true
		} else {
			pathLit, afterPath, ok = extractStringLiteral(trimmed)
		}
		if !ok {
			continue
		}

		alias := ""
		afterPath = strings.TrimSpace(afterPath)
		if strings.HasPrefix(afterPath, "as") {
			aliasRest := strings.TrimPrefix(afterPath, "as")
			if len(aliasRest) > 0 && isImportSeparator(aliasRest[0]) {
				aliasRest = strings.TrimSpace(aliasRest)
				alias = extractIdentifier(aliasRest)
			}
		}

		found = append(found, scannedImport{
			PathLiteral: pathLit,
			Alias:       alias,
			Line:        i + 1,
			IsSystem:    isSystem,
		})
	}

	return found
}

func isImportSeparator(b byte) bool {
	return b == ' ' || b == '\t'
}

// extractStringLiteral finds the first "..." literal in s and returns its
// contents plus everything after the closing quote.
func extractStringLiteral(s string) (literal string, rest string, ok bool) {
	start := strings.IndexByte(s, '"')
	if start == -1 {
		return "", "", false
	}
	end := strings.IndexByte(s[start+1:], '"')
	if end == -1 {
		return "", "", false
	}
	end = start + 1 + end
	return s[start+1 : end], s[end+1:], true
}

// extractIdentifier reads a leading identifier (letters, digits, underscore)
// from s, stopping at the first non-identifier character (such as ';').
func extractIdentifier(s string) string {
	end := 0
	for end < len(s) {
		c := s[end]
		isLetter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
		isDigit := c >= '0' && c <= '9'
		if !isLetter && !(end > 0 && isDigit) {
			break
		}
		end++
	}
	return s[:end]
}
