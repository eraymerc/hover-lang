package loader

import (
	"path/filepath"
	"strings"
)

// resolveImportPath resolves an import's path string relative to the
// directory of the file that contains the import statement.
//
// Example: if currentFileDir is "/proj/foo" and importPath is "./bar/ex2.hvr",
// the result is the cleaned absolute path "/proj/foo/bar/ex2.hvr".
func resolveImportPath(currentFileDir string, importPath string) string {
	joined := filepath.Join(currentFileDir, importPath)
	return filepath.Clean(joined)
}

// scannedImport is a raw import found by scanSourceForImports, before any
// path resolution has happened.
type scannedImport struct {
	PathLiteral string // the string literal contents, e.g. "./bar/ex2.hvr"
	Alias       string // "" if no "as Name" clause
	Line        int    // 1-based line number where the import appears
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

		pathLit, afterPath, ok := extractStringLiteral(rest)
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
